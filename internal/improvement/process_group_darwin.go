//go:build darwin

package improvement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

const (
	darwinEvaluatorBootstrap = "SKILLLOOP_INTERNAL_DARWIN_EVALUATOR_BOOTSTRAP"
	darwinSandboxExecutable  = "/usr/bin/sandbox-exec"
	darwinSandboxProfile     = "(version 1)(allow default)(deny process-fork)"
	maxDarwinBootstrapBytes  = 64 * 1024
)

// init re-enters the current binary after sandbox-exec has applied Seatbelt.
// It proves that process creation is denied before replacing itself with the
// configured evaluator. A single-process evaluator can still use arbitrary
// libraries and exec itself, but it cannot create an escaping descendant.
func init() {
	encoded := os.Getenv(darwinEvaluatorBootstrap)
	if encoded == "" {
		return
	}
	if len(encoded) > maxDarwinBootstrapBytes {
		bootstrapFailure(errors.New("evaluator argv exceeds bootstrap limit"))
	}
	var argv []string
	if err := json.Unmarshal([]byte(encoded), &argv); err != nil || len(argv) == 0 || argv[0] == "" {
		bootstrapFailure(errors.New("invalid evaluator bootstrap argv"))
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		bootstrapFailure(fmt.Errorf("resolve evaluator executable: %w", err))
	}
	if err := verifyDarwinProcessCreationDenied(); err != nil {
		bootstrapFailure(err)
	}
	ready, err := signalEvaluatorContainment()
	if err != nil {
		bootstrapFailure(fmt.Errorf("signal evaluator containment: %w", err))
	}
	environment := withoutEnvironmentKey(os.Environ(), darwinEvaluatorBootstrap)
	if err := syscall.Exec(path, argv, environment); err != nil {
		signalEvaluatorExecFailure(ready)
		bootstrapFailure(fmt.Errorf("exec evaluator: %w", err))
	}
}

func containedEvaluatorCommand(ctx context.Context, argv []string) (*exec.Cmd, *processGroupTerminator, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, nil, errors.New("external runner executable is empty")
	}
	runner, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, nil, fmt.Errorf("external runner executable %q is unavailable: %w", argv[0], err)
	}
	if info, err := os.Stat(darwinSandboxExecutable); err != nil || !info.Mode().IsRegular() {
		return nil, nil, errors.New("macOS evaluator containment is unavailable")
	}
	exactArgv := append([]string{runner}, argv[1:]...)
	encoded, err := json.Marshal(exactArgv)
	if err != nil || len(encoded) > maxDarwinBootstrapBytes {
		return nil, nil, errors.New("external runner argv exceeds containment limit")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("resolve evaluator bootstrap: %w", err)
	}
	command := exec.CommandContext(ctx, darwinSandboxExecutable, "-p", darwinSandboxProfile, "--", executable)
	command.Env = append(withoutEnvironmentKey(os.Environ(), darwinEvaluatorBootstrap), darwinEvaluatorBootstrap+"="+string(encoded))
	terminator, err := configureContainedProcess(command)
	if err != nil {
		return nil, nil, err
	}
	// Seatbelt prevents descendants. Kill the exact process rather than only
	// its group so a single-process evaluator cannot evade cancellation by
	// changing its own session or process group. Never signal that PID after
	// Wait, when the numeric identifier could already have been reused.
	terminator.kill = killEvaluatorProcess
	terminator.killAfterWait = false
	return command, terminator, nil
}

func verifyDarwinProcessCreationDenied() error {
	process, err := os.StartProcess("/usr/bin/true", []string{"true"}, &os.ProcAttr{
		Env:   os.Environ(),
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err == nil {
		_, _ = process.Wait()
		return errors.New("macOS evaluator sandbox unexpectedly permits process creation")
	}
	if !errors.Is(err, syscall.EPERM) && !strings.Contains(strings.ToLower(err.Error()), "operation not permitted") {
		return fmt.Errorf("verify macOS evaluator process isolation: %w", err)
	}
	return nil
}

func killEvaluatorProcess(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(command.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
