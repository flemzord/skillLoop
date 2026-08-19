package improvement

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

func git(ctx context.Context, repository string, args ...string) (string, error) {
	argv := append([]string{"-C", repository}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), message, err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func gitBytes(ctx context.Context, repository string, args ...string) ([]byte, error) {
	argv := append([]string{"-C", repository}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), message, err)
	}
	return stdout.Bytes(), nil
}

func resolveRepository(ctx context.Context, repository string) (string, error) {
	if repository == "" {
		return "", fmt.Errorf("repository path is required: %w", ErrUnsafePath)
	}
	abs, err := filepath.Abs(repository)
	if err != nil {
		return "", fmt.Errorf("resolve repository: %w", err)
	}
	root, err := git(ctx, abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	if !samePath(abs, root) {
		return "", fmt.Errorf("repository path %q is not the git root %q: %w", abs, root, ErrUnsafePath)
	}
	return filepath.Clean(root), nil
}

func resolveInstruction(repository, instruction string) (string, string, error) {
	if instruction == "" {
		return "", "", fmt.Errorf("instruction path is required: %w", ErrUnsafePath)
	}
	var absolute string
	if filepath.IsAbs(instruction) {
		absolute = filepath.Clean(instruction)
	} else {
		absolute = filepath.Join(repository, filepath.Clean(instruction))
	}
	relative, err := filepath.Rel(repository, absolute)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("instruction %q escapes repository: %w", instruction, ErrUnsafePath)
	}
	if filepath.Base(relative) != "SKILL.md" {
		return "", "", fmt.Errorf("only SKILL.md may be improved, got %q: %w", relative, ErrUnsafePath)
	}

	current := repository
	for part := range strings.SplitSeq(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			return "", "", fmt.Errorf("inspect %s: %w", current, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", "", fmt.Errorf("symlink component %s: %w", current, ErrUnsafePath)
		}
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", "", fmt.Errorf("stat instruction: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("instruction is not a regular file: %w", ErrUnsafePath)
	}
	return absolute, filepath.ToSlash(relative), nil
}

func verifyTrackedInstruction(ctx context.Context, repository, revision, relative string) error {
	entry, err := git(ctx, repository, "ls-tree", revision, "--", relative)
	if err != nil {
		return err
	}
	fields := strings.Fields(entry)
	if len(fields) < 4 || (fields[0] != "100644" && fields[0] != "100755") || fields[1] != "blob" {
		return fmt.Errorf("SKILL.md must be a regular tracked file at %s: %w", revision, ErrUnsafePath)
	}
	return nil
}

func commitExists(ctx context.Context, repository, revision string) error {
	if !fullSHA.MatchString(revision) {
		return fmt.Errorf("invalid commit %q: %w", revision, ErrDrift)
	}
	if _, err := git(ctx, repository, "cat-file", "-e", revision+"^{commit}"); err != nil {
		return fmt.Errorf("commit %s is unavailable: %w", revision, ErrDrift)
	}
	return nil
}

func samePath(first, second string) bool {
	firstReal, firstErr := filepath.EvalSymlinks(first)
	secondReal, secondErr := filepath.EvalSymlinks(second)
	if firstErr == nil && secondErr == nil {
		return filepath.Clean(firstReal) == filepath.Clean(secondReal)
	}
	return filepath.Clean(first) == filepath.Clean(second)
}

func randomSuffix() (string, error) {
	value := make([]byte, 6)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("create random suffix: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func safeName(value string) string {
	var out strings.Builder
	for _, char := range strings.ToLower(value) {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9', char == '-', char == '_':
			out.WriteRune(char)
		default:
			out.WriteByte('-')
		}
	}
	clean := strings.Trim(out.String(), "-_")
	if clean == "" {
		clean = "skill"
	}
	if len(clean) > 48 {
		clean = clean[:48]
	}
	return clean
}

func lineCount(diff string) (int, error) {
	var total int
	for line := range strings.SplitSeq(diff, "\n") {
		if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
			continue
		}
		if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
			total++
		}
	}
	return total, nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
