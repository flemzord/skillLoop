package improvement

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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
	if parseErr != nil {
		t.Fatalf("parse timed out descendant PID from %q: %v", result.Output, parseErr)
	}
	assertDescendantStopped(t, pid, marker)
}

func TestProcessGroupTerminationIsIdempotent(t *testing.T) {
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
		t.Fatalf("process group kill calls = %d, want 1", calls)
	}
}

func TestExternalRunnerPipeDescendantHelper(t *testing.T) {
	switch os.Getenv("SKILLLOOP_DESCENDANT_HELPER") {
	case "parent", "timeout-parent":
		mode := os.Getenv("SKILLLOOP_DESCENDANT_HELPER")
		command := exec.Command(os.Args[0], "-test.run=^TestExternalRunnerPipeDescendantHelper$")
		command.Env = append(os.Environ(), "SKILLLOOP_DESCENDANT_HELPER=child")
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Printf("pid=%d\n", command.Process.Pid)
		if mode == "timeout-parent" {
			time.Sleep(30 * time.Second)
		}
		os.Exit(1)
	case "child":
		time.Sleep(750 * time.Millisecond)
		if marker := os.Getenv("SKILLLOOP_DESCENDANT_MARKER"); marker != "" {
			_ = os.WriteFile(marker, []byte("descendant survived\n"), 0o600)
		}
		time.Sleep(30 * time.Second)
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
	time.Sleep(time.Second)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("descendant performed a side effect after runner returned: %v", err)
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
		limits archiveLimits
	}{
		{name: "members", files: map[string]string{"SKILL.md": "a", "scripts/x": "b"}, limits: archiveLimits{members: 1, memberBytes: 10, totalBytes: 10}},
		{name: "member bytes", files: map[string]string{"SKILL.md": "abcde"}, limits: archiveLimits{members: 2, memberBytes: 4, totalBytes: 10}},
		{name: "total bytes", files: map[string]string{"SKILL.md": "abc", "scripts/x": "def"}, limits: archiveLimits{members: 3, memberBytes: 4, totalBytes: 5}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archive := testTar(t, test.files)
			err := extractArchiveWithLimits(archive, t.TempDir(), "", test.limits)
			if !errors.Is(err, ErrResourceLimit) {
				t.Fatalf("extract error = %v, want ErrResourceLimit", err)
			}
		})
	}

	destination := t.TempDir()
	err := extractArchiveWithLimits(testTar(t, map[string]string{"SKILL.md": "ok", "scripts/x": "ok"}), destination, "", archiveLimits{members: 4, memberBytes: 4, totalBytes: 8})
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
	root := t.TempDir()
	oldPrevious := strings.Repeat("a", 40)
	oldCurrent := strings.Repeat("b", 40)
	newCurrent := strings.Repeat("c", 40)
	for _, revision := range []string{oldPrevious, oldCurrent, newCurrent} {
		if err := os.Mkdir(filepath.Join(root, revision), 0o555); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(oldPrevious, filepath.Join(root, "previous")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(oldCurrent, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}

	calls := 0
	err := switchReleasePair(root, oldCurrent, newCurrent, func(root, name, revision string) error {
		calls++
		if calls == 2 {
			return errors.New("injected current switch failure")
		}
		return atomicSymlink(root, name, revision)
	})
	if err == nil {
		t.Fatal("second switch failure was accepted")
	}
	assertReleaseLink(t, root, "previous", oldPrevious)
	assertReleaseLink(t, root, "current", oldCurrent)

	err = switchReleasePairWithJournal(root, oldCurrent, newCurrent, atomicSymlink, func(root string) error {
		if err := os.Remove(filepath.Join(root, releaseTransitionJournalName)); err != nil {
			return err
		}
		return errors.New("injected journal sync failure")
	})
	if err == nil {
		t.Fatal("journal commit failure was accepted")
	}
	assertReleaseLink(t, root, "previous", oldPrevious)
	assertReleaseLink(t, root, "current", oldCurrent)

	if err := switchReleasePair(root, oldCurrent, newCurrent, atomicSymlink); err != nil {
		t.Fatalf("legitimate pair switch: %v", err)
	}
	assertReleaseLink(t, root, "previous", oldCurrent)
	assertReleaseLink(t, root, "current", newCurrent)
}

func TestCurrentReleaseRecoversCrashAfterFirstLinkSwitch(t *testing.T) {
	repository, skill := newTestRepository(t)
	skill.ID = "crash-safe-skill"
	stateDir := t.TempDir()
	t.Cleanup(func() { makeTestTreeWritable(stateDir) })
	service := Service{StateDir: stateDir}
	root := filepath.Join(stateDir, "releases", skill.ID)
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
	if err := os.Symlink(oldPrevious, filepath.Join(root, "previous")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(oldCurrent, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}

	transition, err := snapshotReleaseTransition(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeReleaseTransitionJournal(root, transition); err != nil {
		t.Fatal(err)
	}
	if err := atomicSymlink(root, "previous", oldCurrent); err != nil {
		t.Fatal(err)
	}
	assertReleaseLink(t, root, "previous", oldCurrent)
	if _, err := os.Stat(filepath.Join(root, releaseTransitionJournalName)); err != nil {
		t.Fatalf("durable transition journal missing: %v", err)
	}

	release, err := service.CurrentRelease(skill)
	if err != nil {
		t.Fatalf("recover interrupted release transition: %v", err)
	}
	if release.Commit != oldCurrent {
		t.Fatalf("current release after recovery = %s, want %s", release.Commit, oldCurrent)
	}
	assertReleaseLink(t, root, "previous", oldPrevious)
	assertReleaseLink(t, root, "current", oldCurrent)
	if _, err := os.Stat(filepath.Join(root, releaseTransitionJournalName)); !os.IsNotExist(err) {
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
	root, err := service.skillReleaseRoot(skill.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicSymlink(root, "current", revision); err != nil {
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
	root := t.TempDir()
	guard, err := acquireReleaseGuard(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}

	blocked := exec.Command(os.Args[0], "-test.run=^TestReleaseGuardProcessHelper$")
	blocked.Env = append(os.Environ(),
		"SKILLLOOP_RELEASE_LOCK_ROOT="+root,
		"SKILLLOOP_RELEASE_LOCK_EXPECT=blocked",
	)
	if output, err := blocked.CombinedOutput(); err != nil {
		guard.release()
		t.Fatalf("second process did not observe held lock: %v\n%s", err, output)
	}
	guard.release()

	acquired := exec.Command(os.Args[0], "-test.run=^TestReleaseGuardProcessHelper$")
	acquired.Env = append(os.Environ(),
		"SKILLLOOP_RELEASE_LOCK_ROOT="+root,
		"SKILLLOOP_RELEASE_LOCK_EXPECT=acquired",
	)
	if output, err := acquired.CombinedOutput(); err != nil {
		t.Fatalf("second process could not acquire released lock: %v\n%s", err, output)
	}
}

func TestReleaseGuardProcessHelper(t *testing.T) {
	root := os.Getenv("SKILLLOOP_RELEASE_LOCK_ROOT")
	if root == "" {
		return
	}
	expectation := os.Getenv("SKILLLOOP_RELEASE_LOCK_EXPECT")
	timeout := 2 * time.Second
	if expectation == "blocked" {
		timeout = 200 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	guard, err := acquireReleaseGuard(ctx, root)
	if expectation == "blocked" {
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
