package improvement

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStatePathsRejectSymlinkAncestors(t *testing.T) {
	for _, component := range []string{"worktrees", "evaluations", "releases"} {
		t.Run(component, func(t *testing.T) {
			stateDir := t.TempDir()
			outside := t.TempDir()
			if err := os.Symlink(outside, filepath.Join(stateDir, component)); err != nil {
				t.Fatal(err)
			}

			directory, err := openStateDirectory(stateDir, component, "owned-skill")
			if directory != nil {
				_ = directory.Close()
			}
			if !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("open state directory through symlink error = %v, want ErrUnsafePath", err)
			}
			entries, readErr := os.ReadDir(outside)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("symlink target was modified: %#v", entries)
			}
		})
	}
}

func TestStatePathsCanonicalizeStableAncestorAlias(t *testing.T) {
	parent := t.TempDir()
	outside := t.TempDir()
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(outside, alias); err != nil {
		t.Fatal(err)
	}
	directory, err := openStateDirectory(filepath.Join(alias, "state"), "worktrees")
	if err != nil {
		t.Fatalf("open state directory through stable ancestor alias: %v", err)
	}
	defer func() { _ = directory.Close() }()
	canonicalOutside, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(canonicalOutside, "state", "worktrees")
	if directory.path != want {
		t.Fatalf("canonical state path = %s, want %s", directory.path, want)
	}
}

func TestStatePathsRejectConfiguredRootSymlink(t *testing.T) {
	parent := t.TempDir()
	outside := t.TempDir()
	stateDir := filepath.Join(parent, "state")
	if err := os.Symlink(outside, stateDir); err != nil {
		t.Fatal(err)
	}
	directory, err := openStateDirectory(stateDir, "worktrees")
	if directory != nil {
		_ = directory.Close()
	}
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("open symlinked configured state root error = %v, want ErrUnsafePath", err)
	}
	assertEmptyDirectory(t, outside)
}

func TestStatePathsAllowLegitimateNestedStateDirectory(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "nested", "state")
	directory, err := openStateDirectory(stateDir, "worktrees", "owned-skill")
	if err != nil {
		t.Fatalf("open legitimate state directory: %v", err)
	}
	defer func() { _ = directory.Close() }()
	name, path, err := createStateChild(directory, "candidate-")
	if err != nil {
		t.Fatalf("create legitimate state child: %v", err)
	}
	child, err := openStateChild(directory, name)
	if err != nil {
		t.Fatalf("open legitimate state child: %v", err)
	}
	_ = child.Close()
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("legitimate state child is unavailable: info=%v err=%v", info, err)
	}
}

func TestStateDirectorySecuresEveryLogicalComponent(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	paths := []string{
		stateDir,
		filepath.Join(stateDir, "worktrees"),
		filepath.Join(stateDir, "worktrees", "owned-skill"),
	}
	for _, path := range paths {
		if err := os.Mkdir(path, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	directory, err := openStateDirectory(stateDir, "worktrees", "owned-skill")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = directory.Close() }()
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("logical state component %s mode = %o, want 700", path, info.Mode().Perm())
		}
	}
}

func TestStateChildIdentityRejectsRenamedNamespace(t *testing.T) {
	stateDir := t.TempDir()
	parent, err := openStateDirectory(stateDir, "worktrees", "owned-skill")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parent.Close() }()
	name, _, err := createStateChild(parent, "candidate-")
	if err != nil {
		t.Fatal(err)
	}
	child, err := openStateChild(parent, name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Close() }()
	held := parent.path + "-held"
	if err := os.Rename(parent.path, held); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent.path, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(parent.path, "sentinel")
	if err := os.WriteFile(sentinel, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := verifyStateChildIdentity(parent, name, child); !errors.Is(err, ErrDrift) {
		t.Fatalf("renamed namespace verification error = %v, want ErrDrift", err)
	}
	if contents, err := os.ReadFile(sentinel); err != nil || string(contents) != "replacement" {
		t.Fatalf("replacement sentinel changed: contents=%q err=%v", contents, err)
	}
}

func TestPrepareRejectsSymlinkedWorktreeAncestor(t *testing.T) {
	_, skill := newTestRepository(t)
	stateDir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(stateDir, "worktrees")); err != nil {
		t.Fatal(err)
	}
	service := Service{StateDir: stateDir}

	_, err := service.Prepare(context.Background(), skill, testCluster(skill.ID))
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("prepare through symlink error = %v, want ErrUnsafePath", err)
	}
	assertEmptyDirectory(t, outside)
}

func TestEvaluateRejectsSymlinkedEvaluationAncestor(t *testing.T) {
	_, skill := newTestRepository(t)
	stateDir := t.TempDir()
	service := testServiceWithExternalRunner(t, stateDir)
	candidate, err := service.Prepare(context.Background(), skill, testCluster(skill.ID))
	if err != nil {
		t.Fatalf("prepare candidate: %v", err)
	}
	defer func() { _ = service.Cleanup(context.Background(), candidate) }()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(stateDir, "evaluations")); err != nil {
		t.Fatal(err)
	}

	_, err = service.Evaluate(context.Background(), candidate)
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("evaluate through symlink error = %v, want ErrUnsafePath", err)
	}
	assertEmptyDirectory(t, outside)
}

func TestMaterializeReleaseRejectsSymlinkedReleaseAncestor(t *testing.T) {
	repository, skill := newTestRepository(t)
	stateDir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(stateDir, "releases")); err != nil {
		t.Fatal(err)
	}
	revision := testGit(t, repository, "rev-parse", "HEAD^{commit}")

	_, err := (Service{StateDir: stateDir}).materializeRelease(context.Background(), repository, skill, revision, skill.InstructionPath)
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("materialize release through symlink error = %v, want ErrUnsafePath", err)
	}
	assertEmptyDirectory(t, outside)
}

func TestCleanupRejectsReplacedWorktreeAncestor(t *testing.T) {
	_, skill := newTestRepository(t)
	stateDir := t.TempDir()
	service := Service{StateDir: stateDir}
	candidate, err := service.Prepare(context.Background(), skill, testCluster(skill.ID))
	if err != nil {
		t.Fatalf("prepare candidate: %v", err)
	}
	skillRoot := filepath.Join(stateDir, "worktrees", safeName(skill.ID))
	heldRoot := skillRoot + "-held"
	if err := os.Rename(skillRoot, heldRoot); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, skillRoot); err != nil {
		t.Fatal(err)
	}

	err = service.Cleanup(context.Background(), candidate)
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("cleanup after ancestor replacement error = %v, want ErrUnsafePath", err)
	}
	if contents, err := os.ReadFile(sentinel); err != nil || string(contents) != "preserve" {
		t.Fatalf("outside sentinel changed: contents=%q err=%v", contents, err)
	}
	if _, err := os.Stat(filepath.Join(heldRoot, filepath.Base(candidate.WorktreePath))); err != nil {
		t.Fatalf("authenticated worktree was unexpectedly removed: %v", err)
	}

	if err := os.Remove(skillRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(heldRoot, skillRoot); err != nil {
		t.Fatal(err)
	}
	if err := service.Cleanup(context.Background(), candidate); err != nil {
		t.Fatalf("cleanup after restoring legitimate ancestor: %v", err)
	}
}

func assertEmptyDirectory(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("directory %s is not empty: %#v", path, entries)
	}
}
