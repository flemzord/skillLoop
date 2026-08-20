package improvement

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/flemzord/skillloop/internal/domain"
)

func TestPreflightWorktreeEnforcesTreeLimits(t *testing.T) {
	repository, _ := newTestRepository(t)
	revision := testGit(t, repository, "rev-parse", "HEAD")

	if err := preflightWorktreeWithLimits(context.Background(), repository, revision, 10, 1024*1024); err != nil {
		t.Fatalf("legitimate repository rejected: %v", err)
	}
	if err := preflightWorktreeWithLimits(context.Background(), repository, revision, 1, 1024*1024); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("file-count limit error = %v, want ErrResourceLimit", err)
	}
	if err := preflightWorktreeWithLimits(context.Background(), repository, revision, 10, 1); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("byte limit error = %v, want ErrResourceLimit", err)
	}
}

func TestRunOneReturnsAfterDescendantKeepsOutputDescriptors(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "descendant-effect")
	t.Setenv("SKILLLOOP_DESCENDANT_HELPER", "parent")
	t.Setenv("SKILLLOOP_DESCENDANT_MARKER", marker)
	service := Service{
		Runner: Runner{
			Argv:        []string{os.Args[0], "-test.run=^TestExternalRunnerPipeDescendantHelper$"},
			Timeout:     5 * time.Second,
			OutputLimit: 1024,
		},
	}
	started := time.Now()
	result, err := service.runOne(context.Background(), t.TempDir(), strings.Repeat("a", 40))
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("runner with non-zero parent exit: %v", err)
	}
	if result.ExitCode != 1 {
		t.Fatalf("runner exit = %d, want 1", result.ExitCode)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("runner waited %s for descendant output descriptors", elapsed)
	}
	if strings.Contains(result.Output, "descendant-blocked") {
		assertMarkerAbsent(t, marker)
		return
	}
	pid, parseErr := descendantPID(result.Output)
	if parseErr != nil {
		t.Fatalf("parse descendant PID from %q: %v", result.Output, parseErr)
	}
	assertDescendantStopped(t, pid, marker)
}

func TestRunOneKillsProcessGroupOnTimeout(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "descendant-effect")
	t.Setenv("SKILLLOOP_DESCENDANT_HELPER", "timeout-parent")
	t.Setenv("SKILLLOOP_DESCENDANT_MARKER", marker)
	service := Service{
		Runner: Runner{
			Argv:        []string{os.Args[0], "-test.run=^TestExternalRunnerPipeDescendantHelper$"},
			Timeout:     150 * time.Millisecond,
			OutputLimit: 1024,
		},
	}
	started := time.Now()
	result, err := service.runOne(context.Background(), t.TempDir(), strings.Repeat("b", 40))
	if err != nil {
		t.Fatalf("timed out runner: %v", err)
	}
	if !result.TimedOut {
		t.Fatalf("runner result = %#v, want timeout", result)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("timed out runner returned after %s", elapsed)
	}
	pid, parseErr := descendantPID(result.Output)
	if parseErr != nil && !strings.Contains(result.Output, "descendant-blocked") {
		t.Fatalf("parse timed out descendant PID from %q: %v", result.Output, parseErr)
	}
	if parseErr == nil {
		assertDescendantStopped(t, pid, marker)
	} else {
		assertMarkerAbsent(t, marker)
	}
}

func TestRunOnePreventsDescendantGroupEscape(t *testing.T) {
	for _, mode := range []string{"escape-session-parent", "escape-group-parent"} {
		t.Run(mode, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "escaped-descendant-effect")
			t.Setenv("SKILLLOOP_DESCENDANT_HELPER", mode)
			t.Setenv("SKILLLOOP_DESCENDANT_MARKER", marker)
			service := Service{Runner: Runner{
				Argv:        []string{os.Args[0], "-test.run=^TestExternalRunnerPipeDescendantHelper$"},
				Timeout:     5 * time.Second,
				OutputLimit: 1024,
			}}
			result, err := service.runOne(context.Background(), t.TempDir(), strings.Repeat("c", 40))
			if err != nil {
				t.Fatalf("run contained evaluator: %v", err)
			}
			if result.ExitCode != 1 {
				t.Fatalf("escape runner exit = %d, want 1", result.ExitCode)
			}
			if !strings.Contains(result.Output, "escape-blocked") {
				t.Fatalf("Linux process-group escape was not blocked: %#v", result)
			}
			assertMarkerAbsent(t, marker)
		})
	}
}

func TestRunOneSupportsLegitimateSubprocesses(t *testing.T) {
	t.Setenv("SKILLLOOP_DESCENDANT_HELPER", "legitimate-parent")
	service := Service{Runner: Runner{
		Argv:        []string{os.Args[0], "-test.run=^TestExternalRunnerPipeDescendantHelper$"},
		Timeout:     5 * time.Second,
		OutputLimit: 1024,
	}}
	result, err := service.runOne(context.Background(), t.TempDir(), strings.Repeat("d", 40))
	if err != nil {
		t.Fatalf("run multi-process evaluator: %v", err)
	}
	if runtime.GOOS == "darwin" {
		if result.ExitCode == 0 || !strings.Contains(strings.ToLower(result.Output), "operation not permitted") {
			t.Fatalf("macOS multi-process evaluator did not fail closed: %#v", result)
		}
		return
	}
	if result.ExitCode != 0 || !strings.Contains(result.Output, "multiprocess-ok") {
		t.Fatalf("legitimate subprocess failed: %#v", result)
	}
}

func TestContainedProcessTerminationIsIdempotent(t *testing.T) {
	calls := 0
	terminator := &processGroupTerminator{
		command: &exec.Cmd{},
		kill: func(*exec.Cmd) error {
			calls++
			return nil
		},
	}
	if err := terminator.terminate(); err != nil {
		t.Fatal(err)
	}
	if err := terminator.terminate(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("contained-process kill calls = %d, want 1", calls)
	}
}

func TestExternalRunnerPipeDescendantHelper(t *testing.T) {
	switch os.Getenv("SKILLLOOP_DESCENDANT_HELPER") {
	case "parent", "timeout-parent", "escape-session-parent", "escape-group-parent":
		mode := os.Getenv("SKILLLOOP_DESCENDANT_HELPER")
		command := exec.Command(os.Args[0], "-test.run=^TestExternalRunnerPipeDescendantHelper$")
		childMode := "child"
		if strings.HasPrefix(mode, "escape-") {
			childMode = "escape-child"
		}
		command.Env = append(os.Environ(), "SKILLLOOP_DESCENDANT_HELPER="+childMode)
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		switch mode {
		case "escape-session-parent":
			command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		case "escape-group-parent":
			command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		}
		if err := command.Start(); err != nil {
			if strings.HasPrefix(mode, "escape-") {
				fmt.Println("escape-blocked")
			} else {
				fmt.Println("descendant-blocked")
			}
			if mode == "timeout-parent" {
				time.Sleep(30 * time.Second)
			}
			os.Exit(1)
		}
		fmt.Printf("pid=%d\n", command.Process.Pid)
		if mode == "timeout-parent" {
			time.Sleep(30 * time.Second)
		}
		os.Exit(1)
	case "legitimate-parent":
		command := exec.Command(os.Args[0], "-test.run=^TestExternalRunnerPipeDescendantHelper$")
		command.Env = append(os.Environ(), "SKILLLOOP_DESCENDANT_HELPER=legitimate-child")
		if output, err := command.CombinedOutput(); err != nil {
			fmt.Printf("legitimate child: %v: %s", err, output)
			os.Exit(2)
		}
		fmt.Println("multiprocess-ok")
		os.Exit(0)
	case "legitimate-child":
		fmt.Println("child-ok")
	case "child":
		time.Sleep(750 * time.Millisecond)
		if marker := os.Getenv("SKILLLOOP_DESCENDANT_MARKER"); marker != "" {
			_ = os.WriteFile(marker, []byte("descendant survived\n"), 0o600)
		}
		time.Sleep(30 * time.Second)
	case "escape-child":
		_ = os.Stdout.Close()
		_ = os.Stderr.Close()
		time.Sleep(5 * time.Second)
		if marker := os.Getenv("SKILLLOOP_DESCENDANT_MARKER"); marker != "" {
			_ = os.WriteFile(marker, []byte("escaped descendant survived\n"), 0o600)
		}
		time.Sleep(30 * time.Second)
	}
}

func TestAutomatedGitOperationsDisableExecutableConfiguration(t *testing.T) {
	repository, skill := newTestRepository(t)
	marker := filepath.Join(t.TempDir(), "git-executable-config-invoked")
	t.Setenv("SKILLLOOP_GIT_EXEC_MARKER", marker)
	script := filepath.Join(t.TempDir(), "reject-execution.sh")
	writeTestFile(t, script, []byte("#!/bin/sh\nprintf invoked >> \"$SKILLLOOP_GIT_EXEC_MARKER\"\nexit 99\n"), 0o755)
	writeTestFile(t, filepath.Join(repository, ".gitattributes"), []byte("skills/demo/SKILL.md diff=invoke\n"), 0o644)
	testGit(t, repository, "add", ".gitattributes")
	testGit(t, repository, "commit", "-m", "test: add textconv driver")

	hooks := filepath.Join(t.TempDir(), "hooks")
	for _, name := range []string{"post-checkout", "pre-commit", "commit-msg", "post-commit", "reference-transaction"} {
		writeTestFile(t, filepath.Join(hooks, name), []byte("#!/bin/sh\nprintf invoked >> \"$SKILLLOOP_GIT_EXEC_MARKER\"\nexit 99\n"), 0o755)
	}
	testGit(t, repository, "config", "core.hooksPath", hooks)
	testGit(t, repository, "config", "commit.gpgSign", "true")
	testGit(t, repository, "config", "gpg.program", script)
	testGit(t, repository, "config", "core.fsmonitor", script)
	testGit(t, repository, "config", "diff.external", script)
	testGit(t, repository, "config", "diff.invoke.textconv", script)

	stateDir := t.TempDir()
	t.Cleanup(func() { makeTestTreeWritable(stateDir) })
	service := testServiceWithExternalRunner(t, stateDir)
	candidate, err := service.Prepare(context.Background(), skill, testCluster(skill.ID))
	if err != nil {
		t.Fatalf("Prepare executed repository configuration: %v", err)
	}
	evaluation, err := service.Evaluate(context.Background(), candidate)
	if err != nil || !evaluation.Passed {
		t.Fatalf("Evaluate executed repository configuration: passed=%v err=%v", evaluation.Passed, err)
	}
	if _, err := service.Promote(context.Background(), skill, candidate, evaluation, Approval{
		BaseCommit: candidate.BaseCommit, CandidateCommit: candidate.CandidateCommit,
	}); err != nil {
		t.Fatalf("Promote executed repository configuration: %v", err)
	}
	if err := service.Cleanup(context.Background(), candidate); err != nil {
		t.Fatalf("Cleanup executed repository configuration: %v", err)
	}
	assertMarkerAbsent(t, marker)
}

func TestPrepareRejectsBranchConditionalGitConfigInclude(t *testing.T) {
	for _, scope := range []string{"local", "worktree"} {
		t.Run(scope, func(t *testing.T) {
			repository, skill := newTestRepository(t)
			marker := filepath.Join(t.TempDir(), "conditional-filter-invoked")
			t.Setenv("SKILLLOOP_GIT_EXEC_MARKER", marker)
			script := filepath.Join(t.TempDir(), "conditional-smudge.sh")
			writeTestFile(t, script, []byte("#!/bin/sh\nprintf invoked >> \"$SKILLLOOP_GIT_EXEC_MARKER\"\ncat\n"), 0o755)
			writeTestFile(t, filepath.Join(repository, ".gitattributes"), []byte("skills/demo/SKILL.md filter=conditional\n"), 0o644)
			testGit(t, repository, "add", ".gitattributes")
			testGit(t, repository, "commit", "-m", "test: add conditional filter attribute")

			conditionalConfig := filepath.Join(t.TempDir(), "candidate-branch.gitconfig")
			writeTestFile(t, conditionalConfig, fmt.Appendf(nil,
				"[filter \"conditional\"]\n\tsmudge = %s\n\trequired = true\n", script,
			), 0o600)
			configArgs := []string{"config"}
			if scope == "worktree" {
				testGit(t, repository, "config", "extensions.worktreeConfig", "true")
				configArgs = append(configArgs, "--worktree")
			}
			configArgs = append(configArgs, "includeIf.onbranch:skillloop/**.path", conditionalConfig)
			testGit(t, repository, configArgs...)

			_, err := (Service{StateDir: t.TempDir()}).Prepare(context.Background(), skill, testCluster(skill.ID))
			if !errors.Is(err, ErrUnsafeChange) {
				t.Fatalf("Prepare conditional include error = %v, want ErrUnsafeChange", err)
			}
			assertMarkerAbsent(t, marker)
		})
	}
}

func TestCleanupAuthenticatesExactCandidateWorktree(t *testing.T) {
	repository, skill := newTestRepository(t)
	service := Service{StateDir: t.TempDir()}
	candidate, err := service.Prepare(context.Background(), skill, testCluster(skill.ID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = git(context.Background(), repository, "worktree", "remove", "--force", candidate.WorktreePath)
		_, _ = git(context.Background(), repository, "branch", "-D", candidate.Branch)
	})

	testGit(t, candidate.WorktreePath, "checkout", "--detach", candidate.CandidateCommit)
	if err := service.Cleanup(context.Background(), candidate); !errors.Is(err, ErrDrift) {
		t.Fatalf("detached candidate cleanup error = %v, want ErrDrift", err)
	}
	if _, err := os.Stat(candidate.WorktreePath); err != nil {
		t.Fatalf("inauthentic worktree was removed: %v", err)
	}
	testGit(t, candidate.WorktreePath, "checkout", candidate.Branch)
	if err := service.Cleanup(context.Background(), candidate); err != nil {
		t.Fatalf("legitimate candidate cleanup: %v", err)
	}
}

func TestExactWorktreeRejectsDifferentRepositoryAtSameRevision(t *testing.T) {
	repository, skill := newTestRepository(t)
	service := Service{StateDir: t.TempDir()}
	candidate, err := service.Prepare(context.Background(), skill, testCluster(skill.ID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = git(context.Background(), repository, "branch", "-D", candidate.Branch)
	})
	if _, err := git(context.Background(), repository, "worktree", "remove", "--force", candidate.WorktreePath); err != nil {
		t.Fatal(err)
	}

	clone := exec.Command("git", "clone", "--no-checkout", repository, candidate.WorktreePath)
	if output, err := clone.CombinedOutput(); err != nil {
		t.Fatalf("clone adversarial repository: %v\n%s", err, output)
	}
	testGit(t, candidate.WorktreePath, "checkout", "-b", candidate.Branch, candidate.CandidateCommit)
	if err := verifyExactWorktree(context.Background(), repository, candidate.WorktreePath, candidate.CandidateCommit, candidate.Branch); !errors.Is(err, ErrDrift) {
		t.Fatalf("different repository worktree error = %v, want ErrDrift", err)
	}
}

func TestCleanupRejectsExactParentOfWorktreeDirectory(t *testing.T) {
	repository, _ := newTestRepository(t)
	stateDir := t.TempDir()
	candidate := Candidate{
		RepositoryPath: repository,
		WorktreePath:   stateDir,
		Branch:         "skillloop/demo/candidate",
	}
	if err := (Service{StateDir: stateDir}).Cleanup(context.Background(), candidate); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("parent worktree path error = %v, want ErrUnsafePath", err)
	}
}

func TestSafeGitEnvironmentRejectsCommandInjectionVariables(t *testing.T) {
	environment := safeGitEnvironment([]string{
		"PATH=/usr/bin",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.hooksPath",
		"GIT_CONFIG_VALUE_0=/tmp/attacker-hooks",
		"GIT_EXTERNAL_DIFF=/tmp/attacker-diff",
		"GIT_EXEC_PATH=/tmp/attacker-exec-path",
	})
	joined := strings.Join(environment, "\n")
	for _, rejected := range []string{"GIT_CONFIG_COUNT=", "GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_", "GIT_EXTERNAL_DIFF=", "GIT_EXEC_PATH="} {
		if strings.Contains(joined, rejected) {
			t.Fatalf("unsafe Git environment survived: %s", rejected)
		}
	}
	if !strings.Contains(joined, "PATH=/usr/bin") || !strings.Contains(joined, "GIT_PAGER=") || !strings.Contains(joined, "GIT_NO_REPLACE_OBJECTS=1") {
		t.Fatalf("legitimate environment or pager hardening missing: %q", environment)
	}
}

func TestGitOperationsIgnoreReplacementObjects(t *testing.T) {
	repository, skill := newTestRepository(t)
	base := testGit(t, repository, "rev-parse", "HEAD^{commit}")
	original, err := os.ReadFile(filepath.Join(repository, filepath.FromSlash(skill.InstructionPath)))
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(repository, filepath.FromSlash(skill.InstructionPath)), []byte("---\nname: replacement\ndescription: Replaced tree\n---\n"), 0o644)
	writeTestFile(t, filepath.Join(repository, "replacement-only.txt"), []byte("replacement\n"), 0o644)
	testGit(t, repository, "add", ".")
	testGit(t, repository, "commit", "-m", "test: add replacement tree")
	replacement := testGit(t, repository, "rev-parse", "HEAD^{commit}")
	testGit(t, repository, "replace", base, replacement)

	shown, err := gitBytes(context.Background(), repository, "show", base+":"+skill.InstructionPath)
	if err != nil || !bytes.Equal(shown, original) {
		t.Fatalf("show followed replacement object: contents=%q err=%v", shown, err)
	}
	diff, err := git(context.Background(), repository, "diff", "--no-ext-diff", "--no-textconv", base, replacement)
	if err != nil || diff == "" {
		t.Fatalf("diff followed replacement object: diff=%q err=%v", diff, err)
	}
	tree, err := git(context.Background(), repository, "ls-tree", "-r", "--name-only", base)
	if err != nil || strings.Contains(tree, "replacement-only.txt") {
		t.Fatalf("ls-tree followed replacement object: tree=%q err=%v", tree, err)
	}

	worktree := filepath.Join(t.TempDir(), "exact-base")
	if _, err := addWorktree(context.Background(), repository, worktree, base, ""); err != nil {
		t.Fatalf("materialize original commit: %v", err)
	}
	t.Cleanup(func() { _, _ = git(context.Background(), repository, "worktree", "remove", "--force", worktree) })
	materialized, err := os.ReadFile(filepath.Join(worktree, filepath.FromSlash(skill.InstructionPath)))
	if err != nil || !bytes.Equal(materialized, original) {
		t.Fatalf("worktree followed replacement object: contents=%q err=%v", materialized, err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "replacement-only.txt")); !os.IsNotExist(err) {
		t.Fatalf("replacement-only file was materialized: %v", err)
	}
}

func TestWorktreeMaterializationDisablesConfiguredFilters(t *testing.T) {
	repository, skill := newTestRepository(t)
	writeTestFile(t, filepath.Join(repository, ".gitattributes"), []byte("*.md filter=expand\n"), 0o644)
	testGit(t, repository, "add", ".gitattributes")
	testGit(t, repository, "commit", "-m", "test: add checkout filter")

	marker := filepath.Join(t.TempDir(), "filter-invoked")
	filterScript := filepath.Join(t.TempDir(), "expand-filter.sh")
	writeTestFile(t, filterScript, []byte("#!/bin/sh\nprintf invoked > \"$SKILLLOOP_FILTER_MARKER\"\ncat\nprintf '\\nFILTER_EXPANDED_CONTENT\\n'\n"), 0o755)
	t.Setenv("SKILLLOOP_FILTER_MARKER", marker)
	testGit(t, repository, "config", "filter.expand.clean", shellSingleQuote(filterScript))
	testGit(t, repository, "config", "filter.expand.smudge", shellSingleQuote(filterScript))
	testGit(t, repository, "config", "filter.expand.required", "true")
	t.Setenv("SKILLLOOP_TEST_RUNNER", "1")
	service := Service{
		StateDir: t.TempDir(),
		Runner: Runner{
			Argv:        []string{os.Args[0], "-test.run=^TestExternalRunnerHelper$"},
			Timeout:     5 * time.Second,
			OutputLimit: 512,
		},
	}
	candidate, err := service.Prepare(context.Background(), skill, testCluster(skill.ID))
	if err != nil {
		t.Fatalf("prepare with configured filter: %v", err)
	}
	t.Cleanup(func() { _ = service.Cleanup(context.Background(), candidate) })
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("checkout filter ran during Prepare: %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(candidate.WorktreePath, filepath.FromSlash(skill.InstructionPath)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), "FILTER_EXPANDED_CONTENT") {
		t.Fatal("candidate worktree contains smudge-expanded content")
	}

	evaluation, err := service.Evaluate(context.Background(), candidate)
	if err != nil {
		t.Fatalf("evaluate with configured filter: %v", err)
	}
	if !evaluation.Passed {
		t.Fatalf("evaluation with raw worktrees failed: %#v", evaluation.Checks)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("checkout filter ran during exact-pair evaluation: %v", err)
	}
}

func TestGitDirectoryBootstrapPayloadBoundary(t *testing.T) {
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = directory.Close() }()
	path := "/usr/bin/git"
	empty, err := json.Marshal(gitBootstrapCommand{Path: path, Argv: []string{"git", ""}})
	if err != nil {
		t.Fatal(err)
	}
	exactArgument := strings.Repeat("a", maxGitBootstrapArgvBytes-len(empty))
	exact := &exec.Cmd{Path: path, Args: []string{"git", exactArgument}}
	if _, err := commandInDirectory(context.Background(), directory, exact); err != nil {
		t.Fatalf("exact bootstrap payload limit rejected: %v", err)
	}
	over := &exec.Cmd{Path: path, Args: []string{"git", exactArgument + "a"}}
	if _, err := commandInDirectory(context.Background(), directory, over); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("oversized bootstrap payload error = %v, want ErrResourceLimit", err)
	}
}

func descendantPID(output string) (int, error) {
	for line := range strings.SplitSeq(output, "\n") {
		if raw, found := strings.CutPrefix(strings.TrimSpace(line), "pid="); found {
			return strconv.Atoi(raw)
		}
	}
	return 0, errors.New("pid line not found")
}

func assertDescendantStopped(t *testing.T, pid int, marker string) {
	t.Helper()
	assertProcessStopped(t, pid)
	time.Sleep(time.Second)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("descendant performed a side effect after runner returned: %v", err)
	}
}

func assertProcessStopped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("descendant process %d still exists after runner returned: %v", pid, err)
	}
}

func assertMarkerAbsent(t *testing.T, marker string) {
	t.Helper()
	time.Sleep(time.Second)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("contained process performed a side effect: %v", err)
	}
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func TestMaterializeReleaseArchivesOnlyOwnedSkillFiles(t *testing.T) {
	repository := t.TempDir()
	testGit(t, repository, "init", "-b", "main")
	testGit(t, repository, "config", "user.name", "Test User")
	testGit(t, repository, "config", "user.email", "test@example.invalid")
	writeTestFile(t, filepath.Join(repository, "SKILL.md"), []byte("# Root skill\n"), 0o644)
	writeTestFile(t, filepath.Join(repository, "scripts", "check.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	writeTestFile(t, filepath.Join(repository, "assets", "example.txt"), []byte("example\n"), 0o644)
	writeTestFile(t, filepath.Join(repository, "private.txt"), []byte("must not be released\n"), 0o644)
	testGit(t, repository, "add", ".")
	testGit(t, repository, "commit", "-m", "feat: add root skill")
	revision := testGit(t, repository, "rev-parse", "HEAD")
	skill := domain.Skill{ID: "root-skill", RepositoryPath: repository, InstructionPath: "SKILL.md", Enabled: true}
	stateDir := t.TempDir()
	t.Cleanup(func() { makeTestTreeWritable(stateDir) })

	release, err := (Service{StateDir: stateDir}).materializeRelease(context.Background(), repository, skill, revision, "SKILL.md")
	if err != nil {
		t.Fatalf("materialize scoped release: %v", err)
	}
	for _, owned := range []string{"SKILL.md", "scripts/check.sh", "assets/example.txt"} {
		if _, err := os.Stat(filepath.Join(release, filepath.FromSlash(owned))); err != nil {
			t.Fatalf("owned release file %s missing: %v", owned, err)
		}
	}
	if _, err := os.Stat(filepath.Join(release, "private.txt")); !os.IsNotExist(err) {
		t.Fatalf("unowned repository file was released: %v", err)
	}
	tamperedScript := filepath.Join(release, "scripts", "check.sh")
	if err := os.Chmod(tamperedScript, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tamperedScript, []byte("#!/bin/sh\nexit 42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tamperedScript, 0o555); err != nil {
		t.Fatal(err)
	}
	if _, err := (Service{StateDir: stateDir}).materializeRelease(context.Background(), repository, skill, revision, "SKILL.md"); !errors.Is(err, ErrDrift) {
		t.Fatalf("read-only altered script error = %v, want ErrDrift", err)
	}
}

func TestMaterializeReleaseRejectsLegacyUnscopedSnapshot(t *testing.T) {
	repository, skill := newTestRepository(t)
	revision := testGit(t, repository, "rev-parse", "HEAD")
	stateDir := t.TempDir()
	t.Cleanup(func() { makeTestTreeWritable(stateDir) })
	target := filepath.Join(stateDir, "releases", skill.ID, revision)
	writeTestFile(t, filepath.Join(target, "SKILL.md"), []byte("---\nname: demo\ndescription: Demonstration skill\n---\n\n# Demo\n\nFollow the documented workflow.\n"), 0o444)
	writeTestFile(t, filepath.Join(target, "README.md"), []byte("legacy unowned file\n"), 0o444)
	if err := os.Chmod(target, 0o555); err != nil {
		t.Fatal(err)
	}

	_, err := (Service{StateDir: stateDir}).materializeRelease(context.Background(), repository, skill, revision, skill.InstructionPath)
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("legacy unscoped release error = %v, want ErrUnsafePath", err)
	}
}

func TestReleaseArchiveLimits(t *testing.T) {
	tests := []struct {
		name   string
		files  map[string]string
		limits releaseExtractionLimits
	}{
		{name: "members", files: map[string]string{"SKILL.md": "a", "scripts/x": "b"}, limits: releaseExtractionLimits{members: 1, memberBytes: 10, totalBytes: 10}},
		{name: "member bytes", files: map[string]string{"SKILL.md": "abcde"}, limits: releaseExtractionLimits{members: 2, memberBytes: 4, totalBytes: 10}},
		{name: "total bytes", files: map[string]string{"SKILL.md": "abc", "scripts/x": "def"}, limits: releaseExtractionLimits{members: 3, memberBytes: 4, totalBytes: 5}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archive := testTar(t, test.files)
			destination, openErr := openStateDirectory(t.TempDir())
			if openErr != nil {
				t.Fatal(openErr)
			}
			defer func() { _ = destination.Close() }()
			err := extractArchiveAtWithLimits(archive, destination.file, "", test.limits)
			if !errors.Is(err, ErrResourceLimit) {
				t.Fatalf("extract error = %v, want ErrResourceLimit", err)
			}
		})
	}

	destination, err := openStateDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = destination.Close() }()
	err = extractArchiveAtWithLimits(testTar(t, map[string]string{"SKILL.md": "ok", "scripts/x": "ok"}), destination.file, "", releaseExtractionLimits{members: 4, memberBytes: 4, totalBytes: 8})
	if err != nil {
		t.Fatalf("legitimate archive rejected: %v", err)
	}
}

func TestGitArchiveOutputIsBounded(t *testing.T) {
	repository, _ := newTestRepository(t)
	revision := testGit(t, repository, "rev-parse", "HEAD")
	_, err := gitBytesLimit(context.Background(), repository, 100, "archive", "--format=tar", revision, "--", "README.md")
	if !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("archive output limit error = %v, want ErrResourceLimit", err)
	}
}

func TestGitDiffAndNameOnlyOutputAreBounded(t *testing.T) {
	t.Run("diff", func(t *testing.T) {
		repository, skill := newTestRepository(t)
		base := testGit(t, repository, "rev-parse", "HEAD")
		large := strings.Repeat("changed guidance line\n", 4_000)
		writeTestFile(t, filepath.Join(repository, filepath.FromSlash(skill.InstructionPath)), []byte(large), 0o644)
		testGit(t, repository, "add", "--", skill.InstructionPath)
		testGit(t, repository, "commit", "-m", "test: add oversized diff")
		candidate := testGit(t, repository, "rev-parse", "HEAD")

		_, err := git(context.Background(), repository, "diff", "--no-ext-diff", "--no-textconv", base, candidate, "--", skill.InstructionPath)
		if !errors.Is(err, ErrResourceLimit) || !strings.Contains(err.Error(), "stdout") {
			t.Fatalf("oversized diff error = %v, want bounded stdout ErrResourceLimit", err)
		}
		if _, err := gitBytes(context.Background(), repository, "show", candidate+":"+skill.InstructionPath); !errors.Is(err, ErrResourceLimit) {
			t.Fatalf("unbounded gitBytes output error = %v, want ErrResourceLimit", err)
		}
		contents, err := gitBytesLimit(context.Background(), repository, int64(len(large)), "show", candidate+":"+skill.InstructionPath)
		if err != nil || string(contents) != large {
			t.Fatalf("explicitly bounded legitimate blob: bytes=%d err=%v", len(contents), err)
		}
	})

	t.Run("name-only", func(t *testing.T) {
		repository, _ := newTestRepository(t)
		base := testGit(t, repository, "rev-parse", "HEAD")
		for index := range 400 {
			name := fmt.Sprintf("%04d-%s.txt", index, strings.Repeat("n", 180))
			writeTestFile(t, filepath.Join(repository, "many", name), []byte("changed\n"), 0o644)
		}
		testGit(t, repository, "add", "--", "many")
		testGit(t, repository, "commit", "-m", "test: add many long paths")
		candidate := testGit(t, repository, "rev-parse", "HEAD")

		_, err := git(context.Background(), repository, "diff", "--no-ext-diff", "--no-textconv", "--name-only", base, candidate)
		if !errors.Is(err, ErrResourceLimit) || !strings.Contains(err.Error(), "stdout") {
			t.Fatalf("oversized name-only error = %v, want bounded stdout ErrResourceLimit", err)
		}
		if head, err := git(context.Background(), repository, "rev-parse", "HEAD^{commit}"); err != nil || head != candidate {
			t.Fatalf("normal bounded Git command failed after rejection: head=%q err=%v", head, err)
		}
	})
}

func TestGitStderrLimitKillsAndWaits(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "survived")
	t.Setenv("SKILLLOOP_GIT_OUTPUT_HELPER", "stderr")
	t.Setenv("SKILLLOOP_GIT_OUTPUT_MARKER", marker)
	command := exec.Command(os.Args[0], "-test.run=^TestGitOutputLimitProcessHelper$")
	_, err := runGitCommandBytes(command, []string{"output-limit-helper"}, 1024, 1024)
	if !errors.Is(err, ErrResourceLimit) || !strings.Contains(err.Error(), "stderr") {
		t.Fatalf("oversized stderr error = %v, want bounded stderr ErrResourceLimit", err)
	}
	if command.ProcessState == nil {
		t.Fatalf("oversized stderr process was not waited: %#v", command.ProcessState)
	}
	time.Sleep(time.Second)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("oversized stderr process survived limit enforcement: %v", err)
	}
}

func TestGitOutputLimitProcessHelper(t *testing.T) {
	if os.Getenv("SKILLLOOP_GIT_OUTPUT_HELPER") != "stderr" {
		return
	}
	_, _ = fmt.Fprint(os.Stderr, strings.Repeat("e", 2048))
	time.Sleep(500 * time.Millisecond)
	_ = os.WriteFile(os.Getenv("SKILLLOOP_GIT_OUTPUT_MARKER"), []byte("survived\n"), 0o600)
}

func TestSkillFileReadIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SKILL.md")
	writeTestFile(t, path, []byte("1234"), 0o644)
	contents, err := readFileLimit(path, 4)
	if err != nil || string(contents) != "1234" {
		t.Fatalf("exact file limit rejected: contents=%q err=%v", contents, err)
	}
	writeTestFile(t, path, []byte("12345"), 0o644)
	if _, err := readFileLimit(path, 4); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("oversized skill error = %v, want ErrResourceLimit", err)
	}
}

func TestSwitchReleasePairCompensatesSecondFailure(t *testing.T) {
	rootPath := t.TempDir()
	oldPrevious := strings.Repeat("a", 40)
	oldCurrent := strings.Repeat("b", 40)
	newCurrent := strings.Repeat("c", 40)
	for _, revision := range []string{oldPrevious, oldCurrent, newCurrent} {
		if err := os.Mkdir(filepath.Join(rootPath, revision), 0o555); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(oldPrevious, filepath.Join(rootPath, "previous")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(oldCurrent, filepath.Join(rootPath, "current")); err != nil {
		t.Fatal(err)
	}
	root, err := openStateDirectory(filepath.Dir(rootPath), filepath.Base(rootPath))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	calls := 0
	err = switchReleasePairAtWithHooks(root, oldCurrent, newCurrent, func(root *stateDirectory, name, revision string) error {
		calls++
		if calls == 2 {
			return errors.New("injected current switch failure")
		}
		return atomicSymlinkAt(root, name, revision)
	}, removeReleaseTransitionJournalAt)
	if err == nil {
		t.Fatal("second switch failure was accepted")
	}
	assertReleaseLink(t, rootPath, "previous", oldPrevious)
	assertReleaseLink(t, rootPath, "current", oldCurrent)

	err = switchReleasePairAtWithHooks(root, oldCurrent, newCurrent, atomicSymlinkAt, func(root *stateDirectory) error {
		if err := removeReleaseTransitionJournalAt(root); err != nil {
			return err
		}
		return errors.New("injected journal sync failure")
	})
	if err == nil {
		t.Fatal("journal commit failure was accepted")
	}
	assertReleaseLink(t, rootPath, "previous", oldPrevious)
	assertReleaseLink(t, rootPath, "current", oldCurrent)

	if err := switchReleasePairAt(root, oldCurrent, newCurrent); err != nil {
		t.Fatalf("legitimate pair switch: %v", err)
	}
	assertReleaseLink(t, rootPath, "previous", oldCurrent)
	assertReleaseLink(t, rootPath, "current", newCurrent)
}

func TestSwitchReleasePairRejectsRenamedReleaseRoot(t *testing.T) {
	stateDir := t.TempDir()
	service := Service{StateDir: stateDir}
	root, err := service.openSkillReleaseRoot("identity-skill")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	oldPrevious := strings.Repeat("d", 40)
	oldCurrent := strings.Repeat("e", 40)
	newCurrent := strings.Repeat("f", 40)
	for _, revision := range []string{oldPrevious, oldCurrent, newCurrent} {
		if err := unix.Mkdirat(int(root.file.Fd()), revision, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	if err := atomicSymlinkAt(root, "previous", oldPrevious); err != nil {
		t.Fatal(err)
	}
	if err := atomicSymlinkAt(root, "current", oldCurrent); err != nil {
		t.Fatal(err)
	}
	heldRoot := root.path + "-held"
	replacementSentinel := filepath.Join(root.path, "sentinel")
	calls := 0
	err = switchReleasePairAtWithHooks(root, oldCurrent, newCurrent, func(root *stateDirectory, name, revision string) error {
		calls++
		if err := atomicSymlinkAt(root, name, revision); err != nil {
			return err
		}
		if calls != 1 {
			return nil
		}
		if err := os.Rename(root.path, heldRoot); err != nil {
			return err
		}
		if err := os.Mkdir(root.path, 0o700); err != nil {
			return err
		}
		return os.WriteFile(replacementSentinel, []byte("replacement"), 0o600)
	}, removeReleaseTransitionJournalAt)
	if !errors.Is(err, ErrDrift) {
		t.Fatalf("renamed release root transition error = %v, want ErrDrift", err)
	}
	if contents, err := os.ReadFile(replacementSentinel); err != nil || string(contents) != "replacement" {
		t.Fatalf("replacement sentinel changed: contents=%q err=%v", contents, err)
	}
	if entries, err := os.ReadDir(root.path); err != nil || len(entries) != 1 || entries[0].Name() != "sentinel" {
		t.Fatalf("replacement release root was mutated: entries=%v err=%v", entries, err)
	}
	assertReleaseLink(t, heldRoot, "previous", oldPrevious)
	assertReleaseLink(t, heldRoot, "current", oldCurrent)
}

func TestCurrentReleaseRecoversCrashAfterFirstLinkSwitch(t *testing.T) {
	repository, skill := newTestRepository(t)
	skill.ID = "crash-safe-skill"
	stateDir := t.TempDir()
	t.Cleanup(func() { makeTestTreeWritable(stateDir) })
	service := Service{StateDir: stateDir}
	rootPath := filepath.Join(stateDir, "releases", skill.ID)
	oldPrevious := testGit(t, repository, "rev-parse", "HEAD")
	instruction := filepath.Join(repository, filepath.FromSlash(skill.InstructionPath))
	writeTestFile(t, instruction, []byte("---\nname: demo\ndescription: Updated demo\n---\n\n# Demo\n"), 0o644)
	testGit(t, repository, "add", skill.InstructionPath)
	testGit(t, repository, "commit", "-m", "docs: update demo skill")
	oldCurrent := testGit(t, repository, "rev-parse", "HEAD")
	for _, revision := range []string{oldPrevious, oldCurrent} {
		if _, err := service.materializeRelease(context.Background(), repository, skill, revision, skill.InstructionPath); err != nil {
			t.Fatalf("materialize %s: %v", revision, err)
		}
	}
	if err := os.Symlink(oldPrevious, filepath.Join(rootPath, "previous")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(oldCurrent, filepath.Join(rootPath, "current")); err != nil {
		t.Fatal(err)
	}
	root, err := service.openSkillReleaseRoot(skill.ID)
	if err != nil {
		t.Fatal(err)
	}

	transition, err := snapshotReleaseTransitionAt(root)
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	if err := writeReleaseTransitionJournalAt(root, transition); err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	if err := atomicSymlinkAt(root, "previous", oldCurrent); err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	assertReleaseLink(t, rootPath, "previous", oldCurrent)
	if _, err := os.Stat(filepath.Join(rootPath, releaseTransitionJournalName)); err != nil {
		t.Fatalf("durable transition journal missing: %v", err)
	}

	release, err := service.CurrentRelease(skill)
	if err != nil {
		t.Fatalf("recover interrupted release transition: %v", err)
	}
	if release.Commit != oldCurrent {
		t.Fatalf("current release after recovery = %s, want %s", release.Commit, oldCurrent)
	}
	assertReleaseLink(t, rootPath, "previous", oldPrevious)
	assertReleaseLink(t, rootPath, "current", oldCurrent)
	if _, err := os.Stat(filepath.Join(rootPath, releaseTransitionJournalName)); !os.IsNotExist(err) {
		t.Fatalf("transition journal survived completed recovery: %v", err)
	}
}

func TestCurrentReleaseRejectsTamperedSnapshot(t *testing.T) {
	repository, skill := newTestRepository(t)
	stateDir := t.TempDir()
	t.Cleanup(func() { makeTestTreeWritable(stateDir) })
	service := Service{StateDir: stateDir}
	revision := testGit(t, repository, "rev-parse", "HEAD")
	releasePath, err := service.materializeRelease(context.Background(), repository, skill, revision, skill.InstructionPath)
	if err != nil {
		t.Fatalf("materialize release: %v", err)
	}
	root, err := service.openSkillReleaseRoot(skill.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicSymlinkAt(root, "current", revision); err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	releasedSkill := filepath.Join(releasePath, "SKILL.md")
	if err := os.Chmod(releasedSkill, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(releasedSkill, []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(releasedSkill, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CurrentRelease(skill); !errors.Is(err, ErrDrift) {
		t.Fatalf("tampered current release error = %v, want ErrDrift", err)
	}
}

func TestReleaseGuardSerializesProcesses(t *testing.T) {
	stateDir := t.TempDir()
	const skillID = "release-serialized-skill"
	root, err := (Service{StateDir: stateDir}).openSkillReleaseRoot(skillID)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := acquireReleaseGuardAt(context.Background(), root)
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}

	blocked := exec.Command(os.Args[0], "-test.run=^TestReleaseGuardProcessHelper$")
	blocked.Env = append(os.Environ(),
		"SKILLLOOP_RELEASE_LOCK_STATE="+stateDir,
		"SKILLLOOP_RELEASE_LOCK_SKILL="+skillID,
		"SKILLLOOP_RELEASE_LOCK_EXPECT=blocked",
	)
	if output, err := blocked.CombinedOutput(); err != nil {
		guard.release()
		t.Fatalf("second process did not observe held lock: %v\n%s", err, output)
	}
	guard.release()

	acquired := exec.Command(os.Args[0], "-test.run=^TestReleaseGuardProcessHelper$")
	acquired.Env = append(os.Environ(),
		"SKILLLOOP_RELEASE_LOCK_STATE="+stateDir,
		"SKILLLOOP_RELEASE_LOCK_SKILL="+skillID,
		"SKILLLOOP_RELEASE_LOCK_EXPECT=acquired",
	)
	if output, err := acquired.CombinedOutput(); err != nil {
		t.Fatalf("second process could not acquire released lock: %v\n%s", err, output)
	}
}

func TestReleaseGuardProcessHelper(t *testing.T) {
	stateDir := os.Getenv("SKILLLOOP_RELEASE_LOCK_STATE")
	if stateDir == "" {
		return
	}
	skillID := os.Getenv("SKILLLOOP_RELEASE_LOCK_SKILL")
	expectation := os.Getenv("SKILLLOOP_RELEASE_LOCK_EXPECT")
	timeout := 2 * time.Second
	if expectation == "blocked" {
		timeout = 200 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	root, err := (Service{StateDir: stateDir}).openSkillReleaseRoot(skillID)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := acquireReleaseGuardAt(ctx, root)
	if expectation == "blocked" {
		_ = root.Close()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lock error = %v, want context deadline", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("acquire released lock: %v", err)
	}
	guard.release()
}

func TestSkillFenceSerializesProcesses(t *testing.T) {
	stateDir := t.TempDir()
	service := Service{StateDir: stateDir}
	release, err := service.AcquireSkillFence(context.Background(), "serialized-skill")
	if err != nil {
		t.Fatal(err)
	}

	blocked := exec.Command(os.Args[0], "-test.run=^TestSkillFenceProcessHelper$")
	blocked.Env = append(os.Environ(),
		"SKILLLOOP_SKILL_FENCE_STATE="+stateDir,
		"SKILLLOOP_SKILL_FENCE_EXPECT=blocked",
	)
	if output, err := blocked.CombinedOutput(); err != nil {
		_ = release()
		t.Fatalf("second process did not observe held skill fence: %v\n%s", err, output)
	}
	if err := release(); err != nil {
		t.Fatalf("release skill fence: %v", err)
	}

	acquired := exec.Command(os.Args[0], "-test.run=^TestSkillFenceProcessHelper$")
	acquired.Env = append(os.Environ(),
		"SKILLLOOP_SKILL_FENCE_STATE="+stateDir,
		"SKILLLOOP_SKILL_FENCE_EXPECT=acquired",
	)
	if output, err := acquired.CombinedOutput(); err != nil {
		t.Fatalf("second process could not acquire released skill fence: %v\n%s", err, output)
	}
}

func TestSkillFenceProcessHelper(t *testing.T) {
	stateDir := os.Getenv("SKILLLOOP_SKILL_FENCE_STATE")
	if stateDir == "" {
		return
	}
	expectation := os.Getenv("SKILLLOOP_SKILL_FENCE_EXPECT")
	timeout := 2 * time.Second
	if expectation == "blocked" {
		timeout = 200 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	release, err := (Service{StateDir: stateDir}).AcquireSkillFence(ctx, "serialized-skill")
	if expectation == "blocked" {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("skill fence error=%v, want context deadline", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("acquire released skill fence: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release acquired skill fence: %v", err)
	}
}

func testTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var contents bytes.Buffer
	writer := tar.NewWriter(&contents)
	for name, value := range files {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(value)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return contents.Bytes()
}

func assertReleaseLink(t *testing.T, root, name, want string) {
	t.Helper()
	got, err := os.Readlink(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s link = %s, want %s", name, got, want)
	}
}
