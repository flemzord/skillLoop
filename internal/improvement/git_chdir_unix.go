//go:build darwin || linux

package improvement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	gitDirectoryBootstrap    = "SKILLLOOP_INTERNAL_GIT_DIRECTORY_BOOTSTRAP"
	maxGitBootstrapArgvBytes = 64 * 1024
)

type gitBootstrapCommand struct {
	Path string   `json:"path"`
	Argv []string `json:"argv"`
}

// init re-enters the current binary to fchdir to an authenticated directory
// before Git starts. Go's exec.Cmd resolves Dir before installing ExtraFiles,
// which cannot safely express this operation on both macOS and Linux.
func init() {
	encoded := os.Getenv(gitDirectoryBootstrap)
	if encoded == "" {
		return
	}
	if len(encoded) > maxGitBootstrapArgvBytes {
		bootstrapFailure(errors.New("git directory bootstrap exceeds limit"))
	}
	var command gitBootstrapCommand
	if err := json.Unmarshal([]byte(encoded), &command); err != nil || command.Path == "" || len(command.Argv) == 0 {
		bootstrapFailure(errors.New("invalid git directory bootstrap"))
	}
	if err := unix.Fchdir(3); err != nil {
		bootstrapFailure(fmt.Errorf("enter authenticated git directory: %w", err))
	}
	syscall.CloseOnExec(3)
	environment := withoutEnvironmentKey(os.Environ(), gitDirectoryBootstrap)
	if err := syscall.Exec(command.Path, command.Argv, environment); err != nil {
		bootstrapFailure(fmt.Errorf("exec git in authenticated directory: %w", err))
	}
}

func commandInDirectory(ctx context.Context, directory *os.File, command *exec.Cmd) (*exec.Cmd, error) {
	if directory == nil || command == nil || command.Path == "" || len(command.Args) == 0 {
		return nil, ErrUnsafePath
	}
	payload, err := json.Marshal(gitBootstrapCommand{Path: command.Path, Argv: command.Args})
	if err != nil {
		return nil, fmt.Errorf("encode git directory bootstrap: %w", err)
	}
	if len(payload) > maxGitBootstrapArgvBytes {
		return nil, fmt.Errorf("git directory bootstrap exceeds %d bytes: %w", maxGitBootstrapArgvBytes, ErrResourceLimit)
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve git directory bootstrap: %w", err)
	}
	bootstrap := exec.CommandContext(ctx, executable)
	bootstrap.Env = append(withoutEnvironmentKey(command.Env, gitDirectoryBootstrap), gitDirectoryBootstrap+"="+string(payload))
	bootstrap.ExtraFiles = []*os.File{directory}
	return bootstrap, nil
}
