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
	"math"
	"time"

	"github.com/flemzord/skillloop/internal/config"
	"github.com/flemzord/skillloop/internal/domain"
	"github.com/flemzord/skillloop/internal/improvement"
	"github.com/flemzord/skillloop/internal/store"
)

type Manager struct {
	Config       config.Config
	Store        *store.Store
	Improver     improvement.Service
	Now          func() time.Time
	ConfigLoader func() (config.Config, error)
	PolicyLocker func(context.Context) (func() error, error)
	SkillLocker  func(context.Context, string) (func() error, error)
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
	settings = cloneConfig(settings)
	return Manager{
		Config: settings,
		Store:  database,
		Improver: improvement.Service{
			StateDir: settings.DataDir,
			Runner:   improvement.Runner{Argv: append([]string(nil), settings.Evaluation.Command...)},
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
	current, unlock, err := manager.acquireCurrentPolicy(ctx)
	if err != nil {
		return domain.Proposal{}, false, err
	}
	defer func() { _ = unlock() }()
	if current.Config.Mode == domain.ModeObserve {
		return domain.Proposal{}, false, errors.New("pipeline: observe mode cannot create proposals")
	}
	return current.create(ctx, clusterID, actor)
}

func (manager Manager) create(ctx context.Context, clusterID, actor string) (domain.Proposal, bool, error) {
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
	proposal.Fingerprint = candidate.Fingerprint
	proposal.Lesson = candidate.Lesson
	proposal.CardKind = candidate.CardKind
	proposal.RequiresHumanApproval = candidate.RequiresHumanApproval
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
	if err := manager.validate(); err != nil {
		return domain.Proposal{}, improvement.Evaluation{}, err
	}
	current, unlock, err := manager.acquireCurrentPolicy(ctx)
	if err != nil {
		return domain.Proposal{}, improvement.Evaluation{}, err
	}
	defer func() { _ = unlock() }()
	if current.Config.Mode == domain.ModeObserve {
		return domain.Proposal{}, improvement.Evaluation{}, errors.New("pipeline: observe mode cannot evaluate proposals")
	}
	if !equalCommands(current.Config.Evaluation.Command, current.Improver.Runner.Argv) {
		return domain.Proposal{}, improvement.Evaluation{}, errors.New("pipeline: evaluator runner differs from current evaluation policy")
	}
	proposal, _, _, candidate, err := current.loadCandidate(ctx, proposalID)
	if err != nil {
		return domain.Proposal{}, improvement.Evaluation{}, err
	}
	if proposal.Status != domain.ProposalPending && proposal.Status != domain.ProposalEvaluated {
		return domain.Proposal{}, improvement.Evaluation{}, fmt.Errorf("pipeline: cannot evaluate proposal in status %s", proposal.Status)
	}
	evaluatedPolicy := evaluationPolicyDigest(current.Config)
	evaluation, err := current.Improver.Evaluate(ctx, candidate)
	if err != nil {
		return domain.Proposal{}, evaluation, fmt.Errorf("pipeline: evaluate proposal: %w", err)
	}
	appliedSettings, err := current.currentSettings()
	if err != nil {
		return domain.Proposal{}, evaluation, err
	}
	if appliedSettings.Mode == domain.ModeObserve {
		return domain.Proposal{}, evaluation, errors.New("pipeline: observe mode enabled during evaluation; result discarded")
	}
	if evaluationPolicyDigest(appliedSettings) != evaluatedPolicy {
		return domain.Proposal{}, evaluation, errors.New("pipeline: evaluation policy changed during evaluation; result discarded")
	}
	current.Config = appliedSettings
	current.Improver.Runner.Argv = append([]string(nil), appliedSettings.Evaluation.Command...)
	return current.completeEvaluation(ctx, proposal, evaluation, actor)
}

func (manager Manager) completeEvaluation(
	ctx context.Context,
	proposal domain.Proposal,
	evaluation improvement.Evaluation,
	actor string,
) (domain.Proposal, improvement.Evaluation, error) {
	baselinePassed := !checkPassed(evaluation.Checks, "baseline-derived-case-fails")
	proposal.BaselineScore = boolScore(baselinePassed)
	proposal.CandidateScore = boolScore(evaluation.Passed)
	proposal.Status = domain.ProposalEvaluated
	proposal.UpdatedAt = manager.now().UTC()
	details := evaluationDetails(evaluation, manager.Config)
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
	persistedRequiresHumanApproval, err := persistedEvaluationRequiresHumanApproval(evaluations)
	if err != nil {
		return ProposalView{}, err
	}
	view := ProposalView{
		Proposal: proposal, Evaluations: evaluations, Audit: audit,
		RequiresHumanApproval: proposal.RequiresHumanApproval || persistedRequiresHumanApproval,
	}
	if proposal.BaseCommit != "" && proposal.CandidateCommit != "" {
		_, _, _, candidate, candidateErr := manager.loadCandidate(ctx, proposalID)
		if candidateErr != nil {
			return ProposalView{}, candidateErr
		}
		view.Diff = candidate.Diff
		view.RequiresHumanApproval = view.RequiresHumanApproval || candidate.RequiresHumanApproval
	}
	return view, nil
}

func (manager Manager) Approve(ctx context.Context, proposalID, actor string) (domain.Promotion, error) {
	if err := manager.validate(); err != nil {
		return domain.Promotion{}, err
	}
	current, unlock, err := manager.acquireCurrentPolicy(ctx)
	if err != nil {
		return domain.Promotion{}, err
	}
	defer func() { _ = unlock() }()
	if current.Config.Mode == domain.ModeObserve {
		return domain.Promotion{}, errors.New("pipeline: observe mode cannot approve proposals")
	}
	proposal, err := current.Store.Proposal(ctx, proposalID)
	if err != nil {
		return domain.Promotion{}, err
	}
	if proposal.Status == domain.ProposalPromoted {
		skill, skillErr := current.Store.Skill(ctx, proposal.SkillID)
		if skillErr != nil {
			return domain.Promotion{}, skillErr
		}
		release, releaseErr := current.acquireSkillFence(ctx, skill.ID)
		if releaseErr != nil {
			return domain.Promotion{}, releaseErr
		}
		defer func() { _ = release() }()
		promotion, promotionErr := current.Store.ActivePromotion(ctx, proposal.SkillID)
		if promotionErr != nil {
			return domain.Promotion{}, promotionErr
		}
		if promotion.ProposalID == proposal.ID && promotion.PromotedCommit == proposal.CandidateCommit {
			activeRelease, currentErr := current.Improver.CurrentRelease(skill)
			if currentErr != nil {
				return domain.Promotion{}, currentErr
			}
			if activeRelease.Commit != promotion.PromotedCommit {
				return domain.Promotion{}, errors.New("pipeline: active promotion differs from current release")
			}
			_, _, _, candidate, candidateErr := current.loadCandidate(ctx, proposal.ID)
			if candidateErr == nil {
				_ = current.Improver.Cleanup(ctx, candidate)
			}
			return promotion, nil
		}
		return domain.Promotion{}, errors.New("pipeline: promoted proposal is not the active exact release")
	}
	evaluation, err := current.persistedEvaluation(ctx, proposal)
	if err != nil {
		return domain.Promotion{}, err
	}
	return current.approveAuthorized(ctx, proposal, evaluation, actorOr(actor, "user"))
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
	return manager.rollback(ctx, skillID, domain.Promotion{}, actor, reason)
}

func (manager Manager) rollback(
	ctx context.Context,
	skillID string,
	expected domain.Promotion,
	actor,
	reason string,
) (domain.Rollback, error) {
	if err := manager.validate(); err != nil {
		return domain.Rollback{}, err
	}
	skill, err := manager.Store.Skill(ctx, skillID)
	if err != nil {
		return domain.Rollback{}, err
	}
	release, err := manager.acquireSkillFence(ctx, skill.ID)
	if err != nil {
		return domain.Rollback{}, err
	}
	defer func() { _ = release() }()
	active, err := manager.Store.ActivePromotion(ctx, skill.ID)
	if err != nil {
		return domain.Rollback{}, err
	}
	if expected.ID != "" && (active.ID != expected.ID ||
		active.ProposalID != expected.ProposalID ||
		active.PromotedCommit != expected.PromotedCommit ||
		active.PreviousCommit != expected.PreviousCommit) {
		return domain.Rollback{}, errors.New("pipeline: active promotion generation changed; rollback discarded")
	}
	current, err := manager.Improver.CurrentRelease(skill)
	if err != nil {
		return domain.Rollback{}, err
	}
	toCommit := active.PreviousCommit
	switch current.Commit {
	case active.PromotedCommit:
		rolledBack, rollbackErr := manager.Improver.RollbackExpected(
			ctx, skill, active.PromotedCommit, active.PreviousCommit,
		)
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
	if err := manager.validate(); err != nil {
		return result, err
	}
	current, unlock, err := manager.acquireCurrentPolicy(ctx)
	if err != nil {
		return result, err
	}
	if current.Config.Mode == domain.ModeObserve {
		_ = unlock()
		return result, nil
	}
	_ = unlock()
	return current.processEligible(ctx, clusters)
}

func (manager Manager) processEligible(ctx context.Context, clusters []domain.Cluster) (ProcessResult, error) {
	result := ProcessResult{}
	pending, err := manager.Store.ListProposals(ctx, domain.ProposalPending)
	if err != nil {
		return result, err
	}
	for _, proposal := range pending {
		if proposal.BaseCommit == "" || proposal.CandidateCommit == "" {
			if manager.now().Sub(proposal.UpdatedAt) < emptyReservationStaleAfter {
				continue
			}
			abandoned, abandonErr := manager.abandonProposalIfAllowed(ctx, proposal.ID, proposal.UpdatedAt)
			if abandonErr != nil {
				result.Failures = append(result.Failures, ProcessFailure{ClusterID: proposal.ClusterID, Error: abandonErr.Error()})
				continue
			}
			if !abandoned {
				// Another worker refreshed or filled the reservation after this
				// list snapshot. Leave it for the next drain.
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
					evaluation.RequiresHumanApproval = evaluation.RequiresHumanApproval || candidate.RequiresHumanApproval
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
		proposal.CardKind != domain.CardValidation || proposal.RequiresHumanApproval ||
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
	current, unlock, err := manager.acquireCurrentPolicy(ctx)
	if err != nil {
		return domain.Promotion{}, err
	}
	defer func() { _ = unlock() }()
	if current.Config.Mode == domain.ModeObserve {
		return domain.Promotion{}, errors.New("pipeline: observe mode cannot promote proposals")
	}
	if actor == "autopilot" && autopilotPolicyDigest(current.Config) != autopilotPolicyDigest(manager.Config) {
		return domain.Promotion{}, errors.New("pipeline: autopilot policy changed during processing; automatic promotion refused")
	}
	return current.approveAuthorized(ctx, proposal, evaluation, actor)
}

func (manager Manager) approveAuthorized(ctx context.Context, proposal domain.Proposal, evaluation improvement.Evaluation, actor string) (domain.Promotion, error) {
	if err := improvement.ValidatePromotionProof(evaluation); err != nil {
		return domain.Promotion{}, fmt.Errorf("pipeline: promotion requires comparative proof: %w", err)
	}
	if !evaluation.Passed || proposal.CandidateScore-proposal.BaselineScore < manager.Config.Evaluation.MinimumImprovement {
		return domain.Promotion{}, improvement.ErrEvaluationFailed
	}
	_, skill, cluster, candidate, err := manager.loadCandidate(ctx, proposal.ID)
	if err != nil {
		return domain.Promotion{}, err
	}
	if actor == "autopilot" && (candidate.CardKind != domain.CardValidation || candidate.RequiresHumanApproval) {
		return domain.Promotion{}, fmt.Errorf("pipeline: candidate requires explicit human approval: %w", improvement.ErrUnsafeChange)
	}
	if err := manager.authorizePromotion(actor, cluster); err != nil {
		return domain.Promotion{}, err
	}
	if err := manager.Store.ApproveProposal(ctx, proposal.ID, proposal.BaseCommit, proposal.CandidateCommit, actor, auditJSON(map[string]string{
		"base_commit": proposal.BaseCommit, "candidate_commit": proposal.CandidateCommit,
	})); err != nil {
		return domain.Promotion{}, err
	}
	if err := manager.authorizePromotion(actor, cluster); err != nil {
		return domain.Promotion{}, err
	}
	release, err := manager.acquireSkillFence(ctx, skill.ID)
	if err != nil {
		return domain.Promotion{}, err
	}
	defer func() { _ = release() }()
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
		_, rollbackErr := manager.Improver.RollbackExpected(
			context.Background(), skill, promotion.PromotedCommit, promotion.PreviousCommit,
		)
		if rollbackErr != nil {
			return domain.Promotion{}, errors.Join(
				fmt.Errorf("pipeline: persist promotion decision: %w", err),
				fmt.Errorf("pipeline: compensate filesystem promotion: %w", rollbackErr),
			)
		}
		return domain.Promotion{}, fmt.Errorf("pipeline: persist promotion decision; filesystem promotion rolled back: %w", err)
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
	if baseline.Details != candidate.Details {
		return improvement.Evaluation{}, errors.New("pipeline: persisted evaluation variants have different proof metadata")
	}
	type persistedRun struct {
		Revision            string `json:"revision"`
		ExitCode            int    `json:"exit_code"`
		Truncated           bool   `json:"truncated"`
		TimedOut            bool   `json:"timed_out"`
		ContainmentVerified bool   `json:"containment_verified"`
	}
	var metadata struct {
		RequiresHumanApproval  bool          `json:"requires_human_approval"`
		EvaluationPolicyDigest string        `json:"evaluation_policy_digest"`
		BaselineRun            *persistedRun `json:"baseline_run"`
		CandidateRun           *persistedRun `json:"candidate_run"`
	}
	if err := json.Unmarshal([]byte(candidate.Details), &metadata); err != nil {
		return improvement.Evaluation{}, fmt.Errorf("pipeline: decode persisted evaluation: %w", err)
	}
	if metadata.EvaluationPolicyDigest != evaluationPolicyDigest(manager.Config) {
		return improvement.Evaluation{}, errors.New("pipeline: persisted evaluation policy changed; rerun evaluation before approval")
	}
	var baselineRun, candidateRun *improvement.RunResult
	if metadata.BaselineRun != nil {
		baselineRun = &improvement.RunResult{
			Revision: metadata.BaselineRun.Revision, ExitCode: metadata.BaselineRun.ExitCode,
			Truncated: metadata.BaselineRun.Truncated, TimedOut: metadata.BaselineRun.TimedOut,
			ContainmentVerified: metadata.BaselineRun.ContainmentVerified,
		}
	}
	if metadata.CandidateRun != nil {
		candidateRun = &improvement.RunResult{
			Revision: metadata.CandidateRun.Revision, ExitCode: metadata.CandidateRun.ExitCode,
			Truncated: metadata.CandidateRun.Truncated, TimedOut: metadata.CandidateRun.TimedOut,
			ContainmentVerified: metadata.CandidateRun.ContainmentVerified,
		}
	}
	return improvement.Evaluation{
		SkillID: proposal.SkillID, ClusterID: proposal.ClusterID,
		BaseCommit: proposal.BaseCommit, CandidateCommit: proposal.CandidateCommit,
		BaselineRun: baselineRun, CandidateRun: candidateRun,
		RequiresHumanApproval: proposal.RequiresHumanApproval || metadata.RequiresHumanApproval,
		Passed:                true, EvaluatedAt: candidate.CreatedAt,
	}, nil
}

func persistedEvaluationRequiresHumanApproval(results []domain.EvaluationResult) (bool, error) {
	var candidate *domain.EvaluationResult
	for index := range results {
		if results[index].Variant != domain.EvaluationCandidate {
			continue
		}
		if candidate == nil || results[index].CreatedAt.After(candidate.CreatedAt) {
			candidate = &results[index]
		}
	}
	if candidate == nil {
		return false, nil
	}
	var metadata struct {
		RequiresHumanApproval bool `json:"requires_human_approval"`
	}
	if err := json.Unmarshal([]byte(candidate.Details), &metadata); err != nil {
		return false, fmt.Errorf("pipeline: decode persisted evaluation approval requirement: %w", err)
	}
	return metadata.RequiresHumanApproval, nil
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

func (manager Manager) currentSettings() (config.Config, error) {
	if manager.ConfigLoader == nil {
		return manager.Config, nil
	}
	settings, err := manager.ConfigLoader()
	if err != nil {
		return config.Config{}, fmt.Errorf("pipeline: reload config: %w", err)
	}
	if err := settings.Validate(); err != nil {
		return config.Config{}, fmt.Errorf("pipeline: validate reloaded config: %w", err)
	}
	return cloneConfig(settings), nil
}

func (manager Manager) authorizePromotion(actor string, cluster domain.Cluster) error {
	settings := manager.Config
	if settings.Mode == domain.ModeObserve {
		return errors.New("pipeline: observe mode cannot promote proposals")
	}
	if actor != "autopilot" {
		return nil
	}
	if settings.Mode != domain.ModeAutopilot || !settings.Evaluation.AllowAutopilot || len(settings.Evaluation.Command) == 0 {
		return errors.New("pipeline: autopilot policy changed during processing; automatic promotion refused")
	}
	if cluster.SessionCount < settings.Aggregation.MinimumSessions {
		return fmt.Errorf(
			"pipeline: cluster has %d sessions but current autopilot quorum requires %d; automatic promotion refused",
			cluster.SessionCount, settings.Aggregation.MinimumSessions,
		)
	}
	return nil
}

func (manager Manager) acquirePolicyLock(ctx context.Context) (func() error, error) {
	if manager.PolicyLocker == nil {
		if manager.ConfigLoader != nil {
			return nil, errors.New("pipeline: dynamic config requires a policy lock")
		}
		return func() error { return nil }, nil
	}
	unlock, err := manager.PolicyLocker(ctx)
	if err != nil {
		return nil, fmt.Errorf("pipeline: acquire policy lock: %w", err)
	}
	if unlock == nil {
		return nil, errors.New("pipeline: policy lock returned no unlock function")
	}
	return unlock, nil
}

// acquireCurrentPolicy reads the current settings while holding the shared
// policy lock. The returned Manager is bound to that policy generation. A
// caller that releases the lock around slow non-mutating work must reacquire it
// and compare the relevant policy digest before applying the result.
func (manager Manager) acquireCurrentPolicy(ctx context.Context) (Manager, func() error, error) {
	unlock, err := manager.acquirePolicyLock(ctx)
	if err != nil {
		return Manager{}, nil, err
	}
	settings, err := manager.currentSettings()
	if err != nil {
		_ = unlock()
		return Manager{}, nil, err
	}
	manager.Config = settings
	manager.Improver.Runner.Argv = append([]string(nil), settings.Evaluation.Command...)
	return manager, unlock, nil
}

func (manager Manager) abandonProposalIfAllowed(ctx context.Context, proposalID string, updatedAt time.Time) (bool, error) {
	current, unlock, err := manager.acquireCurrentPolicy(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = unlock() }()
	if current.Config.Mode == domain.ModeObserve {
		return false, nil
	}
	return current.Store.AbandonProposalIfUnchanged(ctx, proposalID, updatedAt)
}

func (manager Manager) acquireSkillFence(ctx context.Context, skillID string) (func() error, error) {
	if manager.SkillLocker == nil {
		return manager.Improver.AcquireSkillFence(ctx, skillID)
	}
	release, err := manager.SkillLocker(ctx, skillID)
	if err != nil {
		return nil, fmt.Errorf("pipeline: acquire skill release fence: %w", err)
	}
	if release == nil {
		return nil, errors.New("pipeline: skill release fence returned no release function")
	}
	return release, nil
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

func evaluationDetails(evaluation improvement.Evaluation, settings config.Config) string {
	type safeRun struct {
		Revision            string `json:"revision"`
		ExitCode            int    `json:"exit_code"`
		Truncated           bool   `json:"truncated"`
		TimedOut            bool   `json:"timed_out"`
		ContainmentVerified bool   `json:"containment_verified"`
	}
	details := struct {
		Checks                 []improvement.Check `json:"checks"`
		RequiresHumanApproval  bool                `json:"requires_human_approval"`
		EvaluationPolicyDigest string              `json:"evaluation_policy_digest"`
		BaselineRun            *safeRun            `json:"baseline_run,omitempty"`
		CandidateRun           *safeRun            `json:"candidate_run,omitempty"`
	}{
		Checks: evaluation.Checks, RequiresHumanApproval: evaluation.RequiresHumanApproval,
		EvaluationPolicyDigest: evaluationPolicyDigest(settings),
	}
	if evaluation.BaselineRun != nil {
		details.BaselineRun = &safeRun{Revision: evaluation.BaselineRun.Revision, ExitCode: evaluation.BaselineRun.ExitCode, Truncated: evaluation.BaselineRun.Truncated, TimedOut: evaluation.BaselineRun.TimedOut, ContainmentVerified: evaluation.BaselineRun.ContainmentVerified}
	}
	if evaluation.CandidateRun != nil {
		details.CandidateRun = &safeRun{Revision: evaluation.CandidateRun.Revision, ExitCode: evaluation.CandidateRun.ExitCode, Truncated: evaluation.CandidateRun.Truncated, TimedOut: evaluation.CandidateRun.TimedOut, ContainmentVerified: evaluation.CandidateRun.ContainmentVerified}
	}
	encoded, _ := json.Marshal(details)
	return string(encoded)
}

func evaluationPolicyDigest(settings config.Config) string {
	payload := struct {
		Version                int      `json:"version"`
		Command                []string `json:"command"`
		MinimumImprovementBits uint64   `json:"minimum_improvement_bits"`
		AllowAutopilot         bool     `json:"allow_autopilot"`
	}{
		Version: settings.Version, Command: settings.Evaluation.Command,
		MinimumImprovementBits: math.Float64bits(settings.Evaluation.MinimumImprovement),
		AllowAutopilot:         settings.Evaluation.AllowAutopilot,
	}
	encoded, _ := json.Marshal(payload)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func autopilotPolicyDigest(settings config.Config) string {
	payload := struct {
		Mode                   domain.AutonomyMode `json:"mode"`
		MinimumSessions        int                 `json:"minimum_sessions"`
		EvaluationPolicyDigest string              `json:"evaluation_policy_digest"`
	}{
		Mode: settings.Mode, MinimumSessions: settings.Aggregation.MinimumSessions,
		EvaluationPolicyDigest: evaluationPolicyDigest(settings),
	}
	encoded, _ := json.Marshal(payload)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func cloneConfig(settings config.Config) config.Config {
	settings.Evaluation.Command = append([]string(nil), settings.Evaluation.Command...)
	settings.ExcludedPaths = append([]string(nil), settings.ExcludedPaths...)
	return settings
}

func equalCommands(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
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
