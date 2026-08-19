package improvement

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Cleanup removes only the generated worktree and its matching candidate
// branch. It refuses persisted paths or refs outside SkillLoop's namespace.
func (service Service) Cleanup(ctx context.Context, candidate Candidate) error {
	repository, err := resolveRepository(ctx, candidate.RepositoryPath)
	if err != nil {
		return err
	}
	stateDir, err := filepath.Abs(service.StateDir)
	if err != nil || service.StateDir == "" {
		return fmt.Errorf("state directory is required: %w", ErrUnsafePath)
	}
	worktreesDir := filepath.Join(stateDir, "worktrees")
	worktree, err := filepath.Abs(candidate.WorktreePath)
	if err != nil {
		return fmt.Errorf("resolve candidate worktree: %w", err)
	}
	relative, err := filepath.Rel(worktreesDir, worktree)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("candidate worktree is outside state directory: %w", ErrUnsafePath)
	}
	if !strings.HasPrefix(candidate.Branch, "skillloop/") || strings.Contains(candidate.Branch, "..") {
		return fmt.Errorf("candidate branch is outside SkillLoop namespace: %w", ErrUnsafePath)
	}
	ref, refErr := git(ctx, repository, "rev-parse", "refs/heads/"+candidate.Branch)
	if refErr == nil && ref != candidate.CandidateCommit {
		return fmt.Errorf("candidate branch moved from approved commit: %w", ErrDrift)
	}
	if _, statErr := os.Lstat(worktree); statErr == nil {
		if _, err := git(ctx, repository, "worktree", "remove", "--force", worktree); err != nil {
			return err
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("inspect candidate worktree: %w", statErr)
	} else {
		_, _ = git(ctx, repository, "worktree", "prune")
	}
	if refErr == nil {
		if _, err := git(ctx, repository, "branch", "-D", candidate.Branch); err != nil {
			return err
		}
	}
	return nil
}

// Reject is an explicit pipeline-friendly alias for Cleanup.
func (service Service) Reject(ctx context.Context, candidate Candidate) error {
	return service.Cleanup(ctx, candidate)
}
