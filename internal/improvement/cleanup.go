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
	worktreeParent, err := openStateDirectory(service.StateDir, "worktrees", safeName(candidate.SkillID))
	if err != nil {
		return err
	}
	defer func() { _ = worktreeParent.Close() }()
	worktree, err := filepath.Abs(candidate.WorktreePath)
	if err != nil {
		return fmt.Errorf("resolve candidate worktree: %w", err)
	}
	relative, err := filepath.Rel(worktreeParent.path, worktree)
	if err != nil || !safeStateComponent(relative) || filepath.IsAbs(relative) {
		return fmt.Errorf("candidate worktree is outside state directory: %w", ErrUnsafePath)
	}
	if !strings.HasPrefix(candidate.Branch, "skillloop/") || strings.Contains(candidate.Branch, "..") {
		return fmt.Errorf("candidate branch is outside SkillLoop namespace: %w", ErrUnsafePath)
	}
	ref, refErr := git(ctx, repository, "rev-parse", "refs/heads/"+candidate.Branch)
	if refErr == nil && ref != candidate.CandidateCommit {
		return fmt.Errorf("candidate branch moved from approved commit: %w", ErrDrift)
	}
	directory, openErr := openStateChild(worktreeParent, relative)
	switch {
	case openErr == nil:
		defer func() { _ = directory.Close() }()
		if err := verifyExactWorktreeAt(ctx, repository, worktree, directory, candidate.CandidateCommit, candidate.Branch); err != nil {
			return fmt.Errorf("authenticate candidate worktree before cleanup: %w", err)
		}
		if _, err := gitAt(ctx, directory, "worktree", "remove", "--force", "."); err != nil {
			return err
		}
	case !os.IsNotExist(openErr):
		return fmt.Errorf("inspect candidate worktree: %w", openErr)
	default:
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
