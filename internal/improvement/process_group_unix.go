//go:build darwin || linux

package improvement

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

type processGroupTerminator struct {
	command *exec.Cmd
	kill    func(*exec.Cmd) error
	once    sync.Once
	err     error
}

func configureProcessGroup(command *exec.Cmd) *processGroupTerminator {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	terminator := &processGroupTerminator{command: command, kill: killProcessGroup}
	command.Cancel = terminator.terminate
	return terminator
}

func (terminator *processGroupTerminator) terminate() error {
	terminator.once.Do(func() {
		terminator.err = terminator.kill(terminator.command)
	})
	return terminator.err
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
