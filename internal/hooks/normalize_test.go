package hooks

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/flemzord/skillloop/internal/domain"
)

func TestNormalizeCodexStopStripsContent(t *testing.T) {
	payload := []byte(`{
        "session_id":"session-1",
        "turn_id":"turn-1",
        "transcript_path":"/tmp/codex.jsonl",
        "cwd":"/workspace",
        "permission_mode":"default",
        "hook_event_name":"Stop",
        "model":"gpt-5.6-sol",
        "stop_hook_active":false,
        "last_assistant_message":"TOP SECRET",
        "future_content":"DO NOT COPY"
    }`)
	event, err := Normalize(domain.SourceCodex, EventStop, payload)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if event.Source != domain.SourceCodex || event.TurnID != "turn-1" || event.HookEventName != "stop" {
		t.Fatalf("unexpected normalized event: %#v", event)
	}
	contents, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal normalized event: %v", err)
	}
	if strings.Contains(string(contents), "TOP SECRET") || strings.Contains(string(contents), "DO NOT COPY") {
		t.Fatalf("raw content leaked into event: %s", contents)
	}
}

func TestNormalizeClaudeStopKeepsOnlyTaskCounts(t *testing.T) {
	payload := []byte(`{
        "session_id":"session-2",
        "prompt_id":"prompt-2",
        "transcript_path":"/tmp/claude.jsonl",
        "cwd":"/workspace",
        "permission_mode":"plan",
        "effort":{"level":"high"},
        "hook_event_name":"Stop",
        "stop_hook_active":true,
        "last_assistant_message":"PRIVATE RESPONSE",
        "background_tasks":[{"command":"PRIVATE COMMAND"},{"description":"PRIVATE DESCRIPTION"}],
        "session_crons":[{"prompt":"PRIVATE PROMPT"}]
    }`)
	event, err := Normalize(domain.SourceClaude, EventStop, payload)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if event.Source != domain.SourceClaude || event.PromptID != "prompt-2" || event.Effort != "high" {
		t.Fatalf("unexpected normalized event: %#v", event)
	}
	if event.BackgroundTaskCount == nil || *event.BackgroundTaskCount != 2 {
		t.Fatalf("unexpected background task count: %#v", event.BackgroundTaskCount)
	}
	if event.SessionCronCount == nil || *event.SessionCronCount != 1 {
		t.Fatalf("unexpected cron count: %#v", event.SessionCronCount)
	}
	contents, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal normalized event: %v", err)
	}
	for _, secret := range []string{"PRIVATE RESPONSE", "PRIVATE COMMAND", "PRIVATE DESCRIPTION", "PRIVATE PROMPT"} {
		if strings.Contains(string(contents), secret) {
			t.Fatalf("sensitive value %q leaked into event: %s", secret, contents)
		}
	}
}

func TestNormalizeSessionEndReasons(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		source domain.Source
		reason string
	}{
		{name: "codex", source: domain.SourceCodex, reason: "other"},
		{name: "claude clear", source: domain.SourceClaude, reason: "clear"},
		{name: "claude future reason", source: domain.SourceClaude, reason: "future_reason"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			payload := []byte(`{"session_id":"session","transcript_path":null,"cwd":"/workspace","hook_event_name":"SessionEnd","reason":"` + testCase.reason + `"}`)
			event, err := Normalize(testCase.source, EventSessionEnd, payload)
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if event.Reason != testCase.reason || event.HookEventName != "session-end" || event.TranscriptPath != "" {
				t.Fatalf("unexpected event: %#v", event)
			}
		})
	}
}

func TestNormalizeRejectsInvalidEnvelope(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		source  domain.Source
		event   Event
		payload string
	}{
		{name: "invalid JSON", source: domain.SourceCodex, event: EventStop, payload: "{"},
		{name: "missing session", source: domain.SourceCodex, event: EventStop, payload: `{"cwd":"/workspace","hook_event_name":"Stop"}`},
		{name: "missing cwd", source: domain.SourceCodex, event: EventStop, payload: `{"session_id":"session","hook_event_name":"Stop"}`},
		{name: "event mismatch", source: domain.SourceClaude, event: EventStop, payload: `{"session_id":"session","cwd":"/workspace","hook_event_name":"SessionEnd"}`},
		{name: "invalid provider", source: domain.Source("other"), event: EventStop, payload: `{"session_id":"session","cwd":"/workspace","hook_event_name":"Stop"}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := Normalize(testCase.source, testCase.event, []byte(testCase.payload)); err == nil {
				t.Fatal("expected normalization error")
			}
		})
	}
}
