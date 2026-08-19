// Package improvement creates, evaluates, promotes, and rolls back isolated
// skill improvements. It deliberately does not persist its return values; the
// pipeline owns that responsibility.
package improvement

import (
	"errors"
	"time"
)

const (
	defaultRunnerTimeout = 30 * time.Second
	defaultOutputLimit   = 64 * 1024
)

var (
	ErrUnsafePath       = errors.New("unsafe skill path")
	ErrUnsafeChange     = errors.New("security-sensitive skill change")
	ErrDiffLimit        = errors.New("candidate diff exceeds limits")
	ErrDrift            = errors.New("git revision drift")
	ErrEvaluationFailed = errors.New("candidate did not pass evaluation")
	ErrNoRelease        = errors.New("skill has no current release")
)

// Service is safe for concurrent use. StateDir contains disposable worktrees
// and immutable releases. Runner is optional.
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
