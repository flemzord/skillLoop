package activation

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flemzord/skillloop/internal/domain"
)

func TestInstallAndUninstallBothPlatformsAreIdempotent(t *testing.T) {
	dataDir := t.TempDir()
	home := t.TempDir()
	skill := domain.Skill{ID: "skill-demo", Name: "My Demo.Skill", Enabled: true}
	current := makeCurrentRelease(t, dataDir, &skill)
	service := Service{DataDir: dataDir, HomeDir: home}

	results, err := service.Install(skill, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(results) != 2 || !results[0].Changed || !results[1].Changed {
		t.Fatalf("unexpected install results: %#v", results)
	}
	for _, destination := range []string{
		filepath.Join(home, ".codex", "skills", "my-demo-skill"),
		filepath.Join(home, ".claude", "skills", "my-demo-skill"),
	} {
		target, err := os.Readlink(destination)
		if err != nil {
			t.Fatalf("read installed link %s: %v", destination, err)
		}
		if target != current {
			t.Fatalf("link %s targets %q, want %q", destination, target, current)
		}
	}

	results, err = service.Install(skill, nil)
	if err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if results[0].Changed || results[1].Changed {
		t.Fatalf("reinstall should be idempotent: %#v", results)
	}

	results, err = service.Uninstall(skill, nil)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !results[0].Changed || !results[1].Changed {
		t.Fatalf("unexpected uninstall results: %#v", results)
	}
	results, err = service.Uninstall(skill, nil)
	if err != nil {
		t.Fatalf("second uninstall: %v", err)
	}
	if results[0].Changed || results[1].Changed {
		t.Fatalf("second uninstall should be idempotent: %#v", results)
	}
}

func TestInstallCanTargetOnePlatform(t *testing.T) {
	dataDir := t.TempDir()
	home := t.TempDir()
	skill := domain.Skill{ID: "skill-demo", Name: "demo", Enabled: true}
	makeCurrentRelease(t, dataDir, &skill)
	service := Service{DataDir: dataDir, HomeDir: home}

	results, err := service.Install(skill, []Platform{PlatformCodex})
	if err != nil {
		t.Fatalf("install Codex only: %v", err)
	}
	if len(results) != 1 || results[0].Platform != PlatformCodex || !results[0].Changed {
		t.Fatalf("unexpected results: %#v", results)
	}
	if _, err := os.Lstat(filepath.Join(home, ".claude", "skills", "demo")); !os.IsNotExist(err) {
		t.Fatalf("Claude destination unexpectedly exists: %v", err)
	}
}

func TestInstallRefusesAnyUnmanagedDestinationBeforeChangingOthers(t *testing.T) {
	dataDir := t.TempDir()
	home := t.TempDir()
	skill := domain.Skill{ID: "skill-demo", Name: "demo", Enabled: true}
	makeCurrentRelease(t, dataDir, &skill)
	claudeDestination := filepath.Join(home, ".claude", "skills", "demo")
	if err := os.MkdirAll(filepath.Dir(claudeDestination), 0o700); err != nil {
		t.Fatalf("create Claude skills directory: %v", err)
	}
	if err := os.WriteFile(claudeDestination, []byte("user-owned"), 0o600); err != nil {
		t.Fatalf("create unmanaged destination: %v", err)
	}

	_, err := (Service{DataDir: dataDir, HomeDir: home}).Install(skill, nil)
	if !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("expected ErrUnmanaged, got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".codex", "skills", "demo")); !os.IsNotExist(err) {
		t.Fatalf("Codex link was created despite failed preflight: %v", err)
	}
	contents, err := os.ReadFile(claudeDestination)
	if err != nil || string(contents) != "user-owned" {
		t.Fatalf("unmanaged destination changed: contents=%q err=%v", contents, err)
	}
}

func TestUninstallRemovesOnlyExactManagedLink(t *testing.T) {
	dataDir := t.TempDir()
	home := t.TempDir()
	skill := domain.Skill{ID: "skill-demo", Name: "demo", Enabled: true}
	current := makeCurrentRelease(t, dataDir, &skill)
	codexDestination := filepath.Join(home, ".codex", "skills", "demo")
	claudeDestination := filepath.Join(home, ".claude", "skills", "demo")
	for _, destination := range []string{codexDestination, claudeDestination} {
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			t.Fatalf("create skills directory: %v", err)
		}
	}
	if err := os.Symlink(current, codexDestination); err != nil {
		t.Fatalf("create managed link: %v", err)
	}
	if err := os.Symlink(filepath.Join(dataDir, "someone-else"), claudeDestination); err != nil {
		t.Fatalf("create unmanaged link: %v", err)
	}

	_, err := (Service{DataDir: dataDir, HomeDir: home}).Uninstall(skill, nil)
	if !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("expected ErrUnmanaged, got %v", err)
	}
	if target, err := os.Readlink(codexDestination); err != nil || target != current {
		t.Fatalf("managed link removed despite failed preflight: target=%q err=%v", target, err)
	}
	if target, err := os.Readlink(claudeDestination); err != nil || target == current {
		t.Fatalf("unmanaged link changed: target=%q err=%v", target, err)
	}
}

func TestInstallRequiresValidCurrentRelease(t *testing.T) {
	service := Service{DataDir: t.TempDir(), HomeDir: t.TempDir()}
	skill := domain.Skill{ID: "skill-demo", Name: "demo", Enabled: true}
	if _, err := service.Install(skill, nil); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("expected ErrNoRelease, got %v", err)
	}
}

func TestSafeName(t *testing.T) {
	for _, testCase := range []struct {
		input    string
		expected string
	}{
		{input: "Demo Skill", expected: "demo-skill"},
		{input: "  DEMO___skill  ", expected: "demo-skill"},
		{input: "Compétence Go", expected: "comp-tence-go"},
	} {
		actual, err := SafeName(testCase.input)
		if err != nil {
			t.Fatalf("safe name %q: %v", testCase.input, err)
		}
		if actual != testCase.expected {
			t.Fatalf("safe name %q = %q, want %q", testCase.input, actual, testCase.expected)
		}
	}
	for _, unsafe := range []string{"", ".", "../escape", `..\escape`, string([]byte{'a', 0, 'b'}), "🔒"} {
		if _, err := SafeName(unsafe); !errors.Is(err, ErrUnsafeName) {
			t.Fatalf("unsafe name %q returned %v", unsafe, err)
		}
	}
}

func makeCurrentRelease(t *testing.T, dataDir string, skill *domain.Skill) string {
	t.Helper()
	repository := t.TempDir()
	runActivationGit(t, repository, "init", "-b", "main")
	runActivationGit(t, repository, "config", "user.name", "Test User")
	runActivationGit(t, repository, "config", "user.email", "test@example.invalid")
	contents := []byte("# Demo\n")
	if err := os.WriteFile(filepath.Join(repository, "SKILL.md"), contents, 0o644); err != nil {
		t.Fatalf("write repository skill: %v", err)
	}
	runActivationGit(t, repository, "add", "SKILL.md")
	runActivationGit(t, repository, "commit", "-m", "feat: add demo skill")
	revision := runActivationGit(t, repository, "rev-parse", "HEAD")
	skill.RepositoryPath = repository
	skill.InstructionPath = "SKILL.md"

	root := filepath.Join(dataDir, "releases", skill.ID)
	release := filepath.Join(root, revision)
	if err := os.MkdirAll(release, 0o700); err != nil {
		t.Fatalf("create release: %v", err)
	}
	if err := os.WriteFile(filepath.Join(release, "SKILL.md"), contents, 0o444); err != nil {
		t.Fatalf("write release: %v", err)
	}
	if err := os.Chmod(release, 0o555); err != nil {
		t.Fatalf("make release immutable: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(release, 0o700)
	})
	if err := os.Symlink(revision, filepath.Join(root, "current")); err != nil {
		t.Fatalf("create current release link: %v", err)
	}
	current, err := filepath.Abs(filepath.Join(root, "current"))
	if err != nil {
		t.Fatalf("resolve current release path: %v", err)
	}
	return current
}

func runActivationGit(t *testing.T, repository string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
