package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flemzord/skillloop/internal/config"
	"github.com/flemzord/skillloop/internal/domain"
	"github.com/flemzord/skillloop/internal/improvement"
	"github.com/flemzord/skillloop/internal/store"
)

func TestProcessEligibleDeduplicatesAndSupportsTwoPromotions(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SKILLLOOP_PIPELINE_RUNNER", "1")
	manager, database, skill := newTestManager(t, domain.ModePropose, []string{os.Args[0], "-test.run=^TestPipelineRunnerHelper$"})
	firstCluster := seedCluster(t, database, skill, "correction requires tests", "Run tests before reporting completion.", 1)

	result, err := manager.ProcessEligible(ctx, []domain.Cluster{firstCluster})
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 1 || result.Evaluated != 1 || result.Promoted != 0 || len(result.Failures) != 0 {
		t.Fatalf("first process result = %#v", result)
	}
	result, err = manager.ProcessEligible(ctx, []domain.Cluster{firstCluster})
	if err != nil || result.Created != 0 || result.Evaluated != 0 {
		t.Fatalf("duplicate process result = %#v err=%v", result, err)
	}
	proposals, err := database.ListProposals(ctx, "")
	if err != nil || len(proposals) != 1 || proposals[0].Status != domain.ProposalEvaluated {
		t.Fatalf("first proposals = %#v err=%v", proposals, err)
	}
	view, err := manager.Show(ctx, proposals[0].ID)
	if err != nil || !strings.Contains(view.Diff, "<!-- skillloop:begin ") {
		t.Fatalf("proposal view diff=%q err=%v", view.Diff, err)
	}
	firstPromotion, err := manager.Approve(ctx, proposals[0].ID, "tester")
	if err != nil {
		t.Fatalf("approve first: %v", err)
	}
	idempotentPromotion, err := manager.Approve(ctx, proposals[0].ID, "tester")
	if err != nil || idempotentPromotion.ID != firstPromotion.ID {
		t.Fatalf("idempotent approval=%#v err=%v", idempotentPromotion, err)
	}

	secondCluster := seedCluster(t, database, skill, "artifact verification", "Inspect the generated artifact before completion.", 10)
	t.Setenv("SKILLLOOP_PIPELINE_EXPECTED_MARKERS", "2")
	result, err = manager.ProcessEligible(ctx, []domain.Cluster{secondCluster})
	if err != nil || result.Created != 1 || result.Evaluated != 1 {
		t.Fatalf("second process result = %#v err=%v", result, err)
	}
	proposals, err = database.ListProposals(ctx, domain.ProposalEvaluated)
	if err != nil || len(proposals) != 1 {
		t.Fatalf("second evaluated proposals = %#v err=%v", proposals, err)
	}
	secondProposal := proposals[0]
	if secondProposal.BaseCommit != firstPromotion.PromotedCommit {
		t.Fatalf("second base=%s, want first promotion=%s", secondProposal.BaseCommit, firstPromotion.PromotedCommit)
	}
	secondPromotion, err := manager.Approve(ctx, secondProposal.ID, "tester")
	if err != nil {
		t.Fatalf("approve second: %v", err)
	}
	if secondPromotion.PreviousCommit != firstPromotion.PromotedCommit {
		t.Fatalf("second promotion previous=%s, want %s", secondPromotion.PreviousCommit, firstPromotion.PromotedCommit)
	}

	rollback, err := manager.Rollback(ctx, skill.ID, "tester", "regression")
	if err != nil {
		t.Fatalf("rollback second: %v", err)
	}
	if rollback.ToCommit != firstPromotion.PromotedCommit {
		t.Fatalf("rollback target=%s, want first candidate=%s", rollback.ToCommit, firstPromotion.PromotedCommit)
	}
	current, err := manager.Improver.CurrentRelease(skill)
	if err != nil || current.Commit != firstPromotion.PromotedCommit {
		t.Fatalf("current after rollback=%#v err=%v", current, err)
	}
	rolledProposal, err := database.Proposal(ctx, secondProposal.ID)
	if err != nil || rolledProposal.Status != domain.ProposalRolledBack {
		t.Fatalf("rolled proposal=%#v err=%v", rolledProposal, err)
	}
	audit, err := database.ListAudit(ctx, "promotion", secondPromotion.ID)
	if err != nil || len(audit) != 2 || audit[0].Action != "promotion.rolled_back" || audit[1].Action != "promotion.created" {
		t.Fatalf("promotion audit=%#v err=%v", audit, err)
	}
}

func TestProcessEligibleObserveDoesNothing(t *testing.T) {
	manager, database, skill := newTestManager(t, domain.ModeObserve, nil)
	cluster := seedCluster(t, database, skill, "observe only", "Run all validation steps.", 1)
	result, err := manager.ProcessEligible(context.Background(), []domain.Cluster{cluster})
	if err != nil || result.Created != 0 || result.Evaluated != 0 || result.Promoted != 0 {
		t.Fatalf("observe result=%#v err=%v", result, err)
	}
	proposals, err := database.ListProposals(context.Background(), "")
	if err != nil || len(proposals) != 0 {
		t.Fatalf("observe proposals=%#v err=%v", proposals, err)
	}
	if _, _, err := manager.Create(context.Background(), cluster.ID, "test"); err == nil {
		t.Fatal("direct proposal creation succeeded in observe mode")
	}
}

func TestCurrentObservePolicyBlocksStaleProposalManager(t *testing.T) {
	manager, database, skill := newTestManager(t, domain.ModePropose, nil)
	pendingCluster := seedCluster(t, database, skill, "pending before observe", "Validate before changing mode.", 1)
	pending, created, err := manager.Create(context.Background(), pendingCluster.ID, "test")
	if err != nil || !created {
		t.Fatalf("create pending proposal: created=%v err=%v", created, err)
	}
	blockedCluster := seedCluster(t, database, skill, "blocked after observe", "Do not mutate in observe mode.", 10)

	settings := manager.Config
	settings.Mode = domain.ModeObserve
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := config.WriteInitial(configPath, settings); err != nil {
		t.Fatalf("write observe config: %v", err)
	}
	var loads atomic.Int32
	manager.ConfigLoader = func() (config.Config, error) {
		loads.Add(1)
		return config.Load(configPath)
	}
	manager.PolicyLocker = func(ctx context.Context) (func() error, error) {
		return config.AcquirePolicyReadLock(ctx, configPath)
	}

	if _, _, err := manager.Evaluate(context.Background(), pending.ID, "test"); err == nil || !strings.Contains(err.Error(), "observe mode") {
		t.Fatalf("stale manager evaluation error=%v, want observe refusal", err)
	}
	if _, _, err := manager.Create(context.Background(), blockedCluster.ID, "test"); err == nil || !strings.Contains(err.Error(), "observe mode") {
		t.Fatalf("stale manager creation error=%v, want observe refusal", err)
	}
	result, err := manager.ProcessEligible(context.Background(), []domain.Cluster{blockedCluster})
	if err != nil || result.Created != 0 || result.Evaluated != 0 || result.Promoted != 0 {
		t.Fatalf("observe process result=%#v err=%v", result, err)
	}
	if loads.Load() < 3 {
		t.Fatalf("current config loads=%d, want create, evaluate, and process reloads", loads.Load())
	}
	stored, err := database.Proposal(context.Background(), pending.ID)
	if err != nil || stored.Status != domain.ProposalPending {
		t.Fatalf("pending proposal mutated=%#v err=%v", stored, err)
	}
	if _, err := database.ProposalForCluster(context.Background(), blockedCluster.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("observe policy created blocked proposal: %v", err)
	}
}

func TestEvaluateSerializesObserveModeChange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	t.Setenv("SKILLLOOP_PIPELINE_RUNNER", "1")
	manager, database, skill := newTestManager(t, domain.ModePropose, []string{os.Args[0], "-test.run=^TestPipelineRunnerHelper$"})
	firstCluster := seedClusterKind(t, database, skill, domain.CardValidation, "serialized evaluation", "go test ./...", 1)
	first, created, err := manager.Create(ctx, firstCluster.ID, "test")
	if err != nil || !created {
		t.Fatalf("create first proposal: created=%v err=%v", created, err)
	}
	secondCluster := seedClusterKind(t, database, skill, domain.CardValidation, "blocked next evaluation", "go test ./...", 10)
	second, created, err := manager.Create(ctx, secondCluster.ID, "test")
	if err != nil || !created {
		t.Fatalf("create second proposal: created=%v err=%v", created, err)
	}
	blockedCluster := seedClusterKind(t, database, skill, domain.CardValidation, "blocked next creation", "go test ./...", 20)

	settings := manager.Config
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := config.WriteInitial(configPath, settings); err != nil {
		t.Fatalf("write propose config: %v", err)
	}
	manager.ConfigLoader = func() (config.Config, error) { return config.Load(configPath) }
	manager.PolicyLocker = func(lockCtx context.Context) (func() error, error) {
		return config.AcquirePolicyReadLock(lockCtx, configPath)
	}

	readyPath := filepath.Join(t.TempDir(), "evaluation-ready")
	continuePath := filepath.Join(t.TempDir(), "evaluation-continue")
	t.Setenv("SKILLLOOP_PIPELINE_POLICY_READY", readyPath)
	t.Setenv("SKILLLOOP_PIPELINE_POLICY_CONTINUE", continuePath)
	type evaluationOutcome struct {
		proposal   domain.Proposal
		evaluation improvement.Evaluation
		err        error
	}
	evaluated := make(chan evaluationOutcome, 1)
	go func() {
		proposal, evaluation, evaluationErr := manager.Evaluate(ctx, first.ID, "test")
		evaluated <- evaluationOutcome{proposal: proposal, evaluation: evaluation, err: evaluationErr}
	}()
	waitForPipelineFile(t, ctx, readyPath)

	settings.Mode = domain.ModeObserve
	saveDone := make(chan error, 1)
	go func() {
		_, saveErr := config.Save(configPath, settings)
		saveDone <- saveErr
	}()
	select {
	case saveErr := <-saveDone:
		t.Fatalf("observe mode bypassed running evaluator lock: %v", saveErr)
	case <-time.After(100 * time.Millisecond):
	}
	if err := os.WriteFile(continuePath, []byte("continue"), 0o600); err != nil {
		t.Fatalf("release evaluator: %v", err)
	}
	if saveErr := <-saveDone; saveErr != nil {
		t.Fatalf("save observe mode: %v", saveErr)
	}
	outcome := <-evaluated
	if outcome.err != nil || outcome.proposal.Status != domain.ProposalEvaluated || !outcome.evaluation.Passed {
		t.Fatalf("serialized evaluation=%#v err=%v", outcome, outcome.err)
	}

	nextReadyPath := filepath.Join(t.TempDir(), "next-evaluation-ready")
	t.Setenv("SKILLLOOP_PIPELINE_POLICY_READY", nextReadyPath)
	if _, _, err := manager.Evaluate(ctx, second.ID, "test"); err == nil || !strings.Contains(err.Error(), "observe mode") {
		t.Fatalf("next evaluation error=%v, want observe refusal", err)
	}
	if _, _, err := manager.Create(ctx, blockedCluster.ID, "test"); err == nil || !strings.Contains(err.Error(), "observe mode") {
		t.Fatalf("next creation error=%v, want observe refusal", err)
	}
	if _, err := os.Stat(nextReadyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("evaluator launched after observe mode was saved: %v", err)
	}
	stored, err := database.Proposal(ctx, second.ID)
	if err != nil || stored.Status != domain.ProposalPending {
		t.Fatalf("next proposal mutated=%#v err=%v", stored, err)
	}
	if _, err := database.ProposalForCluster(ctx, blockedCluster.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("proposal created after observe mode was saved: %v", err)
	}
}

func TestConcurrentCreateReservesOnlyOneProposal(t *testing.T) {
	manager, database, skill := newTestManager(t, domain.ModePropose, nil)
	cluster := seedCluster(t, database, skill, "concurrent correction", "Run validation before completion.", 1)
	var created atomic.Int32
	var failures atomic.Int32
	var wait sync.WaitGroup
	for range 2 {
		wait.Go(func() {
			_, wasCreated, err := manager.Create(context.Background(), cluster.ID, "test")
			if err != nil {
				failures.Add(1)
				return
			}
			if wasCreated {
				created.Add(1)
			}
		})
	}
	wait.Wait()
	if failures.Load() != 0 || created.Load() != 1 {
		t.Fatalf("failures=%d created=%d", failures.Load(), created.Load())
	}
	proposals, err := database.ListProposals(context.Background(), "")
	if err != nil || len(proposals) != 1 {
		t.Fatalf("proposals=%#v err=%v", proposals, err)
	}
}

func TestAutopilotKeepsCorrectionForExplicitApprovalWithoutKeywordMarkers(t *testing.T) {
	t.Setenv("SKILLLOOP_PIPELINE_RUNNER", "1")
	command := []string{os.Args[0], "-test.run=^TestPipelineRunnerHelper$"}
	manager, database, skill := newTestManager(t, domain.ModeAutopilot, command)
	cluster := seedCluster(t, database, skill, "response style", "Prefer concise answers when the user asks a short question.", 1)
	result, err := manager.ProcessEligible(context.Background(), []domain.Cluster{cluster})
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 1 || result.Evaluated != 1 || result.Promoted != 0 || len(result.Failures) != 0 {
		t.Fatalf("risky autopilot result=%#v", result)
	}
	proposals, err := database.ListProposals(context.Background(), domain.ProposalEvaluated)
	if err != nil || len(proposals) != 1 {
		t.Fatalf("evaluated proposals=%#v err=%v", proposals, err)
	}
	if _, err := manager.Approve(context.Background(), proposals[0].ID, "human"); err != nil {
		t.Fatalf("explicit approval: %v", err)
	}
}

func TestApprovePendingProposalFails(t *testing.T) {
	manager, database, skill := newTestManager(t, domain.ModePropose, nil)
	cluster := seedCluster(t, database, skill, "pending approval", "Validate the output first.", 1)
	proposal, created, err := manager.Create(context.Background(), cluster.ID, "test")
	if err != nil || !created {
		t.Fatalf("create pending: created=%v err=%v", created, err)
	}
	_, err = manager.Approve(context.Background(), proposal.ID, "test")
	if !errors.Is(err, improvement.ErrEvaluationFailed) {
		t.Fatalf("approve pending error=%v, want evaluation failure", err)
	}
}

func TestApproveRequiresPersistedExternalComparativeProof(t *testing.T) {
	manager, database, skill := newTestManager(t, domain.ModePropose, nil)
	cluster := seedCluster(t, database, skill, "structural only", "Validate the exact output first.", 1)
	proposal, created, err := manager.Create(context.Background(), cluster.ID, "test")
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	proposal, evaluation, err := manager.Evaluate(context.Background(), proposal.ID, "test")
	if err != nil || !evaluation.Passed || evaluation.BaselineRun != nil || evaluation.CandidateRun != nil {
		t.Fatalf("structural evaluation=%#v err=%v", evaluation, err)
	}
	_, err = manager.Approve(context.Background(), proposal.ID, "human")
	if !errors.Is(err, improvement.ErrExternalEvaluationRequired) {
		t.Fatalf("approve error=%v, want ErrExternalEvaluationRequired", err)
	}
	if !strings.Contains(err.Error(), "promotion requires comparative proof") {
		t.Fatalf("approve error is not explicit: %v", err)
	}
	stored, err := database.Proposal(context.Background(), proposal.ID)
	if err != nil || stored.Status != domain.ProposalEvaluated {
		t.Fatalf("proposal mutated after refused approval=%#v err=%v", stored, err)
	}
}

func TestHumanApprovalRequiresCurrentEvaluationPolicy(t *testing.T) {
	t.Setenv("SKILLLOOP_PIPELINE_RUNNER", "1")
	manager, database, skill := newTestManager(t, domain.ModePropose, []string{os.Args[0], "-test.run=^TestPipelineRunnerHelper$"})
	cluster := seedClusterKind(t, database, skill, domain.CardValidation, "current human proof", "go test ./...", 1)
	proposal, created, err := manager.Create(context.Background(), cluster.ID, "test")
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	proposal, _, err = manager.Evaluate(context.Background(), proposal.ID, "test")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	settings := manager.Config
	settings.Evaluation.MinimumImprovement = 2
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := config.WriteInitial(configPath, settings); err != nil {
		t.Fatalf("write stricter policy: %v", err)
	}
	manager.ConfigLoader = func() (config.Config, error) { return config.Load(configPath) }
	manager.PolicyLocker = func(ctx context.Context) (func() error, error) {
		return config.AcquirePolicyReadLock(ctx, configPath)
	}

	if _, err := manager.Approve(context.Background(), proposal.ID, "human"); err == nil || !strings.Contains(err.Error(), "evaluation policy changed") {
		t.Fatalf("approval error=%v, want stale proof refusal", err)
	}
	stored, err := database.Proposal(context.Background(), proposal.ID)
	if err != nil || stored.Status != domain.ProposalEvaluated {
		t.Fatalf("proposal mutated after stale proof=%#v err=%v", stored, err)
	}
	if _, err := database.ActivePromotion(context.Background(), skill.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale proof produced active promotion: %v", err)
	}
}

func TestApproveRefusesObserveMode(t *testing.T) {
	manager, database, skill := newTestManager(t, domain.ModePropose, nil)
	cluster := seedCluster(t, database, skill, "observe approval", "Validate the output first.", 1)
	proposal, created, err := manager.Create(context.Background(), cluster.ID, "test")
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	manager.Config.Mode = domain.ModeObserve
	if _, _, err := manager.Evaluate(context.Background(), proposal.ID, "test"); err == nil {
		t.Fatal("evaluation succeeded in observe mode")
	}
	manager.Config.Mode = domain.ModePropose
	proposal, _, err = manager.Evaluate(context.Background(), proposal.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	manager.Config.Mode = domain.ModeObserve
	if _, err := manager.Approve(context.Background(), proposal.ID, "test"); err == nil {
		t.Fatal("approval succeeded in observe mode")
	}
}

func TestProcessEligibleResumesPendingProposal(t *testing.T) {
	manager, database, skill := newTestManager(t, domain.ModePropose, nil)
	cluster := seedCluster(t, database, skill, "resume pending", "Validate the exact output.", 1)
	proposal, created, err := manager.Create(context.Background(), cluster.ID, "test")
	if err != nil || !created || proposal.CandidateCommit == "" {
		t.Fatalf("create pending: proposal=%#v created=%v err=%v", proposal, created, err)
	}
	result, err := manager.ProcessEligible(context.Background(), nil)
	if err != nil || result.Evaluated != 1 || len(result.Failures) != 0 {
		t.Fatalf("resume result=%#v err=%v", result, err)
	}
	stored, err := database.Proposal(context.Background(), proposal.ID)
	if err != nil || stored.Status != domain.ProposalEvaluated {
		t.Fatalf("resumed proposal=%#v err=%v", stored, err)
	}
}

func TestProcessEligibleRecreatesEmptyReservation(t *testing.T) {
	manager, database, skill := newTestManager(t, domain.ModePropose, nil)
	cluster := seedCluster(t, database, skill, "resume empty reservation", "Validate the exact output.", 1)
	reserved := domain.Proposal{
		ID: stableID("proposal", cluster.ID), ClusterID: cluster.ID, SkillID: skill.ID,
		Status: domain.ProposalPending, RepositoryPath: skill.RepositoryPath,
		CreatedAt: time.Now().Add(-10 * time.Minute),
	}
	created, err := database.ReserveProposal(context.Background(), reserved)
	if err != nil || !created {
		t.Fatalf("reserve empty proposal: created=%v err=%v", created, err)
	}
	reserved.UpdatedAt = reserved.CreatedAt
	if err := database.SaveProposal(context.Background(), reserved); err != nil {
		t.Fatalf("age empty reservation: %v", err)
	}
	result, err := manager.ProcessEligible(context.Background(), nil)
	if err != nil || result.Created != 1 || result.Evaluated != 1 || len(result.Failures) != 0 {
		t.Fatalf("recreate result=%#v err=%v", result, err)
	}
	stored, err := database.Proposal(context.Background(), reserved.ID)
	if err != nil || stored.Status != domain.ProposalEvaluated || stored.CandidateCommit == "" {
		t.Fatalf("recreated proposal=%#v err=%v", stored, err)
	}
}

func TestAutopilotResumesApprovedProposal(t *testing.T) {
	t.Setenv("SKILLLOOP_PIPELINE_RUNNER", "1")
	manager, database, skill := newTestManager(t, domain.ModeAutopilot, []string{os.Args[0], "-test.run=^TestPipelineRunnerHelper$"})
	cluster := seedClusterKind(t, database, skill, domain.CardValidation, "resume approved", "go test ./...", 1)
	proposal, created, err := manager.Create(context.Background(), cluster.ID, "test")
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	proposal, _, err = manager.Evaluate(context.Background(), proposal.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ApproveProposal(context.Background(), proposal.ID, proposal.BaseCommit, proposal.CandidateCommit, "autopilot", "{}"); err != nil {
		t.Fatal(err)
	}
	result, err := manager.ProcessEligible(context.Background(), nil)
	if err != nil || result.Promoted != 1 || len(result.Failures) != 0 {
		t.Fatalf("approved recovery result=%#v err=%v", result, err)
	}
	active, err := database.ActivePromotion(context.Background(), skill.ID)
	if err != nil || active.ProposalID != proposal.ID {
		t.Fatalf("active promotion=%#v err=%v", active, err)
	}
}

func TestAutopilotRestartRequiresPersistedContainmentProof(t *testing.T) {
	t.Setenv("SKILLLOOP_PIPELINE_RUNNER", "1")
	manager, database, skill := newTestManager(t, domain.ModeAutopilot, []string{os.Args[0], "-test.run=^TestPipelineRunnerHelper$"})
	cluster := seedClusterKind(t, database, skill, domain.CardValidation, "persisted containment", "go test ./...", 1)
	proposal, created, err := manager.Create(context.Background(), cluster.ID, "test")
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	proposal, evaluation, err := manager.Evaluate(context.Background(), proposal.ID, "test")
	if err != nil || !evaluation.Passed {
		t.Fatalf("evaluate: evaluation=%#v err=%v", evaluation, err)
	}

	persisted, err := manager.persistedEvaluation(context.Background(), proposal)
	if err != nil {
		t.Fatalf("restore legitimate persisted evaluation: %v", err)
	}
	if persisted.BaselineRun == nil || persisted.CandidateRun == nil ||
		!persisted.BaselineRun.ContainmentVerified || !persisted.CandidateRun.ContainmentVerified {
		t.Fatalf("persisted containment proof was lost: %#v", persisted)
	}
	if err := improvement.ValidatePromotionProof(persisted); err != nil {
		t.Fatalf("legitimate persisted proof rejected: %v", err)
	}

	evaluation.BaselineRun.ContainmentVerified = false
	evaluation.CandidateRun.ContainmentVerified = false
	details := evaluationDetails(evaluation, manager.Config)
	results, err := database.ListEvaluationResults(context.Background(), proposal.ID)
	if err != nil || len(results) != 2 {
		t.Fatalf("evaluation results=%#v err=%v", results, err)
	}
	for index := range results {
		results[index].Details = details
	}
	if err := database.CompleteEvaluation(context.Background(), proposal, results...); err != nil {
		t.Fatalf("persist uncontained evaluation: %v", err)
	}

	restarted := New(manager.Config, database)
	result, err := restarted.ProcessEligible(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Promoted != 0 || len(result.Failures) != 1 || !strings.Contains(result.Failures[0].Error, "containment was not verified") {
		t.Fatalf("uncontained persisted proof result=%#v", result)
	}
	if _, err := database.ActivePromotion(context.Background(), skill.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("uncontained persisted proof was autopromoted: %v", err)
	}
}

func TestAutopilotRejectsPersistedProofAfterEvaluatorPolicyChange(t *testing.T) {
	t.Setenv("SKILLLOOP_PIPELINE_RUNNER", "1")
	command := []string{os.Args[0], "-test.run=^TestPipelineRunnerHelper$"}
	manager, database, skill := newTestManager(t, domain.ModeAutopilot, command)
	cluster := seedClusterKind(t, database, skill, domain.CardValidation, "stale evaluator proof", "go test ./...", 1)
	proposal, created, err := manager.Create(context.Background(), cluster.ID, "test")
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	proposal, _, err = manager.Evaluate(context.Background(), proposal.ID, "test")
	if err != nil {
		t.Fatal(err)
	}

	manager.Config.Evaluation.Command = append(append([]string(nil), command...), "-test.v")
	result, err := manager.ProcessEligible(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Promoted != 0 || len(result.Failures) != 1 || !strings.Contains(result.Failures[0].Error, "evaluation policy changed") {
		t.Fatalf("stale proof result=%#v", result)
	}
	if _, err := database.ActivePromotion(context.Background(), skill.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale proof was autopromoted: %v", err)
	}
	stored, err := database.Proposal(context.Background(), proposal.ID)
	if err != nil || stored.Status != domain.ProposalEvaluated {
		t.Fatalf("proposal mutated after stale proof=%#v err=%v", stored, err)
	}
}

func TestAutopilotRestartPreservesPersistedHumanApproval(t *testing.T) {
	t.Setenv("SKILLLOOP_PIPELINE_RUNNER", "1")
	manager, database, skill := newTestManager(t, domain.ModeAutopilot, []string{os.Args[0], "-test.run=^TestPipelineRunnerHelper$"})
	cluster := seedClusterKind(t, database, skill, domain.CardValidation, "persisted human review", "go test ./...", 1)
	proposal, created, err := manager.Create(context.Background(), cluster.ID, "test")
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	proposal, evaluation, err := manager.Evaluate(context.Background(), proposal.ID, "test")
	if err != nil {
		t.Fatalf("initial evaluation=%#v err=%v", evaluation, err)
	}
	evaluation.RequiresHumanApproval = true
	details := evaluationDetails(evaluation, manager.Config)
	results, err := database.ListEvaluationResults(context.Background(), proposal.ID)
	if err != nil || len(results) != 2 {
		t.Fatalf("evaluation results=%#v err=%v", results, err)
	}
	for index := range results {
		results[index].Details = details
	}
	if err := database.CompleteEvaluation(context.Background(), proposal, results...); err != nil {
		t.Fatalf("persist human-only evaluation: %v", err)
	}

	restarted := New(manager.Config, database)
	view, err := restarted.Show(context.Background(), proposal.ID)
	if err != nil || !view.RequiresHumanApproval {
		t.Fatalf("restarted view requires_human_approval=%v err=%v", view.RequiresHumanApproval, err)
	}
	result, err := restarted.ProcessEligible(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Promoted != 0 || len(result.Failures) != 0 {
		t.Fatalf("human-only restart result=%#v", result)
	}
	if _, err := database.ActivePromotion(context.Background(), skill.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("human-only evaluation was autopromoted: %v", err)
	}
	stored, err := database.Proposal(context.Background(), proposal.ID)
	if err != nil || stored.Status != domain.ProposalEvaluated {
		t.Fatalf("proposal mutated after restart=%#v err=%v", stored, err)
	}
}

func TestAutopilotPromotionSerializesModeChanges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	t.Setenv("SKILLLOOP_PIPELINE_RUNNER", "1")
	manager, database, skill := newTestManager(t, domain.ModeAutopilot, []string{os.Args[0], "-test.run=^TestPipelineRunnerHelper$"})
	cluster := seedClusterKind(t, database, skill, domain.CardValidation, "serialized policy", "go test ./...", 1)
	proposal, created, err := manager.Create(ctx, cluster.ID, "test")
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	proposal, evaluation, err := manager.Evaluate(ctx, proposal.ID, "test")
	if err != nil || !evaluation.Passed {
		t.Fatalf("evaluate: evaluation=%#v err=%v", evaluation, err)
	}
	// The test targets policy serialization independently of platform-specific
	// evaluator containment, which may itself require human review.
	evaluation.RequiresHumanApproval = false

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	settings := manager.Config
	if _, err := config.WriteInitial(configPath, settings); err != nil {
		t.Fatalf("write config: %v", err)
	}
	manager.ConfigLoader = func() (config.Config, error) { return config.Load(configPath) }
	lockAcquired := make(chan struct{})
	allowPromotion := make(chan struct{})
	manager.PolicyLocker = func(ctx context.Context) (func() error, error) {
		unlock, err := config.AcquirePolicyReadLock(ctx, configPath)
		if err != nil {
			return nil, err
		}
		close(lockAcquired)
		select {
		case <-allowPromotion:
			return unlock, nil
		case <-ctx.Done():
			_ = unlock()
			return nil, ctx.Err()
		}
	}

	resultDone := make(chan ProcessResult, 1)
	go func() {
		result := ProcessResult{}
		manager.autopilot(ctx, proposal, evaluation, &result)
		resultDone <- result
	}()
	<-lockAcquired
	settings.Mode = domain.ModeObserve
	saveDone := make(chan error, 1)
	go func() {
		_, err := config.Save(configPath, settings)
		saveDone <- err
	}()
	select {
	case err := <-saveDone:
		t.Fatalf("mode change bypassed active promotion lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(allowPromotion)
	result := <-resultDone
	if result.Promoted != 1 || len(result.Failures) != 0 {
		t.Fatalf("serialized promotion result=%#v", result)
	}
	if err := <-saveDone; err != nil {
		t.Fatalf("save observe mode after promotion: %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil || loaded.Mode != domain.ModeObserve {
		t.Fatalf("loaded policy=%#v err=%v", loaded, err)
	}
	active, err := database.ActivePromotion(ctx, skill.ID)
	if err != nil || active.ProposalID != proposal.ID {
		t.Fatalf("active promotion=%#v err=%v", active, err)
	}
}

func TestHumanApprovalSerializesModeChanges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	t.Setenv("SKILLLOOP_PIPELINE_RUNNER", "1")
	manager, database, skill := newTestManager(t, domain.ModePropose, []string{os.Args[0], "-test.run=^TestPipelineRunnerHelper$"})
	cluster := seedClusterKind(t, database, skill, domain.CardValidation, "serialized human approval", "go test ./...", 1)
	proposal, created, err := manager.Create(ctx, cluster.ID, "test")
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	proposal, _, err = manager.Evaluate(ctx, proposal.ID, "test")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	settings := manager.Config
	if _, err := config.WriteInitial(configPath, settings); err != nil {
		t.Fatalf("write config: %v", err)
	}
	manager.ConfigLoader = func() (config.Config, error) { return config.Load(configPath) }
	lockAcquired := make(chan struct{})
	allowApproval := make(chan struct{})
	manager.PolicyLocker = func(ctx context.Context) (func() error, error) {
		unlock, err := config.AcquirePolicyReadLock(ctx, configPath)
		if err != nil {
			return nil, err
		}
		close(lockAcquired)
		select {
		case <-allowApproval:
			return unlock, nil
		case <-ctx.Done():
			_ = unlock()
			return nil, ctx.Err()
		}
	}
	type approvalOutcome struct {
		promotion domain.Promotion
		err       error
	}
	approved := make(chan approvalOutcome, 1)
	go func() {
		promotion, err := manager.Approve(ctx, proposal.ID, "human")
		approved <- approvalOutcome{promotion: promotion, err: err}
	}()
	<-lockAcquired
	settings.Mode = domain.ModeObserve
	saveDone := make(chan error, 1)
	go func() {
		_, err := config.Save(configPath, settings)
		saveDone <- err
	}()
	select {
	case err := <-saveDone:
		t.Fatalf("mode change bypassed active approval lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(allowApproval)
	outcome := <-approved
	if outcome.err != nil || outcome.promotion.ProposalID != proposal.ID {
		t.Fatalf("approval=%#v err=%v", outcome.promotion, outcome.err)
	}
	if err := <-saveDone; err != nil {
		t.Fatalf("save observe mode after approval: %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil || loaded.Mode != domain.ModeObserve {
		t.Fatalf("loaded policy=%#v err=%v", loaded, err)
	}
	active, err := database.ActivePromotion(ctx, skill.ID)
	if err != nil || active.ID != outcome.promotion.ID {
		t.Fatalf("active promotion=%#v err=%v", active, err)
	}
}

func TestAutopilotNeverResumesApprovedCorrection(t *testing.T) {
	t.Setenv("SKILLLOOP_PIPELINE_RUNNER", "1")
	manager, database, skill := newTestManager(t, domain.ModeAutopilot, []string{os.Args[0], "-test.run=^TestPipelineRunnerHelper$"})
	cluster := seedCluster(t, database, skill, "approved correction", "Prefer concise output for short questions.", 1)
	proposal, created, err := manager.Create(context.Background(), cluster.ID, "test")
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	proposal, _, err = manager.Evaluate(context.Background(), proposal.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ApproveProposal(context.Background(), proposal.ID, proposal.BaseCommit, proposal.CandidateCommit, "human", "{}"); err != nil {
		t.Fatal(err)
	}
	result, err := manager.ProcessEligible(context.Background(), nil)
	if err != nil || result.Promoted != 0 || len(result.Failures) != 0 {
		t.Fatalf("approved correction recovery result=%#v err=%v", result, err)
	}
	if _, err := database.ActivePromotion(context.Background(), skill.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("correction was autopromoted: %v", err)
	}
}

func TestApproveCompensatesFilesystemWhenPromotionPersistenceFails(t *testing.T) {
	t.Setenv("SKILLLOOP_PIPELINE_RUNNER", "1")
	manager, database, skill := newTestManager(t, domain.ModePropose, []string{os.Args[0], "-test.run=^TestPipelineRunnerHelper$"})
	cluster := seedClusterKind(t, database, skill, domain.CardValidation, "compensated promotion", "go test ./...", 1)
	result, err := manager.ProcessEligible(context.Background(), []domain.Cluster{cluster})
	if err != nil || result.Evaluated != 1 || len(result.Failures) != 0 {
		t.Fatalf("prepare proposal result=%#v err=%v", result, err)
	}
	proposals, err := database.ListProposals(context.Background(), domain.ProposalEvaluated)
	if err != nil || len(proposals) != 1 {
		t.Fatalf("evaluated proposals=%#v err=%v", proposals, err)
	}
	proposal := proposals[0]

	faultDB, err := sql.Open("sqlite", filepath.Join(manager.Config.DataDir, "skillloop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = faultDB.Close() }()
	if _, err := faultDB.Exec(`
		CREATE TRIGGER fail_promotion_persistence
		BEFORE INSERT ON promotions
		BEGIN SELECT RAISE(ABORT, 'injected promotion persistence failure'); END;
	`); err != nil {
		t.Fatalf("install fault trigger: %v", err)
	}

	if _, err := manager.Approve(context.Background(), proposal.ID, "tester"); err == nil ||
		!strings.Contains(err.Error(), "filesystem promotion rolled back") {
		t.Fatalf("approve error=%v, want compensated persistence failure", err)
	}
	current, err := manager.Improver.CurrentRelease(skill)
	if err != nil || current.Commit != proposal.BaseCommit {
		t.Fatalf("current after compensation=%#v err=%v, want baseline %s", current, err, proposal.BaseCommit)
	}
	if _, err := database.ActivePromotion(context.Background(), skill.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("durable promotion exists after failed transaction: %v", err)
	}
	stored, err := database.Proposal(context.Background(), proposal.ID)
	if err != nil || stored.Status != domain.ProposalApproved {
		t.Fatalf("proposal after compensation=%#v err=%v", stored, err)
	}

	if _, err := faultDB.Exec(`DROP TRIGGER fail_promotion_persistence`); err != nil {
		t.Fatalf("remove fault trigger: %v", err)
	}
	promotion, err := manager.Approve(context.Background(), proposal.ID, "tester")
	if err != nil {
		t.Fatalf("retry compensated promotion: %v", err)
	}
	current, err = manager.Improver.CurrentRelease(skill)
	if err != nil || current.Commit != promotion.PromotedCommit {
		t.Fatalf("current after retry=%#v promotion=%#v err=%v", current, promotion, err)
	}
}

func TestPromotionAndRollbackShareExactSkillFence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	t.Setenv("SKILLLOOP_PIPELINE_RUNNER", "1")
	manager, database, skill := newTestManager(t, domain.ModePropose, []string{os.Args[0], "-test.run=^TestPipelineRunnerHelper$"})
	firstCluster := seedClusterKind(t, database, skill, domain.CardValidation, "first fenced release", "go test ./...", 1)
	first, created, err := manager.Create(ctx, firstCluster.ID, "test")
	if err != nil || !created {
		t.Fatalf("create first: created=%v err=%v", created, err)
	}
	first, _, err = manager.Evaluate(ctx, first.ID, "test")
	if err != nil {
		t.Fatalf("evaluate first: %v", err)
	}
	firstPromotion, err := manager.Approve(ctx, first.ID, "test")
	if err != nil {
		t.Fatalf("approve first: %v", err)
	}

	secondCluster := seedClusterKind(t, database, skill, domain.CardValidation, "second fenced release", "go test ./...", 10)
	t.Setenv("SKILLLOOP_PIPELINE_EXPECTED_MARKERS", "2")
	second, created, err := manager.Create(ctx, secondCluster.ID, "test")
	if err != nil || !created {
		t.Fatalf("create second: created=%v err=%v", created, err)
	}
	second, _, err = manager.Evaluate(ctx, second.ID, "test")
	if err != nil {
		t.Fatalf("evaluate second: %v", err)
	}

	firstLocked := make(chan struct{})
	allowRollback := make(chan struct{})
	secondWaiting := make(chan struct{})
	var lockCalls atomic.Int32
	manager.SkillLocker = func(lockCtx context.Context, skillID string) (func() error, error) {
		call := lockCalls.Add(1)
		if call == 2 {
			close(secondWaiting)
		}
		release, lockErr := manager.Improver.AcquireSkillFence(lockCtx, skillID)
		if lockErr != nil {
			return nil, lockErr
		}
		if call == 1 {
			close(firstLocked)
			select {
			case <-allowRollback:
			case <-lockCtx.Done():
				_ = release()
				return nil, lockCtx.Err()
			}
		}
		return release, nil
	}

	type rollbackOutcome struct {
		rollback domain.Rollback
		err      error
	}
	rolledBack := make(chan rollbackOutcome, 1)
	go func() {
		rollback, rollbackErr := manager.Rollback(ctx, skill.ID, "human", "fenced rollback")
		rolledBack <- rollbackOutcome{rollback: rollback, err: rollbackErr}
	}()
	<-firstLocked

	type approvalOutcome struct {
		promotion domain.Promotion
		err       error
	}
	approved := make(chan approvalOutcome, 1)
	go func() {
		promotion, approvalErr := manager.Approve(ctx, second.ID, "human")
		approved <- approvalOutcome{promotion: promotion, err: approvalErr}
	}()
	<-secondWaiting
	select {
	case outcome := <-approved:
		t.Fatalf("promotion bypassed held rollback fence: %#v", outcome)
	case <-time.After(100 * time.Millisecond):
	}
	close(allowRollback)

	rollbackResult := <-rolledBack
	if rollbackResult.err != nil || rollbackResult.rollback.PromotionID != firstPromotion.ID {
		t.Fatalf("rollback=%#v err=%v", rollbackResult.rollback, rollbackResult.err)
	}
	approvalResult := <-approved
	if !errors.Is(approvalResult.err, improvement.ErrDrift) {
		t.Fatalf("concurrent approval=%#v err=%v, want release drift", approvalResult.promotion, approvalResult.err)
	}
	if _, err := database.ActivePromotion(ctx, skill.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("durable release remained active after fenced rollback: %v", err)
	}
	current, err := manager.Improver.CurrentRelease(skill)
	if err != nil || current.Commit != firstPromotion.PreviousCommit {
		t.Fatalf("filesystem release=%#v err=%v, want %s", current, err, firstPromotion.PreviousCommit)
	}
}

func TestPipelineRunnerHelper(t *testing.T) {
	if os.Getenv("SKILLLOOP_PIPELINE_RUNNER") != "1" {
		return
	}
	contents, err := os.ReadFile(filepath.Join("skills", "demo", "SKILL.md"))
	if err != nil {
		os.Exit(2)
	}
	expectedMarkers := 1
	if raw := os.Getenv("SKILLLOOP_PIPELINE_EXPECTED_MARKERS"); raw != "" {
		expectedMarkers, err = strconv.Atoi(raw)
		if err != nil {
			os.Exit(2)
		}
	}
	if strings.Count(string(contents), "<!-- skillloop:begin ") < expectedMarkers {
		os.Exit(1)
	}
	if readyPath := os.Getenv("SKILLLOOP_PIPELINE_POLICY_READY"); readyPath != "" {
		if err := os.WriteFile(readyPath, []byte("ready"), 0o600); err != nil {
			os.Exit(2)
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(os.Getenv("SKILLLOOP_PIPELINE_POLICY_CONTINUE")); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		os.Exit(2)
	}
}

func waitForPipelineFile(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for pipeline evaluator: %v", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func newTestManager(t *testing.T, mode domain.AutonomyMode, command []string) (Manager, *store.Store, domain.Skill) {
	t.Helper()
	dataDir := t.TempDir()
	t.Cleanup(func() { makeWritable(dataDir) })
	database, err := store.Open(context.Background(), filepath.Join(dataDir, "skillloop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(filepath.Join(repository, "skills", "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "init", "-b", "main")
	runGit(t, repository, "config", "user.name", "Test User")
	runGit(t, repository, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "skills", "demo", "SKILL.md"), []byte("---\nname: demo\ndescription: Demo skill\n---\n\n# Demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", ".")
	runGit(t, repository, "commit", "-m", "feat: add skill")
	skill := domain.Skill{
		ID: "demo-skill", Name: "Demo", RepositoryPath: repository,
		InstructionPath: "skills/demo/SKILL.md", Enabled: true, CreatedAt: time.Now().UTC(),
	}
	if _, err := database.RegisterSkill(context.Background(), skill); err != nil {
		t.Fatal(err)
	}
	settings, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	settings.DataDir = dataDir
	settings.Mode = mode
	settings.Aggregation.MinimumSessions = 3
	settings.Evaluation.Command = command
	settings.Evaluation.AllowAutopilot = mode == domain.ModeAutopilot
	manager := New(settings, database)
	return manager, database, skill
}

func seedCluster(t *testing.T, database *store.Store, skill domain.Skill, fingerprint, lesson string, offset int) domain.Cluster {
	return seedClusterKind(t, database, skill, domain.CardCorrection, fingerprint, lesson, offset)
}

func seedClusterKind(t *testing.T, database *store.Store, skill domain.Skill, kind domain.CardKind, fingerprint, lesson string, offset int) domain.Cluster {
	t.Helper()
	for index := range 3 {
		id := fmt.Sprintf("session-%d", offset+index)
		session := domain.Session{Reference: id, Source: domain.SourceCodex, ExternalID: id}
		if _, err := database.RecordSession(context.Background(), session); err != nil {
			t.Fatal(err)
		}
		card := domain.LearningCard{
			ID:         fmt.Sprintf("card-%d-%s", offset+index, strings.ReplaceAll(fingerprint, " ", "-")),
			SessionRef: id, SkillID: skill.ID, Kind: kind,
			Fingerprint: fingerprint, Summary: fingerprint, Lesson: lesson, Confidence: 0.9,
			CreatedAt: time.Now().Add(time.Duration(index) * time.Second),
		}
		if _, err := database.AddLearningCard(context.Background(), card); err != nil {
			t.Fatal(err)
		}
	}
	clusters, err := database.RebuildClusters(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, cluster := range clusters {
		if cluster.Fingerprint == fingerprint {
			return cluster
		}
	}
	t.Fatalf("cluster %q not found in %#v", fingerprint, clusters)
	return domain.Cluster{}
}

func runGit(t *testing.T, repository string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), output, err)
	}
	return strings.TrimSpace(string(output))
}

func makeWritable(root string) {
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			_ = os.Chmod(path, 0o700)
		} else {
			_ = os.Chmod(path, 0o600)
		}
		return nil
	})
}
