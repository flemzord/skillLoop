package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const (
	skillGitTimeout     = 5 * time.Second
	skillGitOutputLimit = 64 << 10
)

var errSkillGitOutputLimit = errors.New("git output exceeds limit")

type skillGitBuffer struct {
	contents bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *skillGitBuffer) Write(contents []byte) (int, error) {
	remaining := buffer.limit - buffer.contents.Len()
	if remaining <= 0 {
		buffer.exceeded = true
		return len(contents), nil
	}
	if len(contents) > remaining {
		_, _ = buffer.contents.Write(contents[:remaining])
		buffer.exceeded = true
		return len(contents), nil
	}
	return buffer.contents.Write(contents)
}

func (buffer *skillGitBuffer) Bytes() []byte {
	return buffer.contents.Bytes()
}

func (buffer *skillGitBuffer) String() string {
	return buffer.contents.String()
}

// runSkillGit executes only the two read-only Git queries used during skill
// registration. It discards inherited Git configuration environment, disables
// local command-bearing configuration at the highest-precedence command scope,
// and bounds both runtime and output.
func runSkillGit(ctx context.Context, repository string, arguments ...string) ([]byte, error) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("locate Git: %w", err)
	}

	commandArguments := []string{
		"--no-optional-locks",
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "credential.helper=",
		"-c", "core.askPass=",
		"-c", "core.editor=false",
		"-c", "sequence.editor=false",
		"-c", "core.sshCommand=false",
		"-c", "pager.rev-parse=false",
		"-c", "pager.ls-files=false",
		"-C", repository,
	}
	commandArguments = append(commandArguments, arguments...)

	commandContext, cancel := context.WithTimeout(ctx, skillGitTimeout)
	defer cancel()
	command := exec.CommandContext(commandContext, gitPath, commandArguments...)
	command.Env = []string{
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_CONFIG_COUNT=0",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_PAGER=cat",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	command.WaitDelay = 250 * time.Millisecond

	stdout := &skillGitBuffer{limit: skillGitOutputLimit}
	stderr := &skillGitBuffer{limit: skillGitOutputLimit}
	command.Stdout = stdout
	command.Stderr = stderr
	runError := command.Run()
	if contextError := commandContext.Err(); contextError != nil {
		return nil, fmt.Errorf("git query timed out or was canceled: %w", contextError)
	}
	if stdout.exceeded || stderr.exceeded {
		return nil, errSkillGitOutputLimit
	}
	if runError != nil {
		detail := strings.TrimSpace(terminalSafe(stderr.String()))
		if detail == "" {
			return nil, fmt.Errorf("git query failed: %w", runError)
		}
		return nil, fmt.Errorf("git query failed: %s: %w", detail, runError)
	}
	return append([]byte(nil), stdout.Bytes()...), nil
}
