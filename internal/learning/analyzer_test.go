package learning

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/flemzord/skillloop/internal/domain"
)

func TestAnalyzeAttributedCorrectionAndFailure(t *testing.T) {
	session := domain.Session{
		Source: domain.SourceCodex, ExternalID: "session-1",
		Messages: append(codexInstructionRead("/skills/go-service/SKILL.md"),
			domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "call-1", Text: `{"cmd":"go test ./..."}`},
			domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "call-1", ToolResult: true, Text: "exit code 1: package failed", Failed: true},
			domain.Message{Role: "user", Text: "Non, il faut utiliser nix develop."},
			domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "call-2", Text: `{"cmd":"nix develop --command go test ./..."}`},
			domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "call-2", ToolResult: true, Text: "ok"},
		),
	}
	skills := []domain.Skill{{ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service", InstructionPath: "SKILL.md", Enabled: true}}
	cards := NewAnalyzer().Analyze(session, skills)
	if len(cards) != 3 {
		t.Fatalf("expected failure, correction, and validation cards, got %#v", cards)
	}
	if cards[0].SessionRef != "codex:session-1" || cards[0].SkillID != "skill-1" {
		t.Fatalf("unexpected attribution: %#v", cards[0])
	}
	if cards[0].Kind != domain.CardFailure || !contains(cards[0].Lesson, "nix develop --command go test") {
		t.Fatalf("expected recovery command from the successful call/result pair, got %#v", cards[0])
	}
	if cards[2].Kind != domain.CardValidation || cards[2].Lesson != "go test" {
		t.Fatalf("expected validation from the successful result, got %#v", cards[2])
	}
}

func TestAnalyzeFailedValidationCallDoesNotCreateValidation(t *testing.T) {
	session := domain.Session{
		Source: domain.SourceClaude, ExternalID: "session-failed-validation",
		Messages: append(claudeInstructionRead("/skills/go-service/SKILL.md"),
			domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "tool-1", Text: `{"command":"go test ./..."}`},
			domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "tool-1", ToolResult: true, Text: "FAIL\nexit status 1", Failed: true},
		),
	}
	skills := []domain.Skill{{ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service", InstructionPath: "SKILL.md", Enabled: true}}
	cards := NewAnalyzer().Analyze(session, skills)
	if len(cards) != 1 || cards[0].Kind != domain.CardFailure {
		t.Fatalf("expected only a failure card after a failed validation result, got %#v", cards)
	}
}

func TestAnalyzeIgnoresFailedToolCallWithoutResult(t *testing.T) {
	session := domain.Session{
		Source: domain.SourceCodex, ExternalID: "session-call-only",
		Messages: append(codexInstructionRead("/skills/go-service/SKILL.md"),
			domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "call-1", Text: `{"cmd":"go test ./..."}`, Failed: true},
		),
	}
	skills := []domain.Skill{{ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service", InstructionPath: "SKILL.md", Enabled: true}}
	if cards := NewAnalyzer().Analyze(session, skills); len(cards) != 0 {
		t.Fatalf("expected tool calls without results to produce no cards, got %#v", cards)
	}
}

func TestAnalyzeDoesNotInferValidationFromResultText(t *testing.T) {
	session := domain.Session{
		Source: domain.SourceClaude, ExternalID: "session-result-text",
		Messages: append(claudeInstructionRead("/skills/go-service/SKILL.md"),
			domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "tool-1", Text: `{"command":"echo done"}`},
			domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "tool-1", ToolResult: true, Text: "go test passed"},
		),
	}
	skills := []domain.Skill{{ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service", InstructionPath: "SKILL.md", Enabled: true}}
	if cards := NewAnalyzer().Analyze(session, skills); len(cards) != 0 {
		t.Fatalf("expected validation detection to inspect the associated call, got %#v", cards)
	}
}

func TestAnalyzeValidationRequiresAnExactStructuredCommand(t *testing.T) {
	skill := domain.Skill{
		ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
		InstructionPath: "SKILL.md", Enabled: true,
	}
	tests := []struct {
		name     string
		source   domain.Source
		toolName string
		payload  string
	}{
		{name: "echo quoted validation", source: domain.SourceCodex, toolName: "exec_command", payload: `{"cmd":"echo 'go test ./...'"}`},
		{name: "shell comment", source: domain.SourceCodex, toolName: "exec_command", payload: `{"cmd":"go test ./... # claimed validation"}`},
		{name: "composed and", source: domain.SourceCodex, toolName: "exec_command", payload: `{"cmd":"go test ./... && true"}`},
		{name: "composed newline", source: domain.SourceCodex, toolName: "exec_command", payload: "{\"cmd\":\"go test ./...\\ntrue\"}"},
		{name: "nested composed command", source: domain.SourceCodex, toolName: "exec_command", payload: `{"cmd":"sh -c 'go test ./...; true'"}`},
		{name: "wrong executable", source: domain.SourceCodex, toolName: "exec_command", payload: `{"cmd":"go-test ./..."}`},
		{name: "substring executable", source: domain.SourceCodex, toolName: "exec_command", payload: `{"cmd":"go testhelper ./..."}`},
		{name: "golangci help is not validation", source: domain.SourceCodex, toolName: "exec_command", payload: `{"cmd":"golangci-lint --help"}`},
		{name: "nix wrapper runs echo", source: domain.SourceCodex, toolName: "exec_command", payload: `{"cmd":"nix develop --command echo go test"}`},
		{name: "raw unstructured command", source: domain.SourceCodex, toolName: "exec_command", payload: `go test ./...`},
		{name: "wrong codex field", source: domain.SourceCodex, toolName: "exec_command", payload: `{"command":"go test ./..."}`},
		{name: "sudo option consumes apparent command", source: domain.SourceCodex, toolName: "exec_command", payload: `{"cmd":"sudo -u go test"}`},
		{name: "env option consumes apparent command", source: domain.SourceCodex, toolName: "exec_command", payload: `{"cmd":"env -u go test"}`},
		{name: "forged codex shell name", source: domain.SourceCodex, toolName: "Bash", payload: `{"command":"go test ./..."}`},
		{name: "forged claude shell name", source: domain.SourceClaude, toolName: "exec_command", payload: `{"cmd":"go test ./..."}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			messages := codexInstructionRead("/skills/go-service/SKILL.md")
			if test.source == domain.SourceClaude {
				messages = claudeInstructionRead("/skills/go-service/SKILL.md")
			}
			messages = append(messages,
				domain.Message{Role: "tool", ToolName: test.toolName, ToolCallID: "validation", Text: test.payload},
				domain.Message{Role: "tool", ToolName: test.toolName, ToolCallID: "validation", ToolResult: true, Text: "ok"},
			)
			session := domain.Session{Source: test.source, ExternalID: test.name, Messages: messages}
			if cards := NewAnalyzer().Analyze(session, []domain.Skill{skill}); len(cards) != 0 {
				t.Fatalf("untrusted validation command created cards: %#v", cards)
			}
		})
	}
}

func TestAnalyzeAcceptsSupportedValidationCommandsAndSafeWrappers(t *testing.T) {
	skill := domain.Skill{
		ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
		InstructionPath: "SKILL.md", Enabled: true,
	}
	commands := map[string]string{
		"go":                  "go test ./...",
		"golangci lint":       "golangci-lint run --timeout 5m",
		"nix flake":           "nix flake check path:.",
		"pytest":              "pytest -q",
		"cargo":               "cargo test --workspace",
		"npm":                 "npm test -- --runInBand",
		"pnpm":                "pnpm test",
		"just":                "just test",
		"nix develop wrapper": "nix develop path:. --command go test ./...",
		"env wrapper":         "env CGO_ENABLED=0 go test ./...",
		"sudo wrapper":        "sudo -n go test ./...",
		"command wrapper":     "command -- go test ./...",
		"shell wrapper":       "sh -c 'go test ./...'",
	}
	for name, command := range commands {
		t.Run(name, func(t *testing.T) {
			messages := append(codexInstructionRead("/skills/go-service/SKILL.md"),
				domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "validation", Text: `{"cmd":` + quotedJSON(t, command) + `}`},
				domain.Message{Role: "tool", ToolName: "exec_command", ToolCallID: "validation", ToolResult: true, Text: "ok"},
			)
			session := domain.Session{Source: domain.SourceCodex, ExternalID: name, Messages: messages}
			cards := NewAnalyzer().Analyze(session, []domain.Skill{skill})
			if len(cards) != 1 || cards[0].Kind != domain.CardValidation {
				t.Fatalf("supported validation command produced %#v", cards)
			}
		})
	}

	messages := append(claudeInstructionRead("/skills/go-service/SKILL.md"),
		domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "validation", Text: `{"command":"go test ./..."}`},
		domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "validation", ToolResult: true, Text: "ok"},
	)
	session := domain.Session{Source: domain.SourceClaude, ExternalID: "claude-bash", Messages: messages}
	if cards := NewAnalyzer().Analyze(session, []domain.Skill{skill}); len(cards) != 1 || cards[0].Kind != domain.CardValidation {
		t.Fatalf("Claude Bash validation produced %#v", cards)
	}
}

func TestAnalyzeDoesNotTreatUnrelatedSuccessfulToolAsRecovery(t *testing.T) {
	session := domain.Session{
		Source: domain.SourceCodex, ExternalID: "session-unrelated-recovery",
		Messages: append(codexInstructionRead("/skills/go-service/SKILL.md"),
			domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "call-1", Text: `{"command":"go test ./..."}`},
			domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "call-1", ToolResult: true, Text: "exit code 1", Failed: true},
			domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "call-2", Text: `{"command":"git status --short"}`},
			domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "call-2", ToolResult: true, Text: "clean"},
		),
	}
	skills := []domain.Skill{{
		ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
		InstructionPath: "SKILL.md", Enabled: true,
	}}
	cards := NewAnalyzer().Analyze(session, skills)
	if len(cards) != 1 || cards[0].Kind != domain.CardFailure {
		t.Fatalf("expected one failure card, got %#v", cards)
	}
	if contains(cards[0].Lesson, "git status") || contains(cards[0].Lesson, "validated recovery") {
		t.Fatalf("unrelated success was retained as a recovery: %#v", cards[0])
	}
}

func TestCommandFamilyUnwrapsCommonCommandPayloads(t *testing.T) {
	tests := []struct {
		name      string
		failed    string
		candidate string
		related   bool
	}{
		{name: "validation wrapper", failed: `{"cmd":"go test ./..."}`, candidate: `{"command":"nix develop --command go test ./..."}`, related: true},
		{name: "same subcommand", failed: `{"command":"git status"}`, candidate: `{"command":"sudo git status --short"}`, related: true},
		{name: "different subcommand", failed: `{"command":"git status"}`, candidate: `{"command":"git add ."}`, related: false},
		{name: "shell wrapper", failed: `{"command":"make verify"}`, candidate: `{"command":"bash -c 'make verify'"}`, related: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := relatedToolCalls(test.failed, test.candidate); got != test.related {
				t.Fatalf("relatedToolCalls() = %v, want %v", got, test.related)
			}
		})
	}
}

func TestAnalyzeRejectsAmbiguousAttribution(t *testing.T) {
	session := domain.Session{Source: domain.SourceClaude, ExternalID: "session-2", Messages: append(
		claudeInstructionRead("/skills/alpha/SKILL.md"),
		append(claudeInstructionReadWithID("/skills/beta/SKILL.md", "read-beta"),
			domain.Message{Role: "user", Text: "No, instead run the tests."},
		)...,
	)}
	skills := []domain.Skill{
		{ID: "a", Name: "alpha", RepositoryPath: "/skills/alpha", InstructionPath: "SKILL.md", Enabled: true},
		{ID: "b", Name: "beta", RepositoryPath: "/skills/beta", InstructionPath: "SKILL.md", Enabled: true},
	}
	if cards := NewAnalyzer().Analyze(session, skills); len(cards) != 0 {
		t.Fatalf("expected ambiguous attribution to be rejected, got %#v", cards)
	}
}

func TestAnalyzeRequiresSuccessfulCorrelatedInstructionEvidence(t *testing.T) {
	instruction := "/skills/go-service/SKILL.md"
	skill := domain.Skill{
		ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
		InstructionPath: "SKILL.md", Enabled: true,
	}
	tests := []struct {
		name     string
		messages []domain.Message
	}{
		{
			name: "user path mention",
			messages: []domain.Message{
				{Role: "user", Text: "No, instead change " + instruction},
			},
		},
		{
			name: "assistant load claim",
			messages: []domain.Message{
				{Role: "assistant", Text: "Loaded " + instruction},
				{Role: "user", Text: "No, instead run the tests"},
			},
		},
		{
			name: "uncorrelated result mention",
			messages: []domain.Message{
				{Role: "tool", ToolName: "Read", ToolCallID: "missing", ToolResult: true, Text: instruction},
				{Role: "user", Text: "No, instead run the tests"},
			},
		},
		{
			name: "failed read",
			messages: append([]domain.Message{
				{Role: "tool", ToolName: "Read", ToolCallID: "read", Text: `{"file_path":"` + instruction + `"}`},
				{Role: "tool", ToolName: "Read", ToolCallID: "read", ToolResult: true, Failed: true, Text: "permission denied"},
			}, domain.Message{Role: "user", Text: "No, instead run the tests"}),
		},
		{
			name: "read result contains enoent despite missing failure flag",
			messages: []domain.Message{
				{Role: "tool", ToolName: "Read", ToolCallID: "read", Text: `{"file_path":"` + instruction + `"}`},
				{Role: "tool", ToolName: "Read", ToolCallID: "read", ToolResult: true, Text: "ENOENT: no such file or directory"},
				{Role: "user", Text: "No, instead run the tests"},
			},
		},
		{
			name: "successful echo",
			messages: []domain.Message{
				{Role: "tool", ToolName: "exec_command", ToolCallID: "echo", Text: `{"cmd":"echo ` + instruction + `"}`},
				{Role: "tool", ToolName: "exec_command", ToolCallID: "echo", ToolResult: true, Text: instruction},
				{Role: "user", Text: "No, instead run the tests"},
			},
		},
		{
			name: "reader help does not read path",
			messages: []domain.Message{
				{Role: "tool", ToolName: "exec_command", ToolCallID: "help", Text: `{"cmd":"cat --help ` + instruction + `"}`},
				{Role: "tool", ToolName: "exec_command", ToolCallID: "help", ToolResult: true, Text: "usage"},
				{Role: "user", Text: "No, instead run the tests"},
			},
		},
		{
			name: "read path only as search option",
			messages: []domain.Message{
				{Role: "tool", ToolName: "exec_command", ToolCallID: "search", Text: `{"cmd":"rg pattern --glob ` + instruction + ` other.md"}`},
				{Role: "tool", ToolName: "exec_command", ToolCallID: "search", ToolResult: true, Text: "ok"},
				{Role: "user", Text: "No, instead run the tests"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := domain.Session{Source: domain.SourceCodex, ExternalID: test.name, Messages: test.messages}
			if cards := NewAnalyzer().Analyze(session, []domain.Skill{skill}); len(cards) != 0 {
				t.Fatalf("untrusted evidence created cards: %#v", cards)
			}
		})
	}
}

func TestAnalyzeRejectsForgedToolNamesForSkillAttribution(t *testing.T) {
	instruction := "/skills/go-service/SKILL.md"
	skill := domain.Skill{
		ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
		InstructionPath: "SKILL.md", Enabled: true,
	}
	tests := []struct {
		name     string
		source   domain.Source
		toolName string
		payload  string
	}{
		{name: "codex database read", source: domain.SourceCodex, toolName: "database_read", payload: `{"path":"` + instruction + `"}`},
		{name: "codex suffix read", source: domain.SourceCodex, toolName: "mcp__filesystem__read", payload: `{"path":"` + instruction + `"}`},
		{name: "codex suffix shell", source: domain.SourceCodex, toolName: "exec_command_suffix", payload: `{"cmd":"cat ` + instruction + `"}`},
		{name: "codex cross provider read", source: domain.SourceCodex, toolName: "Read", payload: `{"file_path":"` + instruction + `"}`},
		{name: "claude database read", source: domain.SourceClaude, toolName: "database_read", payload: `{"path":"` + instruction + `"}`},
		{name: "claude namespaced read", source: domain.SourceClaude, toolName: "tools__Read", payload: `{"file_path":"` + instruction + `"}`},
		{name: "claude suffix read", source: domain.SourceClaude, toolName: "Read_suffix", payload: `{"file_path":"` + instruction + `"}`},
		{name: "claude lowercase read", source: domain.SourceClaude, toolName: "read", payload: `{"file_path":"` + instruction + `"}`},
		{name: "claude suffix bash", source: domain.SourceClaude, toolName: "Bash_suffix", payload: `{"command":"cat ` + instruction + `"}`},
		{name: "claude cross provider shell", source: domain.SourceClaude, toolName: "exec_command", payload: `{"cmd":"cat ` + instruction + `"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := domain.Session{Source: test.source, ExternalID: test.name, Messages: []domain.Message{
				{Role: "tool", ToolName: test.toolName, ToolCallID: "forged", Text: test.payload},
				{Role: "tool", ToolName: test.toolName, ToolCallID: "forged", ToolResult: true, Text: "skill instructions"},
				{Role: "user", Text: "No, instead run the tests"},
			}}
			if cards := NewAnalyzer().Analyze(session, []domain.Skill{skill}); len(cards) != 0 {
				t.Fatalf("forged tool name attributed a skill: %#v", cards)
			}
		})
	}
}

func TestAnalyzeAttributesOnlyTheSkillWithTrustedReadEvidence(t *testing.T) {
	session := domain.Session{
		Source: domain.SourceCodex, ExternalID: "trusted-read-wins",
		Messages: append(codexInstructionRead("/skills/actual/SKILL.md"),
			domain.Message{Role: "user", Text: "No, instead record this for target-skill and /skills/target/SKILL.md"},
		),
	}
	skills := []domain.Skill{
		{ID: "actual", Name: "actual-skill", RepositoryPath: "/skills/actual", InstructionPath: "SKILL.md", Enabled: true},
		{ID: "target", Name: "target-skill", RepositoryPath: "/skills/target", InstructionPath: "SKILL.md", Enabled: true},
	}
	cards := NewAnalyzer().Analyze(session, skills)
	if len(cards) != 1 || cards[0].SkillID != "actual" {
		t.Fatalf("cards = %#v, want only the actually read skill", cards)
	}
}

func TestAnalyzeAcceptsProviderNormalizedInstructionReads(t *testing.T) {
	instruction := "/skills/go-service/SKILL.md"
	skill := domain.Skill{
		ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
		InstructionPath: "SKILL.md", Enabled: true,
	}
	tests := []struct {
		name     string
		source   domain.Source
		messages []domain.Message
	}{
		{name: "codex shell read", source: domain.SourceCodex, messages: codexInstructionRead(instruction)},
		{name: "claude read tool", source: domain.SourceClaude, messages: claudeInstructionRead(instruction)},
		{
			name: "claude skill loader", source: domain.SourceClaude,
			messages: []domain.Message{
				{Role: "tool", ToolName: "Skill", ToolCallID: "skill-load", Text: `{"skill":"go-service"}`},
				{Role: "tool", ToolName: "Skill", ToolCallID: "skill-load", ToolResult: true, Text: "loaded"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			messages := make([]domain.Message, 0, len(test.messages)+1)
			messages = append(messages, test.messages...)
			messages = append(messages, domain.Message{Role: "user", Text: "No, instead run the tests"})
			session := domain.Session{Source: test.source, ExternalID: test.name, Messages: messages}
			cards := NewAnalyzer().Analyze(session, []domain.Skill{skill})
			if len(cards) != 1 || cards[0].SkillID != skill.ID || cards[0].Kind != domain.CardCorrection {
				t.Fatalf("cards = %#v, want one attributed correction", cards)
			}
		})
	}
}

func TestFailureFingerprintBindsNormalizedRecovery(t *testing.T) {
	makeCard := func(sessionID, recovery string) domain.LearningCard {
		session := domain.Session{
			Source: domain.SourceCodex, ExternalID: sessionID,
			Messages: append(codexInstructionRead("/skills/go-service/SKILL.md"),
				domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "failed", Text: `{"command":"go test ./..."}`},
				domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "failed", ToolResult: true, Failed: true, Text: "exit code 1"},
				domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "recovery", Text: recovery},
				domain.Message{Role: "tool", ToolName: "Bash", ToolCallID: "recovery", ToolResult: true, Text: "ok"},
			),
		}
		skill := domain.Skill{
			ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service",
			InstructionPath: "SKILL.md", Enabled: true,
		}
		cards := NewAnalyzer().Analyze(session, []domain.Skill{skill})
		for _, card := range cards {
			if card.Kind == domain.CardFailure {
				return card
			}
		}
		t.Fatalf("no failure card in %#v", cards)
		return domain.LearningCard{}
	}

	first := makeCard("a", `{"command":"nix develop --command go test ./..."}`)
	concordant := makeCard("b", `{"command":"nix   develop --command go test ./..."}`)
	disagreeing := makeCard("c", `{"command":"go test -run Hostile ./..."}`)
	if first.Fingerprint != concordant.Fingerprint {
		t.Fatalf("normalized concordant recoveries split: %q != %q", first.Fingerprint, concordant.Fingerprint)
	}
	if first.Fingerprint == disagreeing.Fingerprint {
		t.Fatalf("disagreeing recovery inherited fingerprint %q", first.Fingerprint)
	}
}

func codexInstructionRead(path string) []domain.Message {
	return []domain.Message{
		{Role: "tool", ToolName: "exec_command", ToolCallID: "read-skill", Text: `{"cmd":"sed -n '1,240p' '` + path + `'"}`},
		{Role: "tool", ToolName: "exec_command", ToolCallID: "read-skill", ToolResult: true, Text: "skill instructions"},
	}
}

func claudeInstructionRead(path string) []domain.Message {
	return claudeInstructionReadWithID(path, "read-skill")
}

func claudeInstructionReadWithID(path, id string) []domain.Message {
	return []domain.Message{
		{Role: "tool", ToolName: "Read", ToolCallID: id, Text: `{"file_path":"` + path + `"}`},
		{Role: "tool", ToolName: "Read", ToolCallID: id, ToolResult: true, Text: "skill instructions"},
	}
}

func contains(value, substring string) bool {
	return strings.Contains(value, substring)
}

func quotedJSON(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
