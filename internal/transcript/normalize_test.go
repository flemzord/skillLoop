package transcript

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/flemzord/skillloop/internal/domain"
)

func TestNormalizeCodexTranscript(t *testing.T) {
	path := writeTranscript(t, `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Use the go-service skill"}]}}
{"type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"call-1","arguments":"{\"cmd\":\"go test ./...\"}"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"Process exited with code 1\nFinal output:\npackage failed"}}
{"type":"event_msg","payload":{"type":"agent_message","message":"I will fix it."}}
`)
	session, err := (Normalizer{}).Normalize(context.Background(), domain.HookEvent{
		ID: "event-1", Source: domain.SourceCodex, SessionID: "session-1", TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(session.Messages) != 4 {
		t.Fatalf("expected 4 messages, got %#v", session.Messages)
	}
	if session.Reference != "codex:session-1" {
		t.Fatalf("unexpected stable session reference: %s", session.Reference)
	}
	if session.Messages[1].ToolResult || session.Messages[1].ToolCallID != "call-1" {
		t.Fatalf("expected an explicit tool call, got %#v", session.Messages[1])
	}
	if !session.Messages[2].ToolResult || !session.Messages[2].Failed || session.Messages[2].ToolName != "exec_command" || session.Messages[2].ToolCallID != "call-1" {
		t.Fatalf("expected correlated failed tool result, got %#v", session.Messages[2])
	}
	if session.Outcome != domain.SessionOutcomeFailed {
		t.Fatalf("outcome = %q, want failed", session.Outcome)
	}
}

func TestNormalizeClaudeTranscript(t *testing.T) {
	path := writeTranscript(t, `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Use skillLoop"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"go test ./..."}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"ok","is_error":false}]}}
{incomplete
`)
	session, err := (Normalizer{}).Normalize(context.Background(), domain.HookEvent{
		ID: "event-2", Source: domain.SourceClaude, SessionID: "session-2", TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(session.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %#v", session.Messages)
	}
	if session.Messages[1].ToolResult || session.Messages[1].ToolCallID != "tool-1" {
		t.Fatalf("expected an explicit tool call, got %#v", session.Messages[1])
	}
	if !session.Messages[2].ToolResult || session.Messages[2].Failed || session.Messages[2].ToolName != "Bash" || session.Messages[2].ToolCallID != "tool-1" {
		t.Fatalf("expected successful correlated tool result, got %#v", session.Messages[2])
	}
	if session.Outcome != domain.SessionOutcomeSucceeded {
		t.Fatalf("outcome = %q, want succeeded", session.Outcome)
	}
}

func TestSessionOutcomeUsesLastCorrelatedToolResult(t *testing.T) {
	path := writeTranscript(t, `{"type":"response_item","payload":{"type":"function_call_output","call_id":"missing","output":"ok"}}
{"type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"call-1","arguments":"{}"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"Process exited with code 1"}}
{"type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"call-2","arguments":"{}"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"call-2","output":"ok"}}
`)
	session, err := (Normalizer{}).Normalize(context.Background(), domain.HookEvent{
		Source: domain.SourceCodex, SessionID: "latest-result", TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if session.Outcome != domain.SessionOutcomeSucceeded {
		t.Fatalf("outcome = %q, want succeeded from the last correlated result", session.Outcome)
	}
}

func TestSessionOutcomeIsUnknownWithoutCorrelatedToolResults(t *testing.T) {
	path := writeTranscript(t, `{"type":"response_item","payload":{"type":"function_call_output","call_id":"missing","output":"ok"}}
{"type":"event_msg","payload":{"type":"agent_message","message":"done"}}
`)
	session, err := (Normalizer{}).Normalize(context.Background(), domain.HookEvent{
		Source: domain.SourceCodex, SessionID: "no-results", TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if session.Outcome != domain.SessionOutcomeUnknown {
		t.Fatalf("outcome = %q, want unknown", session.Outcome)
	}
}

func TestNormalizeRejectsMissingTranscript(t *testing.T) {
	_, err := (Normalizer{}).Normalize(context.Background(), domain.HookEvent{Source: domain.SourceCodex})
	if err == nil {
		t.Fatal("expected missing transcript error")
	}
}

func writeTranscript(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}
