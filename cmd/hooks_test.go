package cmd

import (
	"bytes"
	"os"
	"path/filepath"
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
