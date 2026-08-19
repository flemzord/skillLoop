package improvement

import (
	"context"
	"fmt"

	"github.com/flemzord/skillloop/internal/domain"
)

// Restore reconstructs a candidate from durable domain records. The diff and
// marker fingerprint are derived again from the exact commits, so callers do
// not need to persist implementation-only Candidate fields.
func (service Service) Restore(ctx context.Context, skill domain.Skill, cluster domain.Cluster, proposal domain.Proposal) (Candidate, error) {
	if proposal.SkillID != skill.ID || proposal.ClusterID != cluster.ID || cluster.SkillID != skill.ID {
		return Candidate{}, fmt.Errorf("proposal ownership changed: %w", ErrDrift)
	}
	repository, err := resolveRepository(ctx, skill.RepositoryPath)
	if err != nil {
		return Candidate{}, err
	}
	if !samePath(repository, proposal.RepositoryPath) {
		return Candidate{}, fmt.Errorf("proposal repository changed: %w", ErrDrift)
	}
	_, instruction, err := resolveInstruction(repository, skill.InstructionPath)
	if err != nil {
		return Candidate{}, err
	}
	if err := commitExists(ctx, repository, proposal.BaseCommit); err != nil {
		return Candidate{}, err
	}
	if err := commitExists(ctx, repository, proposal.CandidateCommit); err != nil {
		return Candidate{}, err
	}
	diff, err := git(ctx, repository, "diff", "--no-ext-diff", proposal.BaseCommit, proposal.CandidateCommit, "--", instruction)
	if err != nil {
		return Candidate{}, err
	}
	return Candidate{
		SkillID:         skill.ID,
		ClusterID:       cluster.ID,
		Fingerprint:     MarkerFingerprint(cluster.Fingerprint),
		Lesson:          normalizeLesson(cluster.Lesson),
		RepositoryPath:  repository,
		InstructionPath: instruction,
		WorktreePath:    proposal.WorktreePath,
		Branch:          proposal.Branch,
		BaseCommit:      proposal.BaseCommit,
		CandidateCommit: proposal.CandidateCommit,
		Diff:            diff,
		RequiresHumanApproval: sensitiveChange.MatchString(cluster.Summary+"\n"+cluster.Lesson) ||
			promptInjection.MatchString(cluster.Summary+"\n"+cluster.Lesson),
		CreatedAt: proposal.CreatedAt,
	}, nil
}
