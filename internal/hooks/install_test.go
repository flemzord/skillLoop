package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flemzord/skillloop/internal/domain"
)

func TestInstallerCodexPreservesExistingHooksAndIsIdempotent(t *testing.T) {
	home := t.TempDir()
	executable := filepath.Join(home, "bin with ' quote", "skillloop")
	installer := Installer{HomeDir: home, Executable: executable}
	path, err := installer.Path(domain.SourceCodex)
	if err != nil {
		t.Fatalf("config path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	existing := `{
        "description":"keep this description",
        "custom":{"enabled":true},
        "hooks":{
            "Stop":[{"matcher":"ignored","hooks":[{"type":"command","command":"other-tool","timeout":7}]}],
            "PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"policy"}]}]
        }
    }`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatalf("write existing config: %v", err)
	}

	changed, err := installer.Install(domain.SourceCodex)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !changed {
		t.Fatal("expected first install to change config")
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed config: %v", err)
	}
	assertJSONPathValue(t, first, "description", "keep this description")
	if !bytes.Contains(first, []byte(`"custom"`)) || !bytes.Contains(first, []byte(`"PreToolUse"`)) || !bytes.Contains(first, []byte(`"other-tool"`)) {
		t.Fatalf("existing configuration was lost: %s", first)
	}
	if count := bytes.Count(first, []byte(`--provider codex --event stop`)); count != 1 {
		t.Fatalf("expected one Codex Stop handler, got %d: %s", count, first)
	}
	if count := bytes.Count(first, []byte(`--provider codex --event session-end`)); count != 1 {
		t.Fatalf("expected one Codex SessionEnd handler, got %d: %s", count, first)
	}
	expectedEscapedPath := strings.ReplaceAll(shellQuote(executable), `"`, `\"`)
	if !strings.Contains(string(first), expectedEscapedPath) {
		t.Fatalf("executable path was not safely shell quoted: %s", first)
	}
	assertFileMode(t, path, 0o600)

	changed, err = installer.Install(domain.SourceCodex)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if changed {
		t.Fatal("second install should be idempotent")
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config after second install: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("idempotent install rewrote the config")
	}

	changed, err = installer.Uninstall(domain.SourceCodex)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !changed {
		t.Fatal("expected uninstall to change config")
	}
	uninstalled, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read uninstalled config: %v", err)
	}
	if bytes.Contains(uninstalled, []byte(`--provider codex`)) {
		t.Fatalf("SkillLoop hook remains after uninstall: %s", uninstalled)
	}
	if !bytes.Contains(uninstalled, []byte(`"other-tool"`)) || !bytes.Contains(uninstalled, []byte(`"PreToolUse"`)) {
		t.Fatalf("uninstall removed unrelated hooks: %s", uninstalled)
	}
}

func TestInstallerClaudeUsesExecFormAndPreservesSettings(t *testing.T) {
	home := t.TempDir()
	executable := filepath.Join(home, "SkillLoop App", "skillloop")
	installer := Installer{HomeDir: home, Executable: executable}
	path, err := installer.Path(domain.SourceClaude)
	if err != nil {
		t.Fatalf("config path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	existing := `{
        "model":"claude-opus-4-8",
        "permissions":{"allow":["Read"]},
        "hooks":{"SessionEnd":[{"hooks":[{"type":"command","command":"cleanup","args":[]}]}]}
    }`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatalf("write existing config: %v", err)
	}

	changed, err := installer.Install(domain.SourceClaude)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !changed {
		t.Fatal("expected install to change config")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !bytes.Contains(contents, []byte(`"claude-opus-4-8"`)) || !bytes.Contains(contents, []byte(`"permissions"`)) || !bytes.Contains(contents, []byte(`"cleanup"`)) {
		t.Fatalf("existing settings were lost: %s", contents)
	}
	document := decodeObject(t, contents)
	hookEvents := document["hooks"].(map[string]any)
	for _, eventName := range []string{"Stop", "SessionEnd"} {
		groups := hookEvents[eventName].([]any)
		found := false
		for _, groupValue := range groups {
			group := groupValue.(map[string]any)
			for _, handlerValue := range group["hooks"].([]any) {
				handler := handlerValue.(map[string]any)
				if handler["command"] != executable {
					continue
				}
				args := handler["args"].([]any)
				if len(args) != 5 || args[0] != "hook" || args[1] != "--provider" || args[2] != "claude" || args[3] != "--event" {
					t.Fatalf("unexpected Claude exec args: %#v", args)
				}
				found = true
			}
		}
		if !found {
			t.Fatalf("Claude handler not found for %s: %s", eventName, contents)
		}
	}

	changed, err = installer.Uninstall(domain.SourceClaude)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !changed {
		t.Fatal("expected uninstall to change config")
	}
	contents, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read uninstalled config: %v", err)
	}
	if bytes.Contains(contents, []byte(executable)) || !bytes.Contains(contents, []byte(`"cleanup"`)) {
		t.Fatalf("unexpected uninstall result: %s", contents)
	}
}

func TestInstallerRejectsMalformedExistingConfig(t *testing.T) {
	home := t.TempDir()
	installer := Installer{HomeDir: home, Executable: "/usr/local/bin/skillloop"}
	path, err := installer.Path(domain.SourceClaude)
	if err != nil {
		t.Fatalf("config path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"hooks":`), 0o600); err != nil {
		t.Fatalf("write malformed config: %v", err)
	}
	if _, err := installer.Install(domain.SourceClaude); err == nil {
		t.Fatal("expected malformed config error")
	}
}

func TestUninstallWithoutConfigIsANoop(t *testing.T) {
	home := t.TempDir()
	installer := Installer{HomeDir: home, Executable: "/usr/local/bin/skillloop"}
	changed, err := installer.Uninstall(domain.SourceCodex)
	if err != nil {
		t.Fatalf("uninstall absent config: %v", err)
	}
	if changed {
		t.Fatal("absent config should not be changed")
	}
	path, err := installer.Path(domain.SourceCodex)
	if err != nil {
		t.Fatalf("config path: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("uninstall created config: %v", err)
	}
}

func decodeObject(t *testing.T, contents []byte) map[string]any {
	t.Helper()
	value := map[string]any{}
	if err := json.Unmarshal(contents, &value); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, contents)
	}
	return value
}

func assertJSONPathValue(t *testing.T, contents []byte, key string, expected any) {
	t.Helper()
	document := decodeObject(t, contents)
	if actual := document[key]; actual != expected {
		t.Fatalf("unexpected %s: got %#v, want %#v", key, actual, expected)
	}
}

func assertFileMode(t *testing.T, path string, expected os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if actual := info.Mode().Perm(); actual != expected {
		t.Fatalf("unexpected mode for %s: got %o, want %o", path, actual, expected)
	}
}
