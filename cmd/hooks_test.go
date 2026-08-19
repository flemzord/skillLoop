package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHooksInstallAndUninstallCommandsUseUserScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	install := NewRootCommand()
	install.SetOut(bytes.NewBuffer(nil))
	install.SetErr(bytes.NewBuffer(nil))
	install.SetArgs([]string{"hooks", "install"})
	if err := install.Execute(); err != nil {
		t.Fatalf("install hooks: %v", err)
	}
	for _, path := range []string{
		filepath.Join(home, ".codex", "hooks.json"),
		filepath.Join(home, ".claude", "settings.json"),
	} {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read installed config %s: %v", path, err)
		}
		if !bytes.Contains(contents, []byte(`"Stop"`)) || !bytes.Contains(contents, []byte(`"SessionEnd"`)) {
			t.Fatalf("missing hooks in %s: %s", path, contents)
		}
		if bytes.Contains(contents, []byte(`--config`)) {
			t.Fatalf("default config path should not be persisted in %s: %s", path, contents)
		}
	}

	uninstall := NewRootCommand()
	uninstall.SetOut(bytes.NewBuffer(nil))
	uninstall.SetErr(bytes.NewBuffer(nil))
	uninstall.SetArgs([]string{"hooks", "uninstall"})
	if err := uninstall.Execute(); err != nil {
		t.Fatalf("uninstall hooks: %v", err)
	}
	for _, path := range []string{
		filepath.Join(home, ".codex", "hooks.json"),
		filepath.Join(home, ".claude", "settings.json"),
	} {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read uninstalled config %s: %v", path, err)
		}
		if bytes.Contains(contents, []byte(`--provider`)) {
			t.Fatalf("SkillLoop hooks remain in %s: %s", path, contents)
		}
	}
}

func TestHooksInstallPersistsAbsoluteCustomConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(t.TempDir(), "config dir", "owner's config.yaml")
	absoluteConfig, err := filepath.Abs(configPath)
	if err != nil {
		t.Fatalf("absolute config path: %v", err)
	}

	install := func() {
		command := NewRootCommand()
		command.SetOut(bytes.NewBuffer(nil))
		command.SetErr(bytes.NewBuffer(nil))
		command.SetArgs([]string{"--config", configPath, "hooks", "install"})
		if err := command.Execute(); err != nil {
			t.Fatalf("install hooks: %v", err)
		}
	}
	install()
	codexPath := filepath.Join(home, ".codex", "hooks.json")
	claudePath := filepath.Join(home, ".claude", "settings.json")
	codexFirst, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatalf("read Codex hooks: %v", err)
	}
	claudeFirst, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatalf("read Claude hooks: %v", err)
	}
	expectedCodexPath := []byte(strings.ReplaceAll(shellQuotedForTest(absoluteConfig), `"`, `\"`))
	if !bytes.Contains(codexFirst, []byte(`--config`)) || !bytes.Contains(codexFirst, expectedCodexPath) {
		t.Fatalf("Codex hook missing quoted custom config: %s", codexFirst)
	}
	if !bytes.Contains(claudeFirst, []byte(`"--config"`)) || !bytes.Contains(claudeFirst, []byte(strings.ReplaceAll(absoluteConfig, `"`, `\"`))) {
		t.Fatalf("Claude hook missing custom config argv: %s", claudeFirst)
	}

	install()
	codexSecond, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatalf("read Codex hooks after reinstall: %v", err)
	}
	claudeSecond, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatalf("read Claude hooks after reinstall: %v", err)
	}
	if !bytes.Equal(codexFirst, codexSecond) || !bytes.Equal(claudeFirst, claudeSecond) {
		t.Fatal("custom-config hook installation is not idempotent")
	}
}

func shellQuotedForTest(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
