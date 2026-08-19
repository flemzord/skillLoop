package improvement

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Evaluate rehydrates the exact baseline and candidate commits. Structural
// checks remain useful without a Runner, while promotion separately requires
// an external baseline failure and candidate success from these exact commits.
func (service Service) Evaluate(ctx context.Context, candidate Candidate) (Evaluation, error) {
	evaluation := Evaluation{
		SkillID:               candidate.SkillID,
		ClusterID:             candidate.ClusterID,
		BaseCommit:            candidate.BaseCommit,
		CandidateCommit:       candidate.CandidateCommit,
		RequiresHumanApproval: candidate.RequiresHumanApproval,
		EvaluatedAt:           time.Now().UTC(),
	}
	addCheck := func(name string, passed bool, detail string) {
		evaluation.Checks = append(evaluation.Checks, Check{Name: name, Passed: passed, Detail: detail})
	}

	repository, err := resolveRepository(ctx, candidate.RepositoryPath)
	if err != nil {
		return evaluation, err
	}
	if err := commitExists(ctx, repository, candidate.BaseCommit); err != nil {
		return evaluation, err
	}
	if err := commitExists(ctx, repository, candidate.CandidateCommit); err != nil {
		return evaluation, err
	}
	parent, err := git(ctx, repository, "rev-parse", candidate.CandidateCommit+"^")
	if err != nil || parent != candidate.BaseCommit {
		return evaluation, fmt.Errorf("candidate parent differs from baseline: %w", ErrDrift)
	}
	if candidate.InstructionPath == "" || filepath.IsAbs(candidate.InstructionPath) || filepath.ToSlash(filepath.Clean(candidate.InstructionPath)) != candidate.InstructionPath {
		return evaluation, fmt.Errorf("invalid persisted instruction path: %w", ErrUnsafePath)
	}
	if filepath.Base(candidate.InstructionPath) != "SKILL.md" || strings.HasPrefix(candidate.InstructionPath, "../") {
		return evaluation, fmt.Errorf("invalid persisted instruction path: %w", ErrUnsafePath)
	}
	if err := verifyTrackedInstruction(ctx, repository, candidate.BaseCommit, candidate.InstructionPath); err != nil {
		return evaluation, err
	}
	if err := verifyTrackedInstruction(ctx, repository, candidate.CandidateCommit, candidate.InstructionPath); err != nil {
		return evaluation, err
	}

	changed, err := git(ctx, repository, "diff", "--name-only", candidate.BaseCommit, candidate.CandidateCommit)
	if err != nil {
		return evaluation, err
	}
	addCheck("only-skill-md", changed == candidate.InstructionPath, changed)
	diff, err := git(ctx, repository, "diff", "--no-ext-diff", candidate.BaseCommit, candidate.CandidateCommit, "--", candidate.InstructionPath)
	if err != nil {
		return evaluation, err
	}
	diffErr := validateDiff(diff)
	addCheck("diff-limits", diffErr == nil && diff == candidate.Diff, detailFor(diffErr, diff == candidate.Diff, "diff matches candidate"))
	addCheck("no-secrets", !containsSecret(diff), "candidate diff scanned")

	baselineBytes, err := gitBytes(ctx, repository, "show", candidate.BaseCommit+":"+candidate.InstructionPath)
	if err != nil {
		return evaluation, err
	}
	candidateBytes, err := gitBytes(ctx, repository, "show", candidate.CandidateCommit+":"+candidate.InstructionPath)
	if err != nil {
		return evaluation, err
	}
	baselineContents := string(baselineBytes)
	candidateContents := string(candidateBytes)
	baselineStructureErr := validateStructure(baselineContents)
	candidateStructureErr := validateStructure(candidateContents)
	addCheck("baseline-structure", baselineStructureErr == nil, errorDetail(baselineStructureErr))
	addCheck("candidate-structure", candidateStructureErr == nil, errorDetail(candidateStructureErr))

	expected, applyErr := applyManagedBlock([]byte(baselineContents), candidate.Fingerprint, candidate.Lesson)
	baselineFails := applyErr == nil && string(expected) != baselineContents
	addCheck("baseline-derived-case-fails", baselineFails, "baseline does not contain the learned guidance")
	derivedPasses := applyErr == nil && string(expected) == candidateContents
	addCheck("candidate-derived-case-passes", derivedPasses, "candidate contains exactly the learned guidance")
	reapplied, idempotenceErr := applyManagedBlock([]byte(candidateContents), candidate.Fingerprint, candidate.Lesson)
	idempotent := idempotenceErr == nil && string(reapplied) == candidateContents
	addCheck("idempotent", idempotent, errorDetail(idempotenceErr))

	if len(service.Runner.Argv) > 0 {
		baselineRun, candidateRun, runErr := service.runExactPair(ctx, repository, candidate)
		if runErr != nil {
			return evaluation, runErr
		}
		evaluation.BaselineRun = &baselineRun
		evaluation.CandidateRun = &candidateRun
		baselineExpectedFailure := baselineRun.ExitCode != 0 && !baselineRun.TimedOut
		candidateExpectedPass := candidateRun.ExitCode == 0 && !candidateRun.TimedOut
		addCheck("external-baseline-fails", baselineExpectedFailure, fmt.Sprintf("exit=%d", baselineRun.ExitCode))
		addCheck("external-candidate-passes", candidateExpectedPass, fmt.Sprintf("exit=%d", candidateRun.ExitCode))
	}

	evaluation.Passed = true
	for _, check := range evaluation.Checks {
		if !check.Passed {
			evaluation.Passed = false
			break
		}
	}
	return evaluation, nil
}

func (service Service) runExactPair(ctx context.Context, repository string, candidate Candidate) (RunResult, RunResult, error) {
	if service.Runner.Argv[0] == "" {
		return RunResult{}, RunResult{}, errors.New("external runner executable is empty")
	}
	stateDir, err := filepath.Abs(service.StateDir)
	if err != nil || service.StateDir == "" {
		return RunResult{}, RunResult{}, errors.New("state directory is required")
	}
	parent := filepath.Join(stateDir, "evaluations")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return RunResult{}, RunResult{}, fmt.Errorf("create evaluation directory: %w", err)
	}
	root, err := os.MkdirTemp(parent, "pair-")
	if err != nil {
		return RunResult{}, RunResult{}, fmt.Errorf("create evaluation pair: %w", err)
	}
	baselinePath := filepath.Join(root, "baseline")
	candidatePath := filepath.Join(root, "candidate")
	cleanup := func() {
		_, _ = git(context.Background(), repository, "worktree", "remove", "--force", baselinePath)
		_, _ = git(context.Background(), repository, "worktree", "remove", "--force", candidatePath)
		_ = os.Remove(root)
	}
	defer cleanup()

	if _, err := git(ctx, repository, "worktree", "add", "--detach", baselinePath, candidate.BaseCommit); err != nil {
		return RunResult{}, RunResult{}, err
	}
	if _, err := git(ctx, repository, "worktree", "add", "--detach", candidatePath, candidate.CandidateCommit); err != nil {
		return RunResult{}, RunResult{}, err
	}
	baselineRun, err := service.runOne(ctx, baselinePath, candidate.BaseCommit)
	if err != nil {
		return RunResult{}, RunResult{}, err
	}
	candidateRun, err := service.runOne(ctx, candidatePath, candidate.CandidateCommit)
	if err != nil {
		return RunResult{}, RunResult{}, err
	}
	return baselineRun, candidateRun, nil
}

func (service Service) runOne(ctx context.Context, directory, revision string) (RunResult, error) {
	timeout := service.Runner.Timeout
	if timeout <= 0 {
		timeout = defaultRunnerTimeout
	}
	limit := service.Runner.OutputLimit
	if limit <= 0 {
		limit = defaultOutputLimit
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	output := &cappedBuffer{limit: limit}
	command := exec.CommandContext(runCtx, service.Runner.Argv[0], service.Runner.Argv[1:]...)
	command.Dir = directory
	command.Stdout = output
	command.Stderr = output
	started := time.Now()
	runErr := command.Run()
	duration := time.Since(started)
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	if runErr != nil && exitCode(runErr) < 0 && !timedOut {
		return RunResult{}, fmt.Errorf("run external evaluator at %s: %w", revision, runErr)
	}
	return RunResult{
		Revision:  revision,
		ExitCode:  exitCode(runErr),
		Output:    output.String(),
		Truncated: output.truncated,
		TimedOut:  timedOut,
		Duration:  duration,
	}, nil
}

type cappedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (buffer *cappedBuffer) Write(contents []byte) (int, error) {
	originalLength := len(contents)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining <= 0 {
		buffer.truncated = buffer.truncated || originalLength > 0
		return originalLength, nil
	}
	if len(contents) > remaining {
		contents = contents[:remaining]
		buffer.truncated = true
	}
	_, _ = buffer.buffer.Write(contents)
	return originalLength, nil
}

func (buffer *cappedBuffer) String() string {
	return buffer.buffer.String()
}

func errorDetail(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

func detailFor(err error, same bool, success string) string {
	if err != nil {
		return err.Error()
	}
	if !same {
		return "diff differs from prepared candidate"
	}
	return success
}
