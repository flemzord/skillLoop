package pipeline

import (
	"context"

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
	promotions, err := manager.Store.ListPromotions(ctx, true)
	if err != nil {
		return result, err
	}
	for _, promotion := range promotions {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Checked++

		_, _, _, candidate, err := manager.loadCandidate(ctx, promotion.ProposalID)
		if err != nil {
			result.addFailure(promotion, err)
			continue
		}
		evaluation, err := manager.Improver.Evaluate(ctx, candidate)
		if err != nil {
			result.addFailure(promotion, err)
			continue
		}
		if err := improvement.ValidateExternalRunPair(evaluation); err != nil {
			result.addFailure(promotion, err)
			continue
		}
		if evaluation.Passed {
			if err := manager.Store.UpdatePromotionMonitor(ctx, promotion.ID, domain.MonitorHealthy); err != nil {
				result.addFailure(promotion, err)
				continue
			}
			result.Healthy++
			continue
		}

		if err := manager.Store.UpdatePromotionMonitor(ctx, promotion.ID, domain.MonitorRegressing); err != nil {
			result.addFailure(promotion, err)
			continue
		}
		result.Regressing++
		if _, err := manager.Rollback(ctx, promotion.SkillID, "monitor", monitorRegressionReason); err != nil {
			result.addFailure(promotion, err)
			continue
		}
		result.RolledBack++
	}
	return result, nil
}

func (result *MonitorResult) addFailure(promotion domain.Promotion, err error) {
	result.Failures = append(result.Failures, MonitorFailure{
		PromotionID: promotion.ID,
		SkillID:     promotion.SkillID,
		Error:       err.Error(),
	})
}
