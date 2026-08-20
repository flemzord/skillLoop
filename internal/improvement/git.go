package improvement

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	maxWorktreeFilterConfigBytes = 64 * 1024
	maxWorktreeFilters           = 128
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

func gitBytesLimit(ctx context.Context, repository string, limit int64, args ...string) ([]byte, error) {
	if limit <= 0 {
		return nil, ErrResourceLimit
	}
	argv := append([]string{"-C", repository}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.WaitDelay = runnerWaitDelay
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("capture git %s output: %w", strings.Join(args, " "), err)
	}
	stderr := &cappedBuffer{limit: defaultOutputLimit}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start git %s: %w", strings.Join(args, " "), err)
	}
	contents, readErr := io.ReadAll(io.LimitReader(stdout, limit+1))
	if readErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("read git %s output: %w", strings.Join(args, " "), readErr)
	}
	if int64(len(contents)) > limit {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("git %s produced more than %d bytes: %w", strings.Join(args, " "), limit, ErrResourceLimit)
	}
	if err := cmd.Wait(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), message, err)
	}
	return contents, nil
}

func readFileLimit(path string, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, ErrResourceLimit
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open bounded file: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open bounded file: invalid file descriptor")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect bounded file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("bounded file is not regular: %w", ErrUnsafePath)
	}
	if info.Size() < 0 || info.Size() > limit {
		return nil, fmt.Errorf("file exceeds %d bytes: %w", limit, ErrResourceLimit)
	}
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read bounded file: %w", err)
	}
	if int64(len(contents)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes: %w", limit, ErrResourceLimit)
	}
	return contents, nil
}

func preflightWorktree(ctx context.Context, repository, revision string) error {
	return preflightWorktreeWithLimits(ctx, repository, revision, maxWorktreeFiles, maxWorktreeBytes)
}

// addWorktree materializes committed blobs without applying user- or
// repository-configured filters. The resource preflight counts raw Git
// objects, so allowing filters during checkout would let a tiny blob expand
// beyond those limits or run an arbitrary local filter command.
func addWorktree(ctx context.Context, repository string, args ...string) (string, error) {
	return gitWithoutFilters(ctx, repository, append([]string{"worktree", "add"}, args...)...)
}

// gitWithoutFilters also protects the diff/index operations performed inside
// a candidate worktree. Git's -c settings on worktree add are command-scoped;
// without applying them again, a clean or process filter would be re-enabled
// as soon as SkillLoop inspects or stages the candidate.
func gitWithoutFilters(ctx context.Context, repository string, args ...string) (string, error) {
	overrides, err := worktreeFilterOverrides(ctx, repository)
	if err != nil {
		return "", err
	}
	gitArgs := make([]string, 0, len(overrides)+len(args))
	gitArgs = append(gitArgs, overrides...)
	gitArgs = append(gitArgs, args...)
	return git(ctx, repository, gitArgs...)
}

func worktreeFilterOverrides(ctx context.Context, repository string) ([]string, error) {
	configured, err := gitBytesLimit(ctx, repository, maxWorktreeFilterConfigBytes, "config", "--null", "--name-only", "--list")
	if err != nil {
		return nil, fmt.Errorf("enumerate configured Git filters: %w", err)
	}

	drivers := make(map[string]struct{})
	for rawKey := range bytes.SplitSeq(configured, []byte{0}) {
		if len(rawKey) == 0 {
			continue
		}
		key := string(rawKey)
		lower := strings.ToLower(key)
		if !strings.HasPrefix(lower, "filter.") {
			continue
		}
		var suffix string
		for _, candidate := range []string{".clean", ".smudge", ".process", ".required"} {
			if strings.HasSuffix(lower, candidate) {
				suffix = candidate
				break
			}
		}
		if suffix == "" {
			continue
		}
		driver := key[len("filter.") : len(key)-len(suffix)]
		if !validFilterDriver(driver) {
			return nil, fmt.Errorf("unsafe Git filter driver %q: %w", driver, ErrUnsafeChange)
		}
		drivers[driver] = struct{}{}
		if len(drivers) > maxWorktreeFilters {
			return nil, fmt.Errorf("more than %d Git filters are configured: %w", maxWorktreeFilters, ErrResourceLimit)
		}
	}

	names := make([]string, 0, len(drivers))
	for driver := range drivers {
		names = append(names, driver)
	}
	sort.Strings(names)
	overrides := make([]string, 0, len(names)*8)
	for _, driver := range names {
		overrides = append(overrides,
			"-c", "filter."+driver+".clean=cat",
			"-c", "filter."+driver+".smudge=cat",
			"-c", "filter."+driver+".process=",
			"-c", "filter."+driver+".required=false",
		)
	}
	return overrides, nil
}

func validFilterDriver(driver string) bool {
	if driver == "" || len(driver) > 256 || !utf8.ValidString(driver) || strings.ContainsRune(driver, '=') {
		return false
	}
	for _, character := range driver {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func preflightWorktreeWithLimits(ctx context.Context, repository, revision string, maxFiles int, maxBytes int64) error {
	if maxFiles <= 0 || maxBytes <= 0 {
		return ErrResourceLimit
	}
	if err := commitExists(ctx, repository, revision); err != nil {
		return err
	}
	argv := []string{"-C", repository, "ls-tree", "-r", "-l", "-z", "--full-tree", revision}
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.WaitDelay = runnerWaitDelay
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("capture git tree listing: %w", err)
	}
	stderr := &cappedBuffer{limit: defaultOutputLimit}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start git tree listing: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Split(scanNUL)
	scanner.Buffer(make([]byte, 4096), 64*1024)
	files := 0
	var total int64
	var validationErr error
	for scanner.Scan() {
		files++
		if files > maxFiles {
			validationErr = fmt.Errorf("git tree at %s contains more than %d entries: %w", revision, maxFiles, ErrResourceLimit)
			break
		}
		record := scanner.Text()
		metadata, _, found := strings.Cut(record, "\t")
		fields := strings.Fields(metadata)
		if !found || len(fields) != 4 {
			validationErr = fmt.Errorf("parse git tree entry %q: %w", record, ErrUnsafePath)
			break
		}
		if fields[3] == "-" {
			continue
		}
		size, parseErr := strconv.ParseInt(fields[3], 10, 64)
		if parseErr != nil || size < 0 {
			validationErr = fmt.Errorf("parse git tree object size %q: %w", fields[3], ErrUnsafePath)
			break
		}
		if size > maxBytes-total {
			validationErr = fmt.Errorf("git tree at %s exceeds %d bytes: %w", revision, maxBytes, ErrResourceLimit)
			break
		}
		total += size
	}
	if scanErr := scanner.Err(); scanErr != nil && validationErr == nil {
		validationErr = fmt.Errorf("read bounded git tree listing: %w: %w", scanErr, ErrResourceLimit)
	}
	if validationErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return validationErr
	}
	if err := cmd.Wait(); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ctx.Err()
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("git ls-tree %s: %s: %w", revision, message, err)
	}
	return nil
}

func scanNUL(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if index := bytes.IndexByte(data, 0); index >= 0 {
		return index + 1, data[:index], nil
	}
	if atEOF {
		if len(data) == 0 {
			return 0, nil, nil
		}
		return 0, nil, errors.New("unterminated git tree entry")
	}
	return 0, nil, nil
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
	var (
		total  int
		inHunk bool
	)
	for line := range strings.SplitSeq(diff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			inHunk = false
			continue
		}
		if strings.HasPrefix(line, "@@ ") {
			inHunk = true
			continue
		}
		if inHunk && (strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-")) {
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
