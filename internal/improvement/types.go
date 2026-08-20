// Package improvement creates, evaluates, promotes, and rolls back isolated
// skill improvements. It deliberately does not persist its return values; the
// pipeline owns that responsibility.
package improvement

import (
	"errors"
	"fmt"
	"time"

	"github.com/flemzord/skillloop/internal/domain"
)

const (
	defaultRunnerTimeout = 30 * time.Second
	defaultOutputLimit   = 64 * 1024
	runnerWaitDelay      = time.Second

	maxWorktreeFiles  = 100_000
	maxWorktreeBytes  = 1024 * 1024 * 1024
	maxSkillFileBytes = 8 * 1024 * 1024

	maxReleaseArchiveBytes = 64 * 1024 * 1024
	maxReleaseMembers      = 10_000
	maxReleaseMemberBytes  = maxSkillFileBytes
	maxReleaseTotalBytes   = 32 * 1024 * 1024
)

var (
	ErrUnsafePath                 = errors.New("unsafe skill path")
	ErrUnsafeChange               = errors.New("security-sensitive skill change")
	ErrDiffLimit                  = errors.New("candidate diff exceeds limits")
	ErrResourceLimit              = errors.New("resource limit exceeded")
	ErrDrift                      = errors.New("git revision drift")
	ErrEvaluationFailed           = errors.New("candidate did not pass evaluation")
	ErrExternalEvaluationRequired = errors.New("external baseline/candidate evaluation is required for promotion")
	ErrNoRelease                  = errors.New("skill has no current release")
)

// Service is safe for concurrent use. StateDir contains disposable worktrees
// and immutable releases. Runner is optional for inspection, but promotion
// requires its exact baseline/candidate results.
type Service struct {
	StateDir string
	Runner   Runner
}

// Runner describes an optional external derived-case evaluator. Argv is
// executed directly (never through a shell) once against each exact revision.
// It must fail for the baseline and pass for the candidate.
type Runner struct {
	Argv        []string
	Timeout     time.Duration
	OutputLimit int
}

type Candidate struct {
	SkillID               string
	ClusterID             string
	Fingerprint           string
	Lesson                string
	CardKind              domain.CardKind
	RepositoryPath        string
	InstructionPath       string
	WorktreePath          string
	Branch                string
	BaseCommit            string
	CandidateCommit       string
	Diff                  string
	RequiresHumanApproval bool
	CreatedAt             time.Time
}

type Check struct {
	Name   string
	Passed bool
	Detail string
}

type RunResult struct {
	Revision  string
	ExitCode  int
	Output    string
	Truncated bool
	TimedOut  bool
	Duration  time.Duration
}

type Evaluation struct {
	SkillID               string
	ClusterID             string
	BaseCommit            string
	CandidateCommit       string
	Checks                []Check
	BaselineRun           *RunResult
	CandidateRun          *RunResult
	RequiresHumanApproval bool
	Passed                bool
	EvaluatedAt           time.Time
}

// ValidateExternalRunPair verifies that an external evaluator completed
// against the exact baseline and candidate revisions. It deliberately does not
// judge the exit codes so monitoring can distinguish a completed regression
// from missing or inconclusive evaluator infrastructure.
func ValidateExternalRunPair(evaluation Evaluation) error {
	if evaluation.BaselineRun == nil || evaluation.CandidateRun == nil ||
		evaluation.BaselineRun.Revision == "" || evaluation.CandidateRun.Revision == "" {
		return ErrExternalEvaluationRequired
	}
	if evaluation.BaselineRun.Revision != evaluation.BaseCommit || evaluation.CandidateRun.Revision != evaluation.CandidateCommit {
		return fmt.Errorf("external evaluation revisions do not match candidate: %w", ErrDrift)
	}
	if evaluation.BaselineRun.TimedOut || evaluation.CandidateRun.TimedOut {
		return fmt.Errorf("external baseline/candidate evaluation did not complete: %w", ErrEvaluationFailed)
	}
	return nil
}

// ValidatePromotionProof requires a completed comparative evaluation in which
// the baseline fails and the candidate passes. Structural checks alone are
// useful for inspection, but are never sufficient to promote a skill.
func ValidatePromotionProof(evaluation Evaluation) error {
	if err := ValidateExternalRunPair(evaluation); err != nil {
		return err
	}
	if evaluation.BaselineRun.ExitCode == 0 || evaluation.CandidateRun.ExitCode != 0 {
		return fmt.Errorf("external evaluator must fail the baseline and pass the candidate: %w", ErrEvaluationFailed)
	}
	return nil
}

// Approval is the human or policy decision handed to Promote. Both revisions
// are mandatory so stale approvals cannot be applied to a newer candidate.
type Approval struct {
	BaseCommit      string
	CandidateCommit string
}

type Promotion struct {
	SkillID        string
	CurrentCommit  string
	PreviousCommit string
	ReleasePath    string
	CurrentLink    string
	RolledBack     bool
	PromotedAt     time.Time
}

type Release struct {
	SkillID string
	Commit  string
	Path    string
}
