package pipeline

import (
	"context"
	"errors"

	"github.com/flemzord/skillloop/internal/domain"
	"github.com/flemzord/skillloop/internal/improvement"
)

const monitorRegressionReason = "evaluation regression"

// MonitorResult summarizes one monitoring pass over the active promotions.
// Promotion-specific failures are isolated so a broken evaluator or skill does
// not prevent the remaining promotions from being checked.
type MonitorResult struct {
	Checked    int
	Healthy    int
	Regressing int
	RolledBack int
	Failures   []MonitorFailure
}

type MonitorFailure struct {
	PromotionID string
	SkillID     string
	Error       string
}

// Monitor re-evaluates every active promotion against its exact persisted
// baseline/candidate pair. Only a completed evaluation that reports a
// regression can trigger rollback; evaluator and restoration errors fail safe.
func (manager Manager) Monitor(ctx context.Context) (MonitorResult, error) {
	result := MonitorResult{}
	if err := manager.validate(); err != nil {
		return result, err
	}
	current, unlock, err := manager.acquireCurrentMonitorPolicy(ctx)
	if err != nil {
		return result, err
	}
	if current.Config.Mode == domain.ModeObserve {
		_ = unlock()
		return result, nil
	}
	manager = current
	if err := unlock(); err != nil {
		return result, err
	}
	promotions, err := manager.Store.ListPromotions(ctx, true)
	if err != nil {
		return result, err
	}
	for _, promotion := range promotions {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		evaluation, evaluationPolicy, allowed, err := manager.evaluatePromotion(ctx, promotion)
		if !allowed {
			return result, err
		}
		result.Checked++
		if err != nil {
			result.addFailure(promotion, err)
			continue
		}
		if err := improvement.ValidateExternalRunPair(evaluation); err != nil {
			result.addFailure(promotion, err)
			continue
		}
		if err := manager.applyMonitorEvaluation(ctx, promotion, evaluation, evaluationPolicy, &result); err != nil {
			result.addFailure(promotion, err)
		}
	}
	return result, nil
}

// evaluatePromotion holds the current policy generation through candidate
// restoration and external evaluation. A queued policy writer owns the gate
// before this lock is released, so it takes effect before result application
// or any subsequent monitoring evaluation can acquire a new read lock.
func (manager Manager) evaluatePromotion(
	ctx context.Context,
	promotion domain.Promotion,
) (improvement.Evaluation, string, bool, error) {
	current, unlock, err := manager.acquireCurrentMonitorPolicy(ctx)
	if err != nil {
		return improvement.Evaluation{}, "", false, err
	}
	defer func() { _ = unlock() }()
	if current.Config.Mode == domain.ModeObserve {
		return improvement.Evaluation{}, "", false, nil
	}
	evaluationPolicy := evaluationPolicyDigest(current.Config)
	_, _, _, candidate, err := current.loadCandidate(ctx, promotion.ProposalID)
	if err != nil {
		return improvement.Evaluation{}, evaluationPolicy, true, err
	}
	evaluation, err := current.Improver.Evaluate(ctx, candidate)
	return evaluation, evaluationPolicy, true, err
}

func (manager Manager) acquireCurrentMonitorPolicy(ctx context.Context) (Manager, func() error, error) {
	current, unlock, err := manager.acquireCurrentPolicy(ctx)
	if err != nil {
		return Manager{}, nil, err
	}
	if manager.ConfigLoader == nil {
		current.Improver.Runner.Argv = append([]string(nil), manager.Improver.Runner.Argv...)
	}
	return current, unlock, nil
}

func (manager Manager) applyMonitorEvaluation(
	ctx context.Context,
	promotion domain.Promotion,
	evaluation improvement.Evaluation,
	evaluatedPolicy string,
	result *MonitorResult,
) error {
	unlock, err := manager.acquirePolicyLock(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	settings, err := manager.currentSettings()
	if err != nil {
		return err
	}
	if settings.Mode == domain.ModeObserve {
		return nil
	}
	if evaluationPolicyDigest(settings) != evaluatedPolicy {
		return errors.New("pipeline: monitor evaluation policy changed during evaluation; result discarded")
	}
	if evaluation.Passed {
		if err := manager.Store.UpdatePromotionMonitor(ctx, promotion.ID, domain.MonitorHealthy); err != nil {
			return err
		}
		result.Healthy++
		return nil
	}
	if _, err := manager.rollback(ctx, promotion.SkillID, promotion, "monitor", monitorRegressionReason); err != nil {
		return err
	}
	result.Regressing++
	result.RolledBack++
	return nil
}

func (result *MonitorResult) addFailure(promotion domain.Promotion, err error) {
	result.Failures = append(result.Failures, MonitorFailure{
		PromotionID: promotion.ID,
		SkillID:     promotion.SkillID,
		Error:       err.Error(),
	})
}
