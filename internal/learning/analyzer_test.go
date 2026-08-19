package learning

import (
	"testing"

	"github.com/flemzord/skillloop/internal/domain"
)

func TestAnalyzeAttributedCorrectionAndFailure(t *testing.T) {
	session := domain.Session{
		Source: domain.SourceCodex, ExternalID: "session-1",
		Messages: []domain.Message{
			{Role: "assistant", Text: "Loaded /skills/go-service/SKILL.md"},
			{Role: "tool", ToolName: "Bash", Text: "go test failed with exit code 1", Failed: true},
			{Role: "user", Text: "Non, il faut utiliser nix develop."},
			{Role: "tool", ToolName: "Bash", Text: "nix develop --command go test ./...", Failed: false},
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
