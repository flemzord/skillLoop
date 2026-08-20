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
	maxGitCommandOutputBytes     = 64 * 1024
)

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

var gitConfigOverrides = []string{
	"--no-pager",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "core.untrackedCache=false",
	"-c", "core.attributesFile=/dev/null",
	"-c", "commit.gpgSign=false",
	"-c", "tag.gpgSign=false",
	"-c", "credential.helper=",
	"-c", "credential.interactive=false",
	"-c", "core.sshCommand=false",
	"-c", "gpg.program=false",
	"-c", "gpg.ssh.program=false",
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
	"-c", "submodule.recurse=false",
	"-c", "fetch.recurseSubmodules=false",
}

var unsafeGitEnvironment = map[string]struct{}{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": {},
	"GIT_ASKPASS":                      {},
	"GIT_CEILING_DIRECTORIES":          {},
	"GIT_COMMON_DIR":                   {},
	"GIT_CONFIG":                       {},
	"GIT_CONFIG_COUNT":                 {},
	"GIT_CONFIG_GLOBAL":                {},
	"GIT_CONFIG_PARAMETERS":            {},
	"GIT_CONFIG_SYSTEM":                {},
	"GIT_DIR":                          {},
	"GIT_DISCOVERY_ACROSS_FILESYSTEM":  {},
	"GIT_EXEC_PATH":                    {},
	"GIT_EXTERNAL_DIFF":                {},
	"GIT_INDEX_FILE":                   {},
	"GIT_NAMESPACE":                    {},
	"GIT_OBJECT_DIRECTORY":             {},
	"GIT_PREFIX":                       {},
	"GIT_REPLACE_REF_BASE":             {},
	"GIT_SHALLOW_FILE":                 {},
	"GIT_SSH":                          {},
	"GIT_SSH_COMMAND":                  {},
	"GIT_TEMPLATE_DIR":                 {},
	"GIT_WORK_TREE":                    {},
	"SSH_ASKPASS":                      {},
}

func git(ctx context.Context, repository string, args ...string) (string, error) {
	cmd := gitCommand(ctx, repository, args...)
	return runGitCommand(cmd, args)
}

func gitAt(ctx context.Context, directory *os.File, args ...string) (string, error) {
	cmd, err := commandInDirectory(ctx, directory, gitCommandCurrent(ctx, args...))
	if err != nil {
		return "", err
	}
	return runGitCommand(cmd, args)
}

func runGitCommand(cmd *exec.Cmd, args []string) (string, error) {
	stdout, err := runGitCommandBytes(cmd, args, maxGitCommandOutputBytes, defaultOutputLimit)
	return strings.TrimSpace(string(stdout)), err
}

func gitBytes(ctx context.Context, repository string, args ...string) ([]byte, error) {
	cmd := gitCommand(ctx, repository, args...)
	return runGitCommandBytes(cmd, args, maxGitCommandOutputBytes, defaultOutputLimit)
}

func gitBytesLimit(ctx context.Context, repository string, limit int64, args ...string) ([]byte, error) {
	if limit <= 0 {
		return nil, ErrResourceLimit
	}
	cmd := gitCommand(ctx, repository, args...)
	return runGitCommandBytes(cmd, args, limit, defaultOutputLimit)
}

type gitCommandOutput struct {
	stream   string
	contents []byte
	exceeded bool
	err      error
}

func runGitCommandBytes(cmd *exec.Cmd, args []string, stdoutLimit, stderrLimit int64) ([]byte, error) {
	if cmd == nil || stdoutLimit <= 0 || stderrLimit <= 0 {
		return nil, ErrResourceLimit
	}
	cmd.WaitDelay = runnerWaitDelay
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("capture git %s output: %w", strings.Join(args, " "), err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("capture git %s error output: %w", strings.Join(args, " "), err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start git %s: %w", strings.Join(args, " "), err)
	}

	results := make(chan gitCommandOutput, 2)
	terminate := func() { _ = cmd.Process.Kill() }
	go readGitCommandOutput("stdout", stdout, stdoutLimit, terminate, results)
	go readGitCommandOutput("stderr", stderr, stderrLimit, terminate, results)
	var stdoutResult, stderrResult gitCommandOutput
	killed := false
	for range 2 {
		result := <-results
		if result.exceeded || result.err != nil {
			if !killed {
				_ = cmd.Process.Kill()
				killed = true
			}
		}
		if result.stream == "stdout" {
			stdoutResult = result
		} else {
			stderrResult = result
		}
	}
	waitErr := cmd.Wait()
	for _, result := range []gitCommandOutput{stdoutResult, stderrResult} {
		if result.err != nil {
			return nil, fmt.Errorf("read git %s %s: %w", strings.Join(args, " "), result.stream, result.err)
		}
		if result.exceeded {
			limit := stdoutLimit
			if result.stream == "stderr" {
				limit = stderrLimit
			}
			return nil, fmt.Errorf("git %s produced more than %d bytes on %s: %w", strings.Join(args, " "), limit, result.stream, ErrResourceLimit)
		}
	}
	if waitErr != nil {
		message := strings.TrimSpace(string(stderrResult.contents))
		if message == "" {
			message = strings.TrimSpace(string(stdoutResult.contents))
		}
		if message == "" {
			message = waitErr.Error()
		}
		return nil, fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), message, waitErr)
	}
	return stdoutResult.contents, nil
}

func readGitCommandOutput(stream string, reader io.Reader, limit int64, terminate func(), results chan<- gitCommandOutput) {
	contents, err := io.ReadAll(io.LimitReader(reader, limit+1))
	result := gitCommandOutput{
		stream:   stream,
		contents: contents,
		exceeded: int64(len(contents)) > limit,
		err:      err,
	}
	if (result.exceeded || result.err != nil) && terminate != nil {
		terminate()
	}
	results <- result
}

// gitCommand is the single execution boundary for automated Git operations.
// Repositories are operator-owned, but their local configuration and tracked
// attributes are still data: none may turn an inspection, checkout, commit, or
// ref update into execution of a hook, signer, fsmonitor, filter, or helper.
func gitCommand(ctx context.Context, repository string, args ...string) *exec.Cmd {
	argv := make([]string, 0, len(gitConfigOverrides)+2+len(args))
	argv = append(argv, gitConfigOverrides...)
	argv = append(argv, "-C", repository)
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Env = safeGitEnvironment(os.Environ())
	return cmd
}

func gitCommandCurrent(ctx context.Context, args ...string) *exec.Cmd {
	argv := make([]string, 0, len(gitConfigOverrides)+len(args))
	argv = append(argv, gitConfigOverrides...)
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Env = safeGitEnvironment(os.Environ())
	return cmd
}

func safeGitEnvironment(environment []string) []string {
	safe := make([]string, 0, len(environment)+2)
	for _, entry := range environment {
		key, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if _, unsafe := unsafeGitEnvironment[key]; unsafe || strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			continue
		}
		safe = append(safe, entry)
	}
	// Empty pager variables disable fallback pagers without invoking even a
	// nominally harmless executable such as cat.
	safe = append(safe,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_PAGER=",
		"PAGER=",
	)
	return safe
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

// addWorktree materializes an exact commit without applying configured filters
// or executing Git helpers. A non-empty branch creates and checks out that
// branch; an empty branch creates a detached evaluation worktree.
func addWorktree(ctx context.Context, repository, worktree, revision, branch string) (string, error) {
	parent, err := openStateDirectory(filepath.Dir(worktree))
	if err != nil {
		return "", err
	}
	defer func() { _ = parent.Close() }()
	directory, output, err := addWorktreeAt(ctx, repository, parent, filepath.Base(worktree), worktree, revision, branch)
	if directory != nil {
		_ = directory.Close()
	}
	return output, err
}

func addWorktreeAt(
	ctx context.Context,
	repository string,
	parent *stateDirectory,
	name,
	worktree,
	revision,
	branch string,
) (*os.File, string, error) {
	if parent == nil || parent.file == nil || !safeStateComponent(name) {
		return nil, "", ErrUnsafePath
	}
	if err := unix.Mkdirat(int(parent.file.Fd()), name, 0o700); err != nil {
		return nil, "", fmt.Errorf("create worktree directory: %w", err)
	}
	directory, err := openStateChild(parent, name)
	if err != nil {
		_ = unix.Unlinkat(int(parent.file.Fd()), name, unix.AT_REMOVEDIR)
		return nil, "", fmt.Errorf("open worktree directory: %w", err)
	}
	cleanup := true
	defer func() {
		if !cleanup {
			return
		}
		_, _ = gitAt(context.Background(), directory, "worktree", "remove", "--force", ".")
		_ = directory.Close()
		_ = unix.Unlinkat(int(parent.file.Fd()), name, unix.AT_REMOVEDIR)
	}()
	gitDirectory, err := git(ctx, repository, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, "", err
	}
	args := []string{"worktree", "add"}
	if branch == "" {
		args = append(args, "--detach")
	} else {
		args = append(args, "-b", branch)
	}
	args = append(args, ".", revision)
	output, err := gitWithoutFiltersAt(ctx, repository, directory, append([]string{"--git-dir=" + gitDirectory}, args...)...)
	if err != nil {
		return nil, "", err
	}
	if err := verifyExactWorktreeAt(ctx, repository, worktree, directory, revision, branch); err != nil {
		if branch != "" {
			_, _ = git(context.Background(), repository, "branch", "-D", branch)
		}
		return nil, "", err
	}
	cleanup = false
	return directory, output, nil
}

// gitWithoutFilters also protects the diff/index operations performed inside
// a candidate worktree. Git's -c settings on worktree add are command-scoped;
// without applying them again, a clean or process filter would be re-enabled
// as soon as SkillLoop inspects or stages the candidate.
func gitWithoutFiltersAt(ctx context.Context, repository string, directory *os.File, args ...string) (string, error) {
	overrides, err := worktreeFilterOverrides(ctx, repository)
	if err != nil {
		return "", err
	}
	gitArgs := make([]string, 0, len(overrides)+len(args))
	gitArgs = append(gitArgs, overrides...)
	gitArgs = append(gitArgs, args...)
	return gitAt(ctx, directory, gitArgs...)
}

func verifyExactWorktree(ctx context.Context, repository, worktree, revision, branch string) error {
	parent, err := openStateDirectory(filepath.Dir(worktree))
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	directory, err := openStateChild(parent, filepath.Base(worktree))
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return verifyExactWorktreeAt(ctx, repository, worktree, directory, revision, branch)
}

func verifyExactWorktreeAt(
	ctx context.Context,
	repository,
	worktree string,
	directory *os.File,
	revision,
	branch string,
) error {
	if directory == nil {
		return ErrUnsafePath
	}
	info, err := directory.Stat()
	if err != nil {
		return fmt.Errorf("inspect materialized worktree: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("materialized worktree is not a directory: %w", ErrUnsafePath)
	}
	root, err := gitAt(ctx, directory, "rev-parse", "--show-toplevel")
	if err != nil || !samePath(root, worktree) {
		return fmt.Errorf("materialized worktree root differs from requested path: %w", ErrDrift)
	}
	expected, err := git(ctx, repository, "rev-parse", revision+"^{commit}")
	if err != nil {
		return err
	}
	head, err := gitAt(ctx, directory, "rev-parse", "HEAD^{commit}")
	if err != nil || head != expected {
		return fmt.Errorf("materialized worktree has revision %q instead of %q: %w", head, expected, ErrDrift)
	}
	repositoryCommon, err := git(ctx, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	worktreeCommon, err := gitAt(ctx, directory, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || !samePath(repositoryCommon, worktreeCommon) {
		return fmt.Errorf("materialized worktree belongs to a different repository: %w", ErrDrift)
	}
	ref, err := gitAt(ctx, directory, "rev-parse", "--symbolic-full-name", "HEAD")
	if err != nil {
		return err
	}
	if branch == "" {
		if ref != "HEAD" {
			return fmt.Errorf("evaluation worktree is attached to %q: %w", ref, ErrDrift)
		}
		return nil
	}
	if ref != "refs/heads/"+branch {
		return fmt.Errorf("candidate worktree is attached to %q instead of %q: %w", ref, branch, ErrDrift)
	}
	return nil
}

func worktreeFilterOverrides(ctx context.Context, repository string) ([]string, error) {
	// Read only the repository's raw local config. Conditional includes can be
	// inactive on the source branch and become active only after worktree add
	// creates a skillloop/* branch, hiding an executable filter from this scan.
	// Fail closed before materializing any worktree rather than trying to
	// predict every includeIf condition Git supports.
	scopes := []string{"--local"}
	worktreeConfigEnabled, err := repositoryWorktreeConfigEnabled(ctx, repository)
	if err != nil {
		return nil, err
	}
	if worktreeConfigEnabled {
		scopes = append(scopes, "--worktree")
	}
	configuredByScope := make([][]byte, 0, len(scopes))
	for _, scope := range scopes {
		configured, err := gitBytesLimit(ctx, repository, maxWorktreeFilterConfigBytes,
			"config", scope, "--no-includes", "--null", "--name-only", "--list")
		if err != nil {
			return nil, fmt.Errorf("enumerate %s configured Git filters: %w", scope, err)
		}
		configuredByScope = append(configuredByScope, configured)
	}

	drivers := make(map[string]struct{})
	for _, configured := range configuredByScope {
		for rawKey := range bytes.SplitSeq(configured, []byte{0}) {
			if len(rawKey) == 0 {
				continue
			}
			key := string(rawKey)
			lower := strings.ToLower(key)
			if lower == "include.path" || (strings.HasPrefix(lower, "includeif.") && strings.HasSuffix(lower, ".path")) {
				return nil, fmt.Errorf("repository-local Git config includes %q: %w", key, ErrUnsafeChange)
			}
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

func repositoryWorktreeConfigEnabled(ctx context.Context, repository string) (bool, error) {
	value, err := git(ctx, repository,
		"config", "--local", "--no-includes", "--type=bool", "--get", "extensions.worktreeConfig")
	if err == nil {
		return value == "true", nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("inspect Git worktree configuration: %w", err)
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
	args := []string{"ls-tree", "-r", "-l", "-z", "--full-tree", revision}
	cmd := gitCommand(ctx, repository, args...)
	cmd.WaitDelay = runnerWaitDelay
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("capture git tree listing: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("capture git tree error output: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start git tree listing: %w", err)
	}
	stderrResults := make(chan gitCommandOutput, 1)
	go readGitCommandOutput("stderr", stderr, defaultOutputLimit, func() { _ = cmd.Process.Kill() }, stderrResults)

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
	}
	stderrResult := <-stderrResults
	waitErr := cmd.Wait()
	if stderrResult.exceeded {
		return fmt.Errorf("git ls-tree %s produced more than %d bytes on stderr: %w", revision, defaultOutputLimit, ErrResourceLimit)
	}
	if validationErr != nil {
		return validationErr
	}
	if stderrResult.err != nil {
		return fmt.Errorf("read git ls-tree %s stderr: %w", revision, stderrResult.err)
	}
	if waitErr != nil {
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ctx.Err()
		}
		message := strings.TrimSpace(string(stderrResult.contents))
		if message == "" {
			message = waitErr.Error()
		}
		return fmt.Errorf("git ls-tree %s: %s: %w", revision, message, waitErr)
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
