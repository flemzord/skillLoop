package transcript

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/flemzord/skillloop/internal/domain"
)

func TestNormalizeCodexTranscript(t *testing.T) {
	workingDir := t.TempDir()
	path := writeNativeTranscript(t, domain.SourceCodex, "session-1", workingDir, `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Use the go-service skill"}]}}
{"type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"call-1","arguments":"{\"cmd\":\"go test ./...\"}"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"Process exited with code 1\nFinal output:\npackage failed"}}
{"type":"event_msg","payload":{"type":"agent_message","message":"I will fix it."}}
`)
	session, err := normalizerFor(path, domain.SourceCodex).Normalize(context.Background(), domain.HookEvent{
		ID: "event-1", Source: domain.SourceCodex, SessionID: "session-1", WorkingDir: workingDir, TranscriptPath: path,
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

func TestNormalizeCodexUnifiedExecCommand(t *testing.T) {
	workingDir := t.TempDir()
	wrapper := `const r = await tools.exec_command({"cmd":"go test ./...","workdir":"/workspace"}); text(r.output);`
	contents := fmt.Sprintf(
		"{\"type\":\"response_item\",\"payload\":{\"type\":\"custom_tool_call\",\"name\":\"exec\",\"call_id\":\"call-1\",\"input\":%q}}\n"+
			"{\"type\":\"response_item\",\"payload\":{\"type\":\"custom_tool_call_output\",\"call_id\":\"call-1\",\"output\":\"Script completed\\nFinal output:\\nok\"}}\n",
		wrapper,
	)
	path := writeNativeTranscript(t, domain.SourceCodex, "unified-exec", workingDir, contents)
	session, err := normalizerFor(path, domain.SourceCodex).Normalize(context.Background(), domain.HookEvent{
		Source: domain.SourceCodex, SessionID: "unified-exec", WorkingDir: workingDir, TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(session.Messages) != 2 {
		t.Fatalf("messages = %#v, want one call and one result", session.Messages)
	}
	for _, message := range session.Messages {
		if message.ToolName != "exec_command" {
			t.Fatalf("tool name = %q, want exec_command in %#v", message.ToolName, session.Messages)
		}
	}
	if session.Messages[0].Text != `{"cmd":"go test ./...","workdir":"/workspace"}` {
		t.Fatalf("normalized command = %q", session.Messages[0].Text)
	}
}

func TestNormalizeCodexUnifiedExecBlockOutput(t *testing.T) {
	workingDir := t.TempDir()
	wrapper := `const r = await tools.exec_command({"cmd":"cat /workspace/SKILL.md"}); text(r.output);`
	contents := fmt.Sprintf(
		"{\"type\":\"response_item\",\"payload\":{\"type\":\"custom_tool_call\",\"name\":\"exec\",\"call_id\":\"call-1\",\"input\":%q}}\n"+
			"{\"type\":\"response_item\",\"payload\":{\"type\":\"custom_tool_call_output\",\"call_id\":\"call-1\",\"output\":[{\"type\":\"input_text\",\"text\":\"Script completed\\nOutput:\\n\"},{\"type\":\"input_text\",\"text\":\"# Skill contents\"}]}}\n",
		wrapper,
	)
	path := writeNativeTranscript(t, domain.SourceCodex, "unified-exec-block-output", workingDir, contents)
	session, err := normalizerFor(path, domain.SourceCodex).Normalize(context.Background(), domain.HookEvent{
		Source: domain.SourceCodex, SessionID: "unified-exec-block-output", WorkingDir: workingDir, TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(session.Messages) != 2 {
		t.Fatalf("messages = %#v, want one call and one result", session.Messages)
	}
	result := session.Messages[1]
	if result.ToolName != "exec_command" || !result.ToolResult || result.Failed {
		t.Fatalf("result = %#v", result)
	}
	if result.Text != "Script completed\nOutput:\n\n# Skill contents" {
		t.Fatalf("block output = %q", result.Text)
	}
}

func TestNormalizeCodexUnifiedExecCommandShapes(t *testing.T) {
	tests := []struct {
		name        string
		toolName    string
		input       string
		wantName    string
		wantCommand string
	}{
		{
			name: "bare JavaScript cmd key", toolName: "exec",
			input:       `const r = await tools.exec_command({cmd:"go test ./...",workdir:"/workspace"}); text(r.output);`,
			wantName:    "exec_command",
			wantCommand: `{"cmd":"go test ./..."}`,
		},
		{
			name: "legacy direct call", toolName: "exec_command", input: `{"cmd":"go test ./..."}`,
			wantName: "exec_command", wantCommand: `{"cmd":"go test ./..."}`,
		},
		{
			name: "multiple nested commands remain ambiguous", toolName: "exec",
			input:    `const a = tools.exec_command({"cmd":"first"}); const b = tools.exec_command({"cmd":"second"});`,
			wantName: "exec",
		},
		{
			name: "mixed nested tools remain ambiguous", toolName: "exec",
			input:    `const a = tools.exec_command({"cmd":"first"}); const b = tools.apply_patch("patch");`,
			wantName: "exec",
		},
		{
			name: "invalid payload remains exec", toolName: "exec",
			input:    `const r = tools.exec_command({command:"go test ./..."});`,
			wantName: "exec",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			name, command := normalizeCodexToolCall(test.toolName, test.input)
			if name != test.wantName {
				t.Fatalf("name = %q, want %q", name, test.wantName)
			}
			if test.wantCommand != "" && command != test.wantCommand {
				t.Fatalf("command = %q, want %q", command, test.wantCommand)
			}
		})
	}
}

func TestNormalizeClaudeTranscript(t *testing.T) {
	workingDir := t.TempDir()
	path := writeNativeTranscript(t, domain.SourceClaude, "session-2", workingDir, `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Use skillLoop"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"go test ./..."}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"ok","is_error":false}]}}
{incomplete
`)
	session, err := normalizerFor(path, domain.SourceClaude).Normalize(context.Background(), domain.HookEvent{
		ID: "event-2", Source: domain.SourceClaude, SessionID: "session-2", WorkingDir: workingDir, TranscriptPath: path,
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
	workingDir := t.TempDir()
	path := writeNativeTranscript(t, domain.SourceCodex, "latest-result", workingDir, `{"type":"response_item","payload":{"type":"function_call_output","call_id":"missing","output":"ok"}}
{"type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"call-1","arguments":"{}"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"Process exited with code 1"}}
{"type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"call-2","arguments":"{}"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"call-2","output":"ok"}}
`)
	session, err := normalizerFor(path, domain.SourceCodex).Normalize(context.Background(), domain.HookEvent{
		Source: domain.SourceCodex, SessionID: "latest-result", WorkingDir: workingDir, TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if session.Outcome != domain.SessionOutcomeSucceeded {
		t.Fatalf("outcome = %q, want succeeded from the last correlated result", session.Outcome)
	}
}

func TestSessionOutcomeIsUnknownWithoutCorrelatedToolResults(t *testing.T) {
	workingDir := t.TempDir()
	path := writeNativeTranscript(t, domain.SourceCodex, "no-results", workingDir, `{"type":"response_item","payload":{"type":"function_call_output","call_id":"missing","output":"ok"}}
{"type":"event_msg","payload":{"type":"agent_message","message":"done"}}
`)
	session, err := normalizerFor(path, domain.SourceCodex).Normalize(context.Background(), domain.HookEvent{
		Source: domain.SourceCodex, SessionID: "no-results", WorkingDir: workingDir, TranscriptPath: path,
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

func TestNormalizeAcceptsStandardProviderTranscriptRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	tests := []struct {
		name   string
		source domain.Source
		path   string
		line   string
	}{
		{
			name:   "Codex sessions",
			source: domain.SourceCodex,
			path:   filepath.Join(home, ".codex", "sessions", "2026", "08", "session.jsonl"),
			line:   `{"type":"event_msg","payload":{"type":"agent_message","message":"codex control"}}` + "\n",
		},
		{
			name:   "Codex archived sessions",
			source: domain.SourceCodex,
			path:   filepath.Join(home, ".codex", "archived_sessions", "session.jsonl"),
			line:   `{"type":"event_msg","payload":{"type":"agent_message","message":"codex archive control"}}` + "\n",
		},
		{
			name:   "Claude projects",
			source: domain.SourceClaude,
			path:   filepath.Join(home, ".claude", "projects", "project", "session.jsonl"),
			line:   `{"type":"assistant","message":{"role":"assistant","content":"claude control"}}` + "\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workingDir := t.TempDir()
			if err := os.MkdirAll(filepath.Dir(test.path), 0o700); err != nil {
				t.Fatalf("create provider transcript root: %v", err)
			}
			contents := nativeTranscriptContents(test.source, "valid-control", workingDir, test.line)
			if err := os.WriteFile(test.path, []byte(contents), 0o600); err != nil {
				t.Fatalf("write provider transcript: %v", err)
			}
			session, err := (Normalizer{}).Normalize(context.Background(), domain.HookEvent{
				Source: test.source, SessionID: "valid-control", WorkingDir: workingDir, TranscriptPath: test.path,
			})
			if err != nil {
				t.Fatalf("normalize standard provider transcript: %v", err)
			}
			if len(session.Messages) != 1 {
				t.Fatalf("messages = %#v, want one valid control message", session.Messages)
			}
		})
	}
}

func TestNormalizeHonorsProviderHomeOverrides(t *testing.T) {
	root := t.TempDir()
	codexHome := filepath.Join(root, "codex-home")
	claudeHome := filepath.Join(root, "claude-home")
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)

	for source, path := range map[domain.Source]string{
		domain.SourceCodex:  filepath.Join(codexHome, "sessions", "session.jsonl"),
		domain.SourceClaude: filepath.Join(claudeHome, "projects", "session.jsonl"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("create override root: %v", err)
		}
		workingDir := t.TempDir()
		if err := os.WriteFile(path, []byte(nativeTranscriptContents(source, "override", workingDir, "{}\n")), 0o600); err != nil {
			t.Fatalf("write override transcript: %v", err)
		}
		if _, err := (Normalizer{}).Normalize(context.Background(), domain.HookEvent{
			Source: source, SessionID: "override", WorkingDir: workingDir, TranscriptPath: path,
		}); err != nil {
			t.Fatalf("normalize %s override transcript: %v", source, err)
		}
	}
}

func TestNormalizeRejectsPathsOutsideProviderAuthority(t *testing.T) {
	root := filepath.Join(t.TempDir(), "allowed")
	outside := filepath.Join(t.TempDir(), "private.jsonl")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create allowed root: %v", err)
	}
	if err := os.WriteFile(outside, []byte(`{"type":"event_msg","payload":{"type":"agent_message","message":"private"}}`+"\n"), 0o600); err != nil {
		t.Fatalf("write outside transcript: %v", err)
	}
	normalizer := Normalizer{AllowedRoots: map[domain.Source][]string{domain.SourceCodex: {root}}}
	_, err := normalizer.Normalize(context.Background(), domain.HookEvent{
		Source: domain.SourceCodex, SessionID: "outside", TranscriptPath: outside,
	})
	if err == nil || !strings.Contains(err.Error(), "outside the allowed provider roots") {
		t.Fatalf("outside-root error = %v", err)
	}
}

func TestNormalizeRejectsTranscriptSymlinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.jsonl")
	link := filepath.Join(root, "linked.jsonl")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create transcript symlink: %v", err)
	}
	_, err := normalizerFor(link, domain.SourceCodex).Normalize(context.Background(), domain.HookEvent{
		Source: domain.SourceCodex, SessionID: "symlink", TranscriptPath: link,
	})
	if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestNormalizeRejectsIntermediateSymlinkEscapes(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "allowed")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create root: %v", err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("create outside: %v", err)
	}
	outsidePath := filepath.Join(outside, "private.jsonl")
	if err := os.WriteFile(outsidePath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write outside transcript: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatalf("create intermediate symlink: %v", err)
	}
	path := filepath.Join(root, "escape", "private.jsonl")
	normalizer := Normalizer{AllowedRoots: map[domain.Source][]string{domain.SourceCodex: {root}}}
	_, err := normalizer.Normalize(context.Background(), domain.HookEvent{
		Source: domain.SourceCodex, SessionID: "intermediate-symlink", TranscriptPath: path,
	})
	if err == nil || !strings.Contains(err.Error(), "outside the allowed provider roots") {
		t.Fatalf("intermediate symlink error = %v", err)
	}
}

func TestNormalizeRejectsSpecialFilesWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "transcript.jsonl")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("create FIFO: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := normalizerFor(fifo, domain.SourceCodex).Normalize(context.Background(), domain.HookEvent{
			Source: domain.SourceCodex, SessionID: "fifo", TranscriptPath: fifo,
		})
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("FIFO error = %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("FIFO transcript blocked normalization")
	}
}

func TestNormalizeRejectsAggregateLimitOverflows(t *testing.T) {
	message := `{"type":"event_msg","payload":{"type":"agent_message","message":"hello"}}` + "\n"
	tests := []struct {
		name     string
		source   domain.Source
		contents string
		limits   Limits
		want     string
	}{
		{
			name:     "bytes",
			source:   domain.SourceCodex,
			contents: message,
			limits:   Limits{MaximumBytes: int64(len(message) - 1), MaximumRecords: 10, MaximumMessages: 10},
			want:     "maximum size",
		},
		{
			name:     "records",
			source:   domain.SourceCodex,
			contents: message + message + message,
			limits:   Limits{MaximumBytes: 1 << 20, MaximumRecords: 2, MaximumMessages: 10},
			want:     "maximum record count",
		},
		{
			name:     "messages",
			source:   domain.SourceClaude,
			contents: `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"one"},{"type":"text","text":"two"},{"type":"text","text":"three"}]}}` + "\n",
			limits:   Limits{MaximumBytes: 1 << 20, MaximumRecords: 10, MaximumMessages: 2},
			want:     "maximum retained message count",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workingDir := t.TempDir()
			contents := nativeTranscriptContents(test.source, "over-limit", workingDir, test.contents)
			path := writeTranscript(t, contents)
			normalizer := normalizerFor(path, test.source)
			limits := test.limits
			if test.name == "bytes" {
				limits.MaximumBytes = int64(len(contents) - 1)
			}
			normalizer.Limits = limits
			_, err := normalizer.Normalize(context.Background(), domain.HookEvent{
				Source: test.source, SessionID: "over-limit", WorkingDir: workingDir, TranscriptPath: path,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("limit error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestNormalizeAcceptsAggregateValuesAtLimits(t *testing.T) {
	workingDir := t.TempDir()
	contents := nativeTranscriptContents(domain.SourceCodex, "at-limit", workingDir,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"at the boundary"}}`+"\n")
	path := writeTranscript(t, contents)
	normalizer := normalizerFor(path, domain.SourceCodex)
	normalizer.Limits = Limits{
		MaximumBytes: int64(len(contents)), MaximumRecords: 2, MaximumMessages: 1,
	}
	session, err := normalizer.Normalize(context.Background(), domain.HookEvent{
		Source: domain.SourceCodex, SessionID: "at-limit", WorkingDir: workingDir, TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("normalize transcript at exact limits: %v", err)
	}
	if len(session.Messages) != 1 || session.Messages[0].Text != "at the boundary" {
		t.Fatalf("messages at exact limits = %#v", session.Messages)
	}
}

func TestNormalizeRejectsProviderIdentityReplay(t *testing.T) {
	for _, test := range []struct {
		name      string
		source    domain.Source
		nativeID  string
		nativeCWD string
		hookID    string
		hookCWD   string
		want      string
	}{
		{name: "Codex session", source: domain.SourceCodex, nativeID: "native", nativeCWD: "/workspace", hookID: "replayed", hookCWD: "/workspace", want: "session identity"},
		{name: "Codex cwd", source: domain.SourceCodex, nativeID: "native", nativeCWD: "/workspace", hookID: "native", hookCWD: "/other", want: "does not match hook cwd"},
		{name: "Claude session", source: domain.SourceClaude, nativeID: "native", nativeCWD: "/workspace", hookID: "replayed", hookCWD: "/workspace", want: "session identity"},
		{name: "Claude cwd", source: domain.SourceClaude, nativeID: "native", nativeCWD: "/workspace", hookID: "native", hookCWD: "/other", want: "does not match hook cwd"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeNativeTranscript(t, test.source, test.nativeID, test.nativeCWD, "{}\n")
			_, err := normalizerFor(path, test.source).Normalize(context.Background(), domain.HookEvent{
				Source: test.source, SessionID: test.hookID, WorkingDir: test.hookCWD, TranscriptPath: path,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("identity error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestNormalizeRejectsMissingOrConflictingProviderIdentity(t *testing.T) {
	missing := writeTranscript(t, `{"type":"event_msg","payload":{"type":"agent_message","message":"no metadata"}}`+"\n")
	_, err := normalizerFor(missing, domain.SourceCodex).Normalize(context.Background(), domain.HookEvent{
		Source: domain.SourceCodex, SessionID: "session", WorkingDir: "/workspace", TranscriptPath: missing,
	})
	if err == nil || !strings.Contains(err.Error(), "missing provider-native") {
		t.Fatalf("missing identity error = %v", err)
	}

	conflictingContents := nativeTranscriptContents(domain.SourceClaude, "session", "/workspace", "") +
		nativeTranscriptContents(domain.SourceClaude, "other", "/workspace", "")
	conflicting := writeTranscript(t, conflictingContents)
	_, err = normalizerFor(conflicting, domain.SourceClaude).Normalize(context.Background(), domain.HookEvent{
		Source: domain.SourceClaude, SessionID: "session", WorkingDir: "/workspace", TranscriptPath: conflicting,
	})
	if err == nil || !strings.Contains(err.Error(), "conflicting provider-native") {
		t.Fatalf("conflicting identity error = %v", err)
	}
}

func TestNormalizeClaudeAcceptsPartialRecordsAfterAuthoritativeIdentity(t *testing.T) {
	contents := nativeTranscriptContents(domain.SourceClaude, "session", "/workspace", "") +
		`{"type":"user","sessionId":"session","message":{"role":"user","content":"partial id"}}` + "\n" +
		`{"type":"assistant","cwd":"/workspace","message":{"role":"assistant","content":"partial cwd"}}` + "\n"
	path := writeTranscript(t, contents)
	session, err := normalizerFor(path, domain.SourceClaude).Normalize(context.Background(), domain.HookEvent{
		Source: domain.SourceClaude, SessionID: "session", WorkingDir: "/workspace", TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("normalize Claude partial records: %v", err)
	}
	if session.ExternalID != "session" || session.WorkingDir != "/workspace" || len(session.Messages) != 2 {
		t.Fatalf("normalized Claude session=%#v", session)
	}
}

func TestNormalizeClaudeRejectsExplicitPartialIdentityConflicts(t *testing.T) {
	for _, test := range []struct {
		name   string
		record string
	}{
		{name: "session id", record: `{"type":"user","sessionId":"other"}`},
		{name: "cwd", record: `{"type":"assistant","cwd":"/other"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			contents := nativeTranscriptContents(domain.SourceClaude, "session", "/workspace", test.record+"\n")
			path := writeTranscript(t, contents)
			_, err := normalizerFor(path, domain.SourceClaude).Normalize(context.Background(), domain.HookEvent{
				Source: domain.SourceClaude, SessionID: "session", WorkingDir: "/workspace", TranscriptPath: path,
			})
			if err == nil || !strings.Contains(err.Error(), "conflicting provider-native") {
				t.Fatalf("partial conflict error=%v", err)
			}
		})
	}
}

func TestNormalizeClaudeDoesNotCombinePartialRecordsIntoIdentity(t *testing.T) {
	contents := `{"type":"user","sessionId":"session"}` + "\n" +
		`{"type":"assistant","cwd":"/workspace"}` + "\n"
	path := writeTranscript(t, contents)
	_, err := normalizerFor(path, domain.SourceClaude).Normalize(context.Background(), domain.HookEvent{
		Source: domain.SourceClaude, SessionID: "session", WorkingDir: "/workspace", TranscriptPath: path,
	})
	if err == nil || !strings.Contains(err.Error(), "missing provider-native") {
		t.Fatalf("partial-only identity error=%v", err)
	}
}

func normalizerFor(path string, source domain.Source) Normalizer {
	return Normalizer{AllowedRoots: map[domain.Source][]string{source: {filepath.Dir(path)}}}
}

func writeTranscript(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

func writeNativeTranscript(t *testing.T, source domain.Source, sessionID, workingDir, contents string) string {
	t.Helper()
	return writeTranscript(t, nativeTranscriptContents(source, sessionID, workingDir, contents))
}

func nativeTranscriptContents(source domain.Source, sessionID, workingDir, contents string) string {
	var metadata string
	switch source {
	case domain.SourceCodex:
		metadata = fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":%q}}`, sessionID, workingDir)
	case domain.SourceClaude:
		metadata = fmt.Sprintf(`{"type":"system","sessionId":%q,"cwd":%q}`, sessionID, workingDir)
	}
	return metadata + "\n" + contents
}
