package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunSkillGitHonorsContextDeadline(t *testing.T) {
	binDirectory := t.TempDir()
	gitPath := filepath.Join(binDirectory, "git")
	if err := os.WriteFile(gitPath, []byte("#!/bin/sh\nwhile :; do :; done\n"), 0o700); err != nil {
		t.Fatalf("write blocking Git shim: %v", err)
	}
	t.Setenv("PATH", binDirectory)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := runSkillGit(ctx, t.TempDir(), "rev-parse", "--show-toplevel")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runSkillGit() error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Git timeout took %s, want less than one second", elapsed)
	}
}

func TestRunSkillGitBoundsCommandOutput(t *testing.T) {
	binDirectory := t.TempDir()
	gitPath := filepath.Join(binDirectory, "git")
	script := "#!/bin/sh\ni=0\nwhile [ \"$i\" -lt 70000 ]; do printf x; i=$((i + 1)); done\n"
	if err := os.WriteFile(gitPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write noisy Git shim: %v", err)
	}
	t.Setenv("PATH", binDirectory)

	output, err := runSkillGit(context.Background(), t.TempDir(), "rev-parse", "--show-toplevel")
	if !errors.Is(err, errSkillGitOutputLimit) {
		t.Fatalf("runSkillGit() output bytes = %d, error = %v, want output limit", len(output), err)
	}
}
