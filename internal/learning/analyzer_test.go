package learning

import (
	"strings"
	"testing"

	"github.com/flemzord/skillloop/internal/domain"
)

func TestAnalyzeAttributedCorrectionAndFailure(t *testing.T) {
	session := domain.Session{
		Source: domain.SourceCodex, ExternalID: "session-1",
		Messages: []domain.Message{
			{Role: "assistant", Text: "Loaded /skills/go-service/SKILL.md"},
			{Role: "tool", ToolName: "Bash", ToolCallID: "call-1", Text: `{"command":"go test ./..."}`},
			{Role: "tool", ToolName: "Bash", ToolCallID: "call-1", ToolResult: true, Text: "exit code 1: package failed", Failed: true},
			{Role: "user", Text: "Non, il faut utiliser nix develop."},
			{Role: "tool", ToolName: "Bash", ToolCallID: "call-2", Text: `{"command":"nix develop --command go test ./..."}`},
			{Role: "tool", ToolName: "Bash", ToolCallID: "call-2", ToolResult: true, Text: "ok"},
		},
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
		Messages: []domain.Message{
			{Role: "assistant", Text: "Loaded /skills/go-service/SKILL.md"},
			{Role: "tool", ToolName: "Bash", ToolCallID: "tool-1", Text: `{"command":"go test ./..."}`},
			{Role: "tool", ToolName: "Bash", ToolCallID: "tool-1", ToolResult: true, Text: "FAIL\nexit status 1", Failed: true},
		},
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
		Messages: []domain.Message{
			{Role: "assistant", Text: "Loaded /skills/go-service/SKILL.md"},
			{Role: "tool", ToolName: "exec_command", ToolCallID: "call-1", Text: `{"cmd":"go test ./..."}`, Failed: true},
		},
	}
	skills := []domain.Skill{{ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service", InstructionPath: "SKILL.md", Enabled: true}}
	if cards := NewAnalyzer().Analyze(session, skills); len(cards) != 0 {
		t.Fatalf("expected tool calls without results to produce no cards, got %#v", cards)
	}
}

func TestAnalyzeDoesNotInferValidationFromResultText(t *testing.T) {
	session := domain.Session{
		Source: domain.SourceClaude, ExternalID: "session-result-text",
		Messages: []domain.Message{
			{Role: "assistant", Text: "Loaded /skills/go-service/SKILL.md"},
			{Role: "tool", ToolName: "Bash", ToolCallID: "tool-1", Text: `{"command":"echo done"}`},
			{Role: "tool", ToolName: "Bash", ToolCallID: "tool-1", ToolResult: true, Text: "go test passed"},
		},
	}
	skills := []domain.Skill{{ID: "skill-1", Name: "go-service", RepositoryPath: "/skills/go-service", InstructionPath: "SKILL.md", Enabled: true}}
	if cards := NewAnalyzer().Analyze(session, skills); len(cards) != 0 {
		t.Fatalf("expected validation detection to inspect the associated call, got %#v", cards)
	}
}

func TestAnalyzeRejectsAmbiguousAttribution(t *testing.T) {
	session := domain.Session{Source: domain.SourceClaude, ExternalID: "session-2", Messages: []domain.Message{
		{Role: "assistant", Text: "Use alpha and beta"},
		{Role: "user", Text: "No, instead run the tests."},
	}}
	skills := []domain.Skill{
		{ID: "a", Name: "alpha", Enabled: true},
		{ID: "b", Name: "beta", Enabled: true},
	}
	if cards := NewAnalyzer().Analyze(session, skills); len(cards) != 0 {
		t.Fatalf("expected ambiguous attribution to be rejected, got %#v", cards)
	}
}

func contains(value, substring string) bool {
	return strings.Contains(value, substring)
}
