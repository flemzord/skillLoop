package improvement

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/flemzord/skillloop/internal/sanitize"
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

	changed, err := git(ctx, repository, "diff", "--no-ext-diff", "--no-textconv", "--name-only", candidate.BaseCommit, candidate.CandidateCommit)
	if err != nil {
		return evaluation, err
	}
	addCheck("only-skill-md", changed == candidate.InstructionPath, changed)
	diff, err := git(ctx, repository, "diff", "--no-ext-diff", "--no-textconv", candidate.BaseCommit, candidate.CandidateCommit, "--", candidate.InstructionPath)
	if err != nil {
		return evaluation, err
	}
	diffErr := validateDiff(diff)
	addCheck("diff-limits", diffErr == nil && diff == candidate.Diff, detailFor(diffErr, diff == candidate.Diff, "diff matches candidate"))
	addCheck("no-secrets", !sanitize.ContainsSecret(diff), "candidate diff scanned")

	baselineBytes, err := gitBytesLimit(ctx, repository, maxSkillFileBytes, "show", candidate.BaseCommit+":"+candidate.InstructionPath)
	if err != nil {
		return evaluation, err
	}
	candidateBytes, err := gitBytesLimit(ctx, repository, maxSkillFileBytes, "show", candidate.CandidateCommit+":"+candidate.InstructionPath)
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
	evaluations, err := openStateDirectory(service.StateDir, "evaluations")
	if err != nil {
		return RunResult{}, RunResult{}, err
	}
	defer func() { _ = evaluations.Close() }()
	pairName, root, err := createStateChild(evaluations, "pair-")
	if err != nil {
		return RunResult{}, RunResult{}, fmt.Errorf("create evaluation pair: %w", err)
	}
	pairFile, err := openStateChild(evaluations, pairName)
	if err != nil {
		_ = unix.Unlinkat(int(evaluations.file.Fd()), pairName, unix.AT_REMOVEDIR)
		return RunResult{}, RunResult{}, fmt.Errorf("open evaluation pair: %w", err)
	}
	pair := &stateDirectory{path: root, file: pairFile}
	defer func() { _ = pair.Close() }()
	baselinePath := filepath.Join(root, "baseline")
	candidatePath := filepath.Join(root, "candidate")
	var baselineDirectory, candidateDirectory *os.File
	cleanup := func() {
		if baselineDirectory != nil {
			_, _ = gitAt(context.Background(), baselineDirectory, "worktree", "remove", "--force", ".")
			_ = baselineDirectory.Close()
		}
		if candidateDirectory != nil {
			_, _ = gitAt(context.Background(), candidateDirectory, "worktree", "remove", "--force", ".")
			_ = candidateDirectory.Close()
		}
		_ = unix.Unlinkat(int(evaluations.file.Fd()), pairName, unix.AT_REMOVEDIR)
	}
	defer cleanup()

	if err := preflightWorktree(ctx, repository, candidate.BaseCommit); err != nil {
		return RunResult{}, RunResult{}, err
	}
	if err := preflightWorktree(ctx, repository, candidate.CandidateCommit); err != nil {
		return RunResult{}, RunResult{}, err
	}
	baselineDirectory, _, err = addWorktreeAt(ctx, repository, pair, "baseline", baselinePath, candidate.BaseCommit, "")
	if err != nil {
		return RunResult{}, RunResult{}, err
	}
	candidateDirectory, _, err = addWorktreeAt(ctx, repository, pair, "candidate", candidatePath, candidate.CandidateCommit, "")
	if err != nil {
		return RunResult{}, RunResult{}, err
	}
	baselineRun, err := service.runOneAt(ctx, baselineDirectory, candidate.BaseCommit)
	if err != nil {
		return RunResult{}, RunResult{}, err
	}
	candidateRun, err := service.runOneAt(ctx, candidateDirectory, candidate.CandidateCommit)
	if err != nil {
		return RunResult{}, RunResult{}, err
	}
	if err := verifyStateChildIdentity(evaluations, pairName, pair.file); err != nil {
		return RunResult{}, RunResult{}, fmt.Errorf("verify evaluation pair namespace: %w", err)
	}
	if err := verifyStateChildIdentity(pair, "baseline", baselineDirectory); err != nil {
		return RunResult{}, RunResult{}, fmt.Errorf("verify baseline evaluation namespace: %w", err)
	}
	if err := verifyStateChildIdentity(pair, "candidate", candidateDirectory); err != nil {
		return RunResult{}, RunResult{}, fmt.Errorf("verify candidate evaluation namespace: %w", err)
	}
	if err := verifyStateChildIdentity(evaluations, pairName, pair.file); err != nil {
		return RunResult{}, RunResult{}, fmt.Errorf("reverify evaluation pair namespace: %w", err)
	}
	return baselineRun, candidateRun, nil
}

func (service Service) runOne(ctx context.Context, directory, revision string) (RunResult, error) {
	fd, err := unix.Open(directory, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return RunResult{}, fmt.Errorf("open external evaluator directory: %w", err)
	}
	file := os.NewFile(uintptr(fd), directory)
	if file == nil {
		_ = unix.Close(fd)
		return RunResult{}, errors.New("open external evaluator directory: invalid file descriptor")
	}
	defer func() { _ = file.Close() }()
	return service.runOneAt(ctx, file, revision)
}

func (service Service) runOneAt(ctx context.Context, directory *os.File, revision string) (RunResult, error) {
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
	command, terminator, err := containedEvaluatorCommand(runCtx, service.Runner.Argv)
	if err != nil {
		return RunResult{}, fmt.Errorf("contain external evaluator at %s: %w", revision, err)
	}
	if directory == nil {
		terminator.closeContainmentHandshake()
		return RunResult{}, errors.New("external evaluator directory is unavailable")
	}
	// configureContainedProcess reserves child descriptor 3 for its handshake.
	// The worktree directory is descriptor 4 and becomes the bootstrap's cwd;
	// the bootstrap marks it close-on-exec before starting repository code.
	command.ExtraFiles = append(command.ExtraFiles, directory)
	reader, writer, err := os.Pipe()
	if err != nil {
		terminator.closeContainmentHandshake()
		return RunResult{}, fmt.Errorf("create evaluator output pipe: %w", err)
	}
	command.Stdout = writer
	command.Stderr = writer
	readDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(output, reader)
		readDone <- copyErr
	}()
	started := time.Now()
	if err := command.Start(); err != nil {
		terminator.closeContainmentHandshake()
		_ = writer.Close()
		_ = reader.Close()
		<-readDone
		return RunResult{}, fmt.Errorf("start external evaluator at %s: %w", revision, err)
	}
	_ = writer.Close()
	if err := terminator.confirmContainment(); err != nil {
		_ = terminator.terminate()
		_ = command.Wait()
		_ = reader.Close()
		<-readDone
		return RunResult{}, fmt.Errorf("confirm external evaluator containment at %s: %w", revision, err)
	}
	runErr := command.Wait()
	// Wait returns as soon as the evaluator leader exits because stdout/stderr
	// are explicit files. Terminate the platform-contained evaluator after a
	// normal exit as well so no allowed process can outlive this call.
	terminationErr := terminator.terminateAfterWait()
	var readErr error
	select {
	case readErr = <-readDone:
	case <-time.After(runnerWaitDelay):
		_ = reader.Close()
		readErr = <-readDone
	}
	_ = reader.Close()
	duration := time.Since(started)
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	if readErr != nil && !errors.Is(readErr, os.ErrClosed) {
		return RunResult{}, fmt.Errorf("read external evaluator output at %s: %w", revision, readErr)
	}
	if terminationErr != nil && !errors.Is(terminationErr, os.ErrProcessDone) {
		return RunResult{}, fmt.Errorf("terminate contained external evaluator at %s: %w", revision, terminationErr)
	}
	if runErr != nil && exitCode(runErr) < 0 && !timedOut {
		return RunResult{}, fmt.Errorf("run external evaluator at %s: %w", revision, runErr)
	}
	return RunResult{
		Revision:            revision,
		ExitCode:            exitCode(runErr),
		Output:              output.String(),
		Truncated:           output.truncated,
		TimedOut:            timedOut,
		ContainmentVerified: terminator.containmentVerified,
		Duration:            duration,
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
