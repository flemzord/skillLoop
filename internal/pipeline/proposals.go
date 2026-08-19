// Package pipeline coordinates durable proposal state with isolated Git
// improvement operations. It is shared by the CLI and daemon.
package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/flemzord/skillloop/internal/config"
	"github.com/flemzord/skillloop/internal/domain"
	"github.com/flemzord/skillloop/internal/improvement"
	"github.com/flemzord/skillloop/internal/store"
)

type Manager struct {
	Config   config.Config
	Store    *store.Store
	Improver improvement.Service
	Now      func() time.Time
}

type ProposalView struct {
	Proposal              domain.Proposal           `json:"proposal"`
	Diff                  string                    `json:"diff"`
	RequiresHumanApproval bool                      `json:"requires_human_approval"`
	Evaluations           []domain.EvaluationResult `json:"evaluations"`
	Audit                 []domain.AuditEntry       `json:"audit"`
}

type ProcessResult struct {
	Created   int
	Evaluated int
	Promoted  int
	Failures  []ProcessFailure
}

type ProcessFailure struct {
	ClusterID string
	Error     string
}

const emptyReservationStaleAfter = 5 * time.Minute

func New(settings config.Config, database *store.Store) Manager {
	return Manager{
		Config: settings,
		Store:  database,
		Improver: improvement.Service{
			StateDir: settings.DataDir,
			Runner:   improvement.Runner{Argv: settings.Evaluation.Command},
		},
		Now: time.Now,
	}
}

// Create reserves one proposal per cluster before touching Git, then fills the
// durable record with the isolated candidate revisions.
func (manager Manager) Create(ctx context.Context, clusterID, actor string) (domain.Proposal, bool, error) {
	if err := manager.validate(); err != nil {
		return domain.Proposal{}, false, err
	}
	if manager.Config.Mode == domain.ModeObserve {
		return domain.Proposal{}, false, errors.New("pipeline: observe mode cannot create proposals")
	}
	cluster, err := manager.Store.Cluster(ctx, clusterID)
	if err != nil {
		return domain.Proposal{}, false, err
	}
	if cluster.Status != domain.ClusterOpen {
		proposal, proposalErr := manager.Store.ProposalForCluster(ctx, cluster.ID)
		if proposalErr == nil {
			return proposal, false, nil
		}
		return domain.Proposal{}, false, fmt.Errorf("pipeline: cluster %s is %s", cluster.ID, cluster.Status)
	}
	if cluster.SessionCount < manager.Config.Aggregation.MinimumSessions {
		return domain.Proposal{}, false, fmt.Errorf(
			"pipeline: cluster %s has %d sessions, requires %d",
			cluster.ID, cluster.SessionCount, manager.Config.Aggregation.MinimumSessions,
		)
	}
	skill, err := manager.Store.Skill(ctx, cluster.SkillID)
	if err != nil {
		return domain.Proposal{}, false, err
	}
	proposal := domain.Proposal{
		ID:             stableID("proposal", cluster.ID),
		ClusterID:      cluster.ID,
		SkillID:        skill.ID,
		Status:         domain.ProposalPending,
		RepositoryPath: skill.RepositoryPath,
		CreatedAt:      manager.now().UTC(),
	}
	reserved, err := manager.Store.ReserveProposal(ctx, proposal)
	if err != nil {
		return domain.Proposal{}, false, err
	}
	if !reserved {
		existing, existingErr := manager.Store.ProposalForCluster(ctx, cluster.ID)
		if existingErr != nil {
			return domain.Proposal{}, false, fmt.Errorf("pipeline: proposal reservation lost: %w", existingErr)
		}
		return existing, false, nil
	}

	candidate, err := manager.Improver.Prepare(ctx, skill, cluster)
	if err != nil {
		_ = manager.Store.AbandonProposal(context.Background(), proposal.ID)
		return domain.Proposal{}, false, fmt.Errorf("pipeline: prepare proposal: %w", err)
	}
	abort := func() {
		_ = manager.Improver.Cleanup(context.Background(), candidate)
		_ = manager.Store.AbandonProposal(context.Background(), proposal.ID)
	}
	proposal.WorktreePath = candidate.WorktreePath
	proposal.Branch = candidate.Branch
	proposal.BaseCommit = candidate.BaseCommit
	proposal.CandidateCommit = candidate.CandidateCommit
	proposal.CreatedAt = candidate.CreatedAt
	proposal.UpdatedAt = candidate.CreatedAt
	if err := manager.Store.SaveProposal(ctx, proposal); err != nil {
		abort()
		return domain.Proposal{}, false, err
	}
	details := auditJSON(map[string]string{
		"base_commit": candidate.BaseCommit, "candidate_commit": candidate.CandidateCommit,
	})
	if _, err := manager.Store.AppendAudit(ctx, domain.AuditEntry{
		Action: "proposal.created", EntityType: "proposal", EntityID: proposal.ID,
		Actor: actorOr(actor, "pipeline"), Details: details,
	}); err != nil {
		abort()
		return domain.Proposal{}, false, err
	}
	return proposal, true, nil
}

func (manager Manager) Evaluate(ctx context.Context, proposalID, actor string) (domain.Proposal, improvement.Evaluation, error) {
	if manager.Config.Mode == domain.ModeObserve {
		return domain.Proposal{}, improvement.Evaluation{}, errors.New("pipeline: observe mode cannot evaluate proposals")
	}
	proposal, skill, cluster, candidate, err := manager.loadCandidate(ctx, proposalID)
	if err != nil {
		return domain.Proposal{}, improvement.Evaluation{}, err
	}
	if proposal.Status != domain.ProposalPending && proposal.Status != domain.ProposalEvaluated {
		return domain.Proposal{}, improvement.Evaluation{}, fmt.Errorf("pipeline: cannot evaluate proposal in status %s", proposal.Status)
	}
	evaluation, err := manager.Improver.Evaluate(ctx, candidate)
	if err != nil {
		return domain.Proposal{}, evaluation, fmt.Errorf("pipeline: evaluate proposal: %w", err)
	}
	_ = skill
	_ = cluster
	baselinePassed := !checkPassed(evaluation.Checks, "baseline-derived-case-fails")
	proposal.BaselineScore = boolScore(baselinePassed)
	proposal.CandidateScore = boolScore(evaluation.Passed)
	proposal.Status = domain.ProposalEvaluated
	proposal.UpdatedAt = manager.now().UTC()
	details := evaluationDetails(evaluation)
	baseline := domain.EvaluationResult{
		ID: stableID("evaluation-baseline", proposal.ID), ProposalID: proposal.ID,
		Variant: domain.EvaluationBaseline, Passed: baselinePassed, Score: proposal.BaselineScore,
		Duration: runDuration(evaluation.BaselineRun), Details: details, CreatedAt: evaluation.EvaluatedAt,
	}
	candidateResult := domain.EvaluationResult{
		ID: stableID("evaluation-candidate", proposal.ID), ProposalID: proposal.ID,
		Variant: domain.EvaluationCandidate, Passed: evaluation.Passed, Score: proposal.CandidateScore,
		Duration: runDuration(evaluation.CandidateRun), Details: details, CreatedAt: evaluation.EvaluatedAt,
	}
	if err := manager.Store.CompleteEvaluation(ctx, proposal, baseline, candidateResult); err != nil {
		return domain.Proposal{}, evaluation, err
	}
	if _, err := manager.Store.AppendAudit(ctx, domain.AuditEntry{
		Action: "proposal.evaluated", EntityType: "proposal", EntityID: proposal.ID,
		Actor: actorOr(actor, "pipeline"), Details: auditJSON(map[string]any{
			"base_commit": proposal.BaseCommit, "candidate_commit": proposal.CandidateCommit,
			"passed": evaluation.Passed,
		}),
	}); err != nil {
		return domain.Proposal{}, evaluation, err
	}
	stored, err := manager.Store.Proposal(ctx, proposal.ID)
	return stored, evaluation, err
}

func (manager Manager) Show(ctx context.Context, proposalID string) (ProposalView, error) {
	proposal, err := manager.Store.Proposal(ctx, proposalID)
	if err != nil {
		return ProposalView{}, err
	}
	evaluations, err := manager.Store.ListEvaluationResults(ctx, proposalID)
	if err != nil {
		return ProposalView{}, err
	}
	audit, err := manager.Store.ListAudit(ctx, "proposal", proposalID)
	if err != nil {
		return ProposalView{}, err
	}
	view := ProposalView{Proposal: proposal, Evaluations: evaluations, Audit: audit}
	if proposal.BaseCommit != "" && proposal.CandidateCommit != "" {
		_, _, _, candidate, candidateErr := manager.loadCandidate(ctx, proposalID)
		if candidateErr != nil {
			return ProposalView{}, candidateErr
		}
		view.Diff = candidate.Diff
		view.RequiresHumanApproval = candidate.RequiresHumanApproval
	}
	return view, nil
}

func (manager Manager) Approve(ctx context.Context, proposalID, actor string) (domain.Promotion, error) {
	if manager.Config.Mode == domain.ModeObserve {
		return domain.Promotion{}, errors.New("pipeline: observe mode cannot approve proposals")
	}
	proposal, err := manager.Store.Proposal(ctx, proposalID)
	if err != nil {
		return domain.Promotion{}, err
	}
	if proposal.Status == domain.ProposalPromoted {
		promotion, promotionErr := manager.Store.ActivePromotion(ctx, proposal.SkillID)
		if promotionErr != nil {
			return domain.Promotion{}, promotionErr
		}
		if promotion.ProposalID == proposal.ID && promotion.PromotedCommit == proposal.CandidateCommit {
			_, _, _, candidate, candidateErr := manager.loadCandidate(ctx, proposal.ID)
			if candidateErr == nil {
				_ = manager.Improver.Cleanup(ctx, candidate)
			}
			return promotion, nil
		}
		return domain.Promotion{}, errors.New("pipeline: promoted proposal is not the active exact release")
	}
	evaluation, err := manager.persistedEvaluation(ctx, proposal)
	if err != nil {
		return domain.Promotion{}, err
	}
	return manager.approve(ctx, proposal, evaluation, actorOr(actor, "user"))
}

func (manager Manager) Reject(ctx context.Context, proposalID, actor, reason string) error {
	proposal, skill, cluster, candidate, err := manager.loadCandidate(ctx, proposalID)
	if err != nil {
		return err
	}
	if proposal.Status == domain.ProposalPromoted || proposal.Status == domain.ProposalRolledBack {
		return fmt.Errorf("pipeline: cannot reject proposal in status %s", proposal.Status)
	}
	if proposal.CandidateCommit != "" {
		if err := manager.Improver.Reject(ctx, candidate); err != nil {
			return fmt.Errorf("pipeline: clean rejected candidate: %w", err)
		}
	}
	_ = skill
	_ = cluster
	return manager.Store.RejectProposal(ctx, proposal.ID, actorOr(actor, "user"), auditJSON(map[string]string{
		"reason":      reasonOr(reason, "rejected by user"),
		"base_commit": proposal.BaseCommit, "candidate_commit": proposal.CandidateCommit,
	}))
}

func (manager Manager) Rollback(ctx context.Context, skillID, actor, reason string) (domain.Rollback, error) {
	if err := manager.validate(); err != nil {
		return domain.Rollback{}, err
	}
	skill, err := manager.Store.Skill(ctx, skillID)
	if err != nil {
		return domain.Rollback{}, err
	}
	active, err := manager.Store.ActivePromotion(ctx, skill.ID)
	if err != nil {
		return domain.Rollback{}, err
	}
	current, err := manager.Improver.CurrentRelease(skill)
	if err != nil {
		return domain.Rollback{}, err
	}
	toCommit := active.PreviousCommit
	switch current.Commit {
	case active.PromotedCommit:
		rolledBack, rollbackErr := manager.Improver.Rollback(ctx, skill)
		if rollbackErr != nil {
			return domain.Rollback{}, rollbackErr
		}
		toCommit = rolledBack.CurrentCommit
	case active.PreviousCommit:
		// Filesystem rollback already completed; finish durable persistence.
	default:
		return domain.Rollback{}, fmt.Errorf("pipeline: current release drifted from active promotion")
	}
	rollback := domain.Rollback{
		ID:          stableID("rollback", active.ID+"\x00"+active.PromotedCommit+"\x00"+toCommit),
		PromotionID: active.ID, FromCommit: active.PromotedCommit, ToCommit: toCommit,
		Reason: reasonOr(reason, "manual rollback"), Actor: actorOr(actor, "user"), CreatedAt: manager.now().UTC(),
	}
	if err := manager.Store.RecordRollback(ctx, rollback); err != nil {
		return domain.Rollback{}, err
	}
	return rollback, nil
}

// ProcessEligible prepares and evaluates all open eligible clusters. Individual
// cluster failures are isolated so one bad skill cannot stop a daemon drain.
func (manager Manager) ProcessEligible(ctx context.Context, clusters []domain.Cluster) (ProcessResult, error) {
	result := ProcessResult{}
	if manager.Config.Mode == domain.ModeObserve {
		return result, nil
	}
	pending, err := manager.Store.ListProposals(ctx, domain.ProposalPending)
	if err != nil {
		return result, err
	}
	for _, proposal := range pending {
		if proposal.BaseCommit == "" || proposal.CandidateCommit == "" {
			if manager.now().Sub(proposal.CreatedAt) < emptyReservationStaleAfter {
				continue
			}
			if err := manager.Store.AbandonProposal(ctx, proposal.ID); err != nil {
				result.Failures = append(result.Failures, ProcessFailure{ClusterID: proposal.ClusterID, Error: err.Error()})
				continue
			}
			cluster, clusterErr := manager.Store.Cluster(ctx, proposal.ClusterID)
			if clusterErr != nil {
				result.Failures = append(result.Failures, ProcessFailure{ClusterID: proposal.ClusterID, Error: clusterErr.Error()})
				continue
			}
			proposal, _, err = manager.Create(ctx, cluster.ID, "daemon")
			if err != nil {
				result.Failures = append(result.Failures, ProcessFailure{ClusterID: cluster.ID, Error: err.Error()})
				continue
			}
			result.Created++
		}
		proposal, evaluation, evaluationErr := manager.Evaluate(ctx, proposal.ID, "daemon")
		if evaluationErr != nil {
			result.Failures = append(result.Failures, ProcessFailure{ClusterID: proposal.ClusterID, Error: evaluationErr.Error()})
			continue
		}
		result.Evaluated++
		manager.autopilot(ctx, proposal, evaluation, &result)
	}
	if manager.Config.Mode == domain.ModeAutopilot {
		for _, status := range []domain.ProposalStatus{domain.ProposalEvaluated, domain.ProposalApproved} {
			proposals, listErr := manager.Store.ListProposals(ctx, status)
			if listErr != nil {
				return result, listErr
			}
			for _, proposal := range proposals {
				evaluation, evaluationErr := manager.persistedEvaluation(ctx, proposal)
				if evaluationErr != nil {
					result.Failures = append(result.Failures, ProcessFailure{ClusterID: proposal.ClusterID, Error: evaluationErr.Error()})
					continue
				}
				if status == domain.ProposalEvaluated {
					_, _, _, candidate, candidateErr := manager.loadCandidate(ctx, proposal.ID)
					if candidateErr != nil {
						result.Failures = append(result.Failures, ProcessFailure{ClusterID: proposal.ClusterID, Error: candidateErr.Error()})
						continue
					}
					evaluation.RequiresHumanApproval = candidate.RequiresHumanApproval
				}
				manager.autopilot(ctx, proposal, evaluation, &result)
			}
		}
	}
	for _, cluster := range clusters {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if cluster.Status != domain.ClusterOpen || cluster.SessionCount < manager.Config.Aggregation.MinimumSessions {
			continue
		}
		proposal, created, err := manager.Create(ctx, cluster.ID, "daemon")
		if err != nil {
			result.Failures = append(result.Failures, ProcessFailure{ClusterID: cluster.ID, Error: err.Error()})
			continue
		}
		if !created {
			continue
		}
		result.Created++
		proposal, evaluation, err := manager.Evaluate(ctx, proposal.ID, "daemon")
		if err != nil {
			result.Failures = append(result.Failures, ProcessFailure{ClusterID: cluster.ID, Error: err.Error()})
			continue
		}
		result.Evaluated++
		manager.autopilot(ctx, proposal, evaluation, &result)
	}
	return result, nil
}

func (manager Manager) autopilot(ctx context.Context, proposal domain.Proposal, evaluation improvement.Evaluation, result *ProcessResult) {
	if manager.Config.Mode != domain.ModeAutopilot || !manager.Config.Evaluation.AllowAutopilot ||
		len(manager.Config.Evaluation.Command) == 0 || !evaluation.Passed || evaluation.RequiresHumanApproval ||
		evaluation.BaselineRun == nil || evaluation.CandidateRun == nil {
		return
	}
	if _, err := manager.approve(ctx, proposal, evaluation, "autopilot"); err != nil {
		result.Failures = append(result.Failures, ProcessFailure{ClusterID: proposal.ClusterID, Error: err.Error()})
		return
	}
	result.Promoted++
}

func (manager Manager) approve(ctx context.Context, proposal domain.Proposal, evaluation improvement.Evaluation, actor string) (domain.Promotion, error) {
	if !evaluation.Passed || proposal.CandidateScore-proposal.BaselineScore < manager.Config.Evaluation.MinimumImprovement {
		return domain.Promotion{}, improvement.ErrEvaluationFailed
	}
	if err := manager.Store.ApproveProposal(ctx, proposal.ID, proposal.BaseCommit, proposal.CandidateCommit, actor, auditJSON(map[string]string{
		"base_commit": proposal.BaseCommit, "candidate_commit": proposal.CandidateCommit,
	})); err != nil {
		return domain.Promotion{}, err
	}
	_, skill, _, candidate, err := manager.loadCandidate(ctx, proposal.ID)
	if err != nil {
		return domain.Promotion{}, err
	}
	promoted, err := manager.Improver.Promote(ctx, skill, candidate, evaluation, improvement.Approval{
		BaseCommit: proposal.BaseCommit, CandidateCommit: proposal.CandidateCommit,
	})
	if err != nil {
		return domain.Promotion{}, err
	}
	promotion := domain.Promotion{
		ID:         stableID("promotion", proposal.ID+"\x00"+proposal.CandidateCommit),
		ProposalID: proposal.ID, SkillID: proposal.SkillID,
		PreviousCommit: promoted.PreviousCommit, PromotedCommit: promoted.CurrentCommit,
		Active: true, MonitorStatus: domain.MonitorPending, PromotedAt: promoted.PromotedAt,
	}
	if err := manager.Store.RecordPromotionDecision(ctx, promotion, actor, auditJSON(map[string]string{
		"previous_commit": promotion.PreviousCommit, "promoted_commit": promotion.PromotedCommit,
	})); err != nil {
		return domain.Promotion{}, err
	}
	if err := manager.Improver.Cleanup(ctx, candidate); err != nil {
		return promotion, fmt.Errorf("pipeline: promoted but candidate cleanup failed: %w", err)
	}
	return promotion, nil
}

func (manager Manager) persistedEvaluation(ctx context.Context, proposal domain.Proposal) (improvement.Evaluation, error) {
	results, err := manager.Store.ListEvaluationResults(ctx, proposal.ID)
	if err != nil {
		return improvement.Evaluation{}, err
	}
	var baseline, candidate *domain.EvaluationResult
	for index := range results {
		switch results[index].Variant {
		case domain.EvaluationBaseline:
			baseline = &results[index]
		case domain.EvaluationCandidate:
			candidate = &results[index]
		}
	}
	if baseline == nil || candidate == nil || baseline.Passed || !candidate.Passed {
		return improvement.Evaluation{}, improvement.ErrEvaluationFailed
	}
	var metadata struct {
		RequiresHumanApproval bool            `json:"requires_human_approval"`
		BaselineRun           json.RawMessage `json:"baseline_run"`
		CandidateRun          json.RawMessage `json:"candidate_run"`
	}
	if err := json.Unmarshal([]byte(candidate.Details), &metadata); err != nil {
		return improvement.Evaluation{}, fmt.Errorf("pipeline: decode persisted evaluation: %w", err)
	}
	var baselineRun, candidateRun *improvement.RunResult
	if len(metadata.BaselineRun) > 0 && string(metadata.BaselineRun) != "null" {
		baselineRun = &improvement.RunResult{}
	}
	if len(metadata.CandidateRun) > 0 && string(metadata.CandidateRun) != "null" {
		candidateRun = &improvement.RunResult{}
	}
	return improvement.Evaluation{
		SkillID: proposal.SkillID, ClusterID: proposal.ClusterID,
		BaseCommit: proposal.BaseCommit, CandidateCommit: proposal.CandidateCommit,
		BaselineRun: baselineRun, CandidateRun: candidateRun,
		RequiresHumanApproval: metadata.RequiresHumanApproval,
		Passed:                true, EvaluatedAt: candidate.CreatedAt,
	}, nil
}

func (manager Manager) loadCandidate(ctx context.Context, proposalID string) (domain.Proposal, domain.Skill, domain.Cluster, improvement.Candidate, error) {
	proposal, err := manager.Store.Proposal(ctx, proposalID)
	if err != nil {
		return domain.Proposal{}, domain.Skill{}, domain.Cluster{}, improvement.Candidate{}, err
	}
	skill, err := manager.Store.Skill(ctx, proposal.SkillID)
	if err != nil {
		return domain.Proposal{}, domain.Skill{}, domain.Cluster{}, improvement.Candidate{}, err
	}
	cluster, err := manager.Store.Cluster(ctx, proposal.ClusterID)
	if err != nil {
		return domain.Proposal{}, domain.Skill{}, domain.Cluster{}, improvement.Candidate{}, err
	}
	candidate, err := manager.Improver.Restore(ctx, skill, cluster, proposal)
	if err != nil {
		return domain.Proposal{}, domain.Skill{}, domain.Cluster{}, improvement.Candidate{}, err
	}
	return proposal, skill, cluster, candidate, nil
}

func (manager Manager) validate() error {
	if manager.Store == nil {
		return errors.New("pipeline: store is required")
	}
	if manager.Now == nil {
		return errors.New("pipeline: clock is required")
	}
	return nil
}

func (manager Manager) now() time.Time {
	if manager.Now == nil {
		return time.Now()
	}
	return manager.Now()
}

func stableID(prefix, value string) string {
	digest := sha256.Sum256([]byte(value))
	return prefix + "-" + hex.EncodeToString(digest[:12])
}

func checkPassed(checks []improvement.Check, name string) bool {
	for _, check := range checks {
		if check.Name == name {
			return check.Passed
		}
	}
	return false
}

func boolScore(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func runDuration(result *improvement.RunResult) time.Duration {
	if result == nil {
		return 0
	}
	return result.Duration
}

func evaluationDetails(evaluation improvement.Evaluation) string {
	type safeRun struct {
		ExitCode  int  `json:"exit_code"`
		Truncated bool `json:"truncated"`
		TimedOut  bool `json:"timed_out"`
	}
	details := struct {
		Checks                []improvement.Check `json:"checks"`
		RequiresHumanApproval bool                `json:"requires_human_approval"`
		BaselineRun           *safeRun            `json:"baseline_run,omitempty"`
		CandidateRun          *safeRun            `json:"candidate_run,omitempty"`
	}{Checks: evaluation.Checks, RequiresHumanApproval: evaluation.RequiresHumanApproval}
	if evaluation.BaselineRun != nil {
		details.BaselineRun = &safeRun{ExitCode: evaluation.BaselineRun.ExitCode, Truncated: evaluation.BaselineRun.Truncated, TimedOut: evaluation.BaselineRun.TimedOut}
	}
	if evaluation.CandidateRun != nil {
		details.CandidateRun = &safeRun{ExitCode: evaluation.CandidateRun.ExitCode, Truncated: evaluation.CandidateRun.Truncated, TimedOut: evaluation.CandidateRun.TimedOut}
	}
	encoded, _ := json.Marshal(details)
	return string(encoded)
}

func auditJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func actorOr(actor, fallback string) string {
	if actor != "" {
		return actor
	}
	return fallback
}

func reasonOr(reason, fallback string) string {
	if reason != "" {
		return reason
	}
	return fallback
}
