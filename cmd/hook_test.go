package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flemzord/skillloop/internal/capture"
	"github.com/flemzord/skillloop/internal/config"
	"github.com/flemzord/skillloop/internal/domain"
)

func TestHookCommandCapturesEventWithoutOutput(t *testing.T) {
	dataRoot := t.TempDir()
	configRoot := t.TempDir()
	configPath := filepath.Join(configRoot, "config.yaml")
	settings, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	settings.DataDir = filepath.Join(dataRoot, "skillloop")
	if _, err := config.WriteInitial(configPath, settings); err != nil {
		t.Fatal(err)
	}
	payload := `{
        "session_id":"session-1",
        "prompt_id":"prompt-1",
        "transcript_path":"/tmp/transcript.jsonl",
        "cwd":"/workspace",
        "permission_mode":"default",
        "hook_event_name":"Stop",
        "stop_hook_active":false,
        "last_assistant_message":"must not persist",
        "background_tasks":[{"command":"secret command"}]
    }`
	command := newHookCommand(&rootOptions{configPath: configPath})
	stdout := bytes.NewBuffer(nil)
	stderr := bytes.NewBuffer(nil)
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.SetIn(strings.NewReader(payload))
	command.SetArgs([]string{"--provider", "claude", "--event", "stop"})
	if err := command.Execute(); err != nil {
		t.Fatalf("execute hook: %v", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("hook emitted output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	incoming := filepath.Join(dataRoot, "skillloop", "spool", "incoming")
	entries, err := os.ReadDir(incoming)
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one event, got %d", len(entries))
	}
	contents, err := os.ReadFile(filepath.Join(incoming, entries[0].Name()))
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	event := domain.HookEvent{}
	if err := json.Unmarshal(contents, &event); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if event.Source != domain.SourceClaude || event.PromptID != "prompt-1" || event.HookEventName != "stop" {
		t.Fatalf("unexpected event: %#v", event)
	}
	if strings.Contains(string(contents), "must not persist") || strings.Contains(string(contents), "secret command") {
		t.Fatalf("raw hook data persisted: %s", contents)
	}
}

func TestHookCommandAlwaysFailsOpenAndStaysSilent(t *testing.T) {
	overLimit := bytes.Repeat([]byte("x"), int(capture.MaxHookInputBytes)+1)
	tests := []struct {
		name    string
		args    []string
		payload []byte
	}{
		{name: "invalid JSON", args: []string{"hook", "--provider", "codex", "--event", "stop"}, payload: []byte("{")},
		{name: "oversized", args: []string{"hook", "--provider", "codex", "--event", "stop"}, payload: overLimit},
		{name: "unknown provider", args: []string{"hook", "--provider", "other", "--event", "stop"}, payload: []byte(`{}`)},
		{name: "unknown event", args: []string{"hook", "--provider", "codex", "--event", "other"}, payload: []byte(`{}`)},
		{name: "event mismatch", args: []string{"hook", "--provider", "codex", "--event", "stop"}, payload: []byte(`{"session_id":"s","cwd":"/w","hook_event_name":"SessionEnd"}`)},
		{name: "unknown flag", args: []string{"hook", "--bogus"}, payload: []byte(`{}`)},
		{name: "missing flags", args: []string{"hook"}, payload: []byte(`{}`)},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			command := NewRootCommand()
			stdout := bytes.NewBuffer(nil)
			stderr := bytes.NewBuffer(nil)
			command.SetOut(stdout)
			command.SetErr(stderr)
			command.SetIn(bytes.NewReader(testCase.payload))
			command.SetArgs(testCase.args)
			if err := command.Execute(); err != nil {
				t.Fatalf("fail-open hook returned error: %v", err)
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("hook emitted output: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestHookCommandFailsOpenWhenSpoolIsUnavailable(t *testing.T) {
	dataRoot := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(dataRoot, []byte("file"), 0o600); err != nil {
		t.Fatalf("create blocking file: %v", err)
	}
	t.Setenv("XDG_DATA_HOME", dataRoot)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	payload := `{"session_id":"session","cwd":"/workspace","hook_event_name":"SessionEnd","reason":"other"}`
	command := NewRootCommand()
	stdout := bytes.NewBuffer(nil)
	stderr := bytes.NewBuffer(nil)
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.SetIn(strings.NewReader(payload))
	command.SetArgs([]string{"hook", "--provider", "codex", "--event", "session-end"})
	if err := command.Execute(); err != nil {
		t.Fatalf("fail-open hook returned error: %v", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("hook emitted output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
