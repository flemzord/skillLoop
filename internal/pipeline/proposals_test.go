package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	manager, database, skill := newTestManager(t, domain.ModePropose, nil)
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

	secondCluster := seedCluster(t, database, skill, "artifact verification", "Inspect the generated artifact before completion.", 10)
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

func TestAutopilotKeepsRiskyCandidateForExplicitApproval(t *testing.T) {
	t.Setenv("SKILLLOOP_PIPELINE_RUNNER", "1")
	command := []string{os.Args[0], "-test.run=^TestPipelineRunnerHelper$"}
	manager, database, skill := newTestManager(t, domain.ModeAutopilot, command)
	cluster := seedCluster(t, database, skill, "security permissions", "Review chmod permissions before applying the change.", 1)
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
	cluster := seedCluster(t, database, skill, "resume approved", "Validate the exact output.", 1)
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

func TestPipelineRunnerHelper(t *testing.T) {
	if os.Getenv("SKILLLOOP_PIPELINE_RUNNER") != "1" {
		return
	}
	contents, err := os.ReadFile(filepath.Join("skills", "demo", "SKILL.md"))
	if err != nil {
		os.Exit(2)
	}
	if !strings.Contains(string(contents), "<!-- skillloop:begin ") {
		os.Exit(1)
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
	t.Helper()
	for index := range 3 {
		id := fmt.Sprintf("session-%d", offset+index)
		session := domain.Session{Reference: id, Source: domain.SourceCodex, ExternalID: id}
		if _, err := database.RecordSession(context.Background(), session); err != nil {
			t.Fatal(err)
		}
		card := domain.LearningCard{
			ID:         fmt.Sprintf("card-%d-%s", offset+index, strings.ReplaceAll(fingerprint, " ", "-")),
			SessionRef: id, SkillID: skill.ID, Kind: domain.CardCorrection,
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
