//go:build darwin || linux

package improvement

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

type processGroupTerminator struct {
	command             *exec.Cmd
	kill                func(*exec.Cmd) error
	readyReader         *os.File
	readyWriter         *os.File
	containmentVerified bool
	killAfterWait       bool
	once                sync.Once
	err                 error
}

func configureProcessGroup(command *exec.Cmd) *processGroupTerminator {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	terminator := &processGroupTerminator{command: command, kill: killProcessGroup, killAfterWait: true}
	command.Cancel = terminator.terminate
	return terminator
}

func configureContainedProcess(command *exec.Cmd) (*processGroupTerminator, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create evaluator containment handshake: %w", err)
	}
	command.ExtraFiles = append(command.ExtraFiles, writer)
	terminator := configureProcessGroup(command)
	terminator.readyReader = reader
	terminator.readyWriter = writer
	return terminator, nil
}

func (terminator *processGroupTerminator) confirmContainment() error {
	if terminator.readyReader == nil && terminator.readyWriter == nil {
		return errors.New("evaluator containment handshake is unavailable")
	}
	if terminator.readyReader == nil || terminator.readyWriter == nil {
		return errors.New("evaluator containment handshake is unavailable")
	}
	_ = terminator.readyWriter.Close()
	terminator.readyWriter = nil
	var signal [1]byte
	_, err := io.ReadFull(terminator.readyReader, signal[:])
	if err != nil || signal[0] != 1 {
		_ = terminator.readyReader.Close()
		terminator.readyReader = nil
		return errors.New("evaluator containment setup failed")
	}
	// The bootstrap marks the descriptor close-on-exec. EOF after the ready
	// byte therefore proves that the contained evaluator was actually exec'd;
	// a second byte reports an exec failure instead of manufacturing a result.
	var failure [1]byte
	count, execErr := terminator.readyReader.Read(failure[:])
	_ = terminator.readyReader.Close()
	terminator.readyReader = nil
	if count != 0 || !errors.Is(execErr, io.EOF) {
		return errors.New("evaluator executable could not start inside containment")
	}
	terminator.containmentVerified = true
	return nil
}

func (terminator *processGroupTerminator) closeContainmentHandshake() {
	if terminator.readyWriter != nil {
		_ = terminator.readyWriter.Close()
		terminator.readyWriter = nil
	}
	if terminator.readyReader != nil {
		_ = terminator.readyReader.Close()
		terminator.readyReader = nil
	}
}

func signalEvaluatorContainment() (*os.File, error) {
	if err := unix.Fchdir(4); err != nil {
		return nil, fmt.Errorf("enter authenticated evaluator worktree: %w", err)
	}
	ready := os.NewFile(3, "skillloop-evaluator-containment")
	if ready == nil {
		return nil, errors.New("evaluator containment descriptor is unavailable")
	}
	if _, err := ready.Write([]byte{1}); err != nil {
		_ = ready.Close()
		return nil, err
	}
	syscall.CloseOnExec(3)
	// Contained evaluators receive their already-open worktree as descriptor 4
	// so the initial chdir cannot be redirected by a replaced pathname ancestor.
	// Repository code does not retain that descriptor after the bootstrap exec.
	syscall.CloseOnExec(4)
	return ready, nil
}

func signalEvaluatorExecFailure(ready *os.File) {
	if ready == nil {
		return
	}
	_, _ = ready.Write([]byte{0})
	_ = ready.Close()
}

func withoutEnvironmentKey(environment []string, key string) []string {
	clean := make([]string, 0, len(environment))
	prefix := key + "="
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			clean = append(clean, entry)
		}
	}
	return clean
}

func bootstrapFailure(err error) {
	_, _ = fmt.Fprintln(os.Stderr, "skillloop evaluator bootstrap:", err)
	os.Exit(127)
}

func (terminator *processGroupTerminator) terminate() error {
	terminator.once.Do(func() {
		terminator.err = terminator.kill(terminator.command)
	})
	return terminator.err
}

func (terminator *processGroupTerminator) terminateAfterWait() error {
	if !terminator.killAfterWait {
		return os.ErrProcessDone
	}
	return terminator.terminate()
}

func killProcessGroup(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
