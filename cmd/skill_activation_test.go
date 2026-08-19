package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flemzord/skillloop/internal/config"
	"github.com/flemzord/skillloop/internal/domain"
	"github.com/flemzord/skillloop/internal/store"
)

const activationTestRevision = "0123456789abcdef0123456789abcdef01234567"

func TestSkillInstallAndUninstallCommandsUseConfiguredReleaseAndUserHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath, dataDir, skill := prepareActivationCommand(t)
	current := makeActivationRelease(t, dataDir, skill.ID)

	installOutput := bytes.NewBuffer(nil)
	install := NewRootCommand()
	install.SetOut(installOutput)
	install.SetErr(bytes.NewBuffer(nil))
	install.SetArgs([]string{"--config", configPath, "skill", "install", skill.ID, "--codex-only"})
	if err := install.Execute(); err != nil {
		t.Fatalf("install skill: %v", err)
	}
	codexDestination := filepath.Join(home, ".codex", "skills", "demo-skill")
	target, err := os.Readlink(codexDestination)
	if err != nil {
		t.Fatalf("read Codex skill link: %v", err)
	}
	if target != current {
		t.Fatalf("Codex link targets %q, want %q", target, current)
	}
	if !strings.Contains(installOutput.String(), "codex\tinstalled\t"+codexDestination) {
		t.Fatalf("unexpected install output: %q", installOutput.String())
	}
	if _, err := os.Lstat(filepath.Join(home, ".claude", "skills", "demo-skill")); !os.IsNotExist(err) {
		t.Fatalf("Claude destination unexpectedly exists: %v", err)
	}

	uninstallOutput := bytes.NewBuffer(nil)
	uninstall := NewRootCommand()
	uninstall.SetOut(uninstallOutput)
	uninstall.SetErr(bytes.NewBuffer(nil))
	uninstall.SetArgs([]string{"--config", configPath, "skill", "uninstall", skill.ID, "--codex-only"})
	if err := uninstall.Execute(); err != nil {
		t.Fatalf("uninstall skill: %v", err)
	}
	if _, err := os.Lstat(codexDestination); !os.IsNotExist(err) {
		t.Fatalf("Codex destination remains after uninstall: %v", err)
	}
	if !strings.Contains(uninstallOutput.String(), "codex\tuninstalled\t"+codexDestination) {
		t.Fatalf("unexpected uninstall output: %q", uninstallOutput.String())
	}
}

func TestSkillInstallRejectsMutuallyExclusivePlatformFlags(t *testing.T) {
	command := NewRootCommand()
	command.SetOut(bytes.NewBuffer(nil))
	command.SetErr(bytes.NewBuffer(nil))
	command.SetArgs([]string{"skill", "install", "skill-demo", "--codex-only", "--claude-only"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually exclusive flags error, got %v", err)
	}
}

func prepareActivationCommand(t *testing.T) (string, string, domain.Skill) {
	t.Helper()
	dataDir := t.TempDir()
	settings, err := config.Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	settings.DataDir = dataDir
	configPath := filepath.Join(t.TempDir(), "custom config", "skillloop.yaml")
	if _, err := config.WriteInitial(configPath, settings); err != nil {
		t.Fatalf("write config: %v", err)
	}

	database, err := store.Open(context.Background(), filepath.Join(dataDir, "skillloop.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	skill := domain.Skill{
		ID:              "skill-demo",
		Name:            "Demo Skill",
		RepositoryPath:  t.TempDir(),
		InstructionPath: "SKILL.md",
		Enabled:         true,
		CreatedAt:       time.Now().UTC(),
	}
	created, err := database.RegisterSkill(context.Background(), skill)
	if err != nil {
		_ = database.Close()
		t.Fatalf("register skill: %v", err)
	}
	if !created {
		_ = database.Close()
		t.Fatal("skill was not registered")
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	return configPath, dataDir, skill
}

func makeActivationRelease(t *testing.T, dataDir, skillID string) string {
	t.Helper()
	root := filepath.Join(dataDir, "releases", skillID)
	release := filepath.Join(root, activationTestRevision)
	if err := os.MkdirAll(release, 0o700); err != nil {
		t.Fatalf("create release: %v", err)
	}
	if err := os.WriteFile(filepath.Join(release, "SKILL.md"), []byte("# Demo\n"), 0o444); err != nil {
		t.Fatalf("write release: %v", err)
	}
	if err := os.Chmod(release, 0o555); err != nil {
		t.Fatalf("make release immutable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(release, 0o700) })
	if err := os.Symlink(activationTestRevision, filepath.Join(root, "current")); err != nil {
		t.Fatalf("create current release link: %v", err)
	}
	current, err := filepath.Abs(filepath.Join(root, "current"))
	if err != nil {
		t.Fatalf("resolve current release path: %v", err)
	}
	return current
}
