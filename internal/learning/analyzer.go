package learning

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/flemzord/skillloop/internal/domain"
	"github.com/flemzord/skillloop/internal/sanitize"
)

var correctionPattern = regexp.MustCompile(`(?i)(?:^|\b)(non[, :]|no[, :]|instead\b|plut[oô]t\b|pas (?:ça|cela|comme)|you should\b|il faut\b|tu dois\b|ce n['’]est pas)`) //nolint:lll

type Analyzer struct {
	now func() time.Time
}

func NewAnalyzer() Analyzer {
	return Analyzer{now: time.Now}
}

func (analyzer Analyzer) Analyze(session domain.Session, skills []domain.Skill) []domain.LearningCard {
	skill, ok := attributedSkill(session, skills)
	if !ok {
		return nil
	}
	sessionRef := session.Reference
	if sessionRef == "" {
		sessionRef = string(session.Source) + ":" + session.ExternalID
	}
	cards := make([]domain.LearningCard, 0, 4)
	toolCalls := correlateToolCalls(session.Messages)
	for index, message := range session.Messages {
		switch {
		case message.Role == "user" && correctionPattern.MatchString(message.Text):
			lesson := sanitize.Text(message.Text)
			cards = append(cards, analyzer.card(sessionRef, skill.ID, domain.CardCorrection, lesson, "Explicit user correction", 0.9))
		case message.Role == "tool" && message.ToolResult && message.Failed:
			call := toolCalls[index]
			recovery := nextSuccessfulTool(session.Messages, toolCalls, index+1, message.ToolName)
			fact := sanitize.Text(message.ToolName + " " + call.Text + " " + message.Text)
			lesson := "Handle the recurring failure before continuing"
			if recovery != "" {
				lesson = "After this failure, use the validated recovery: " + sanitize.Text(recovery)
			}
			cards = append(cards, analyzer.card(sessionRef, skill.ID, domain.CardFailure, fact, "Recurring tool failure", 0.8))
			cards[len(cards)-1].Lesson = lesson
		case message.Role == "tool" && message.ToolResult && !message.Failed && isValidation(toolCalls[index].Text):
			command := validationCommand(toolCalls[index].Text)
			cards = append(cards, analyzer.card(sessionRef, skill.ID, domain.CardValidation, command, "Successful validation step", 0.75))
		}
	}
	return deduplicate(cards)
}

func (analyzer Analyzer) card(sessionRef, skillID string, kind domain.CardKind, fact, summary string, confidence float64) domain.LearningCard {
	now := analyzer.now
	if now == nil {
		now = time.Now
	}
	fingerprint := sanitize.Fingerprint(string(kind) + " " + fact)
	id := stableID(sessionRef, skillID, string(kind), fingerprint)
	return domain.LearningCard{
		ID:          id,
		SessionRef:  sessionRef,
		SkillID:     skillID,
		Kind:        kind,
		Fingerprint: fingerprint,
		Summary:     summary,
		Lesson:      sanitize.Text(fact),
		Confidence:  confidence,
		CreatedAt:   now().UTC(),
	}
}

func attributedSkill(session domain.Session, skills []domain.Skill) (domain.Skill, bool) {
	type match struct {
		skill domain.Skill
		score int
	}
	matches := make([]match, 0, len(skills))
	for _, skill := range skills {
		if !skill.Enabled {
			continue
		}
		needles := []struct {
			value string
			score int
		}{
			{skill.InstructionPath, 4},
			{filepath.Join(skill.RepositoryPath, skill.InstructionPath), 5},
			{skill.Name, 2},
		}
		score := 0
		for _, message := range session.Messages {
			lower := strings.ToLower(message.Text)
			for _, needle := range needles {
				if needle.value != "" && strings.Contains(lower, strings.ToLower(needle.value)) && needle.score > score {
					score = needle.score
				}
			}
		}
		if score > 0 {
			matches = append(matches, match{skill: skill, score: score})
		}
	}
	if len(matches) == 0 {
		return domain.Skill{}, false
	}
	best := matches[0]
	unique := true
	for _, candidate := range matches[1:] {
		if candidate.score > best.score {
			best = candidate
			unique = true
		} else if candidate.score == best.score {
			unique = false
		}
	}
	return best.skill, unique
}

func correlateToolCalls(messages []domain.Message) map[int]domain.Message {
	byID := make(map[string]domain.Message)
	latestByName := make(map[string]domain.Message)
	result := make(map[int]domain.Message)
	for index, message := range messages {
		if message.Role != "tool" {
			continue
		}
		if !message.ToolResult {
			if message.ToolCallID != "" {
				byID[message.ToolCallID] = message
			} else {
				latestByName[strings.ToLower(message.ToolName)] = message
			}
			continue
		}
		if message.ToolCallID != "" {
			if call, ok := byID[message.ToolCallID]; ok {
				result[index] = call
				delete(byID, message.ToolCallID)
			}
			continue
		}
		key := strings.ToLower(message.ToolName)
		if call, ok := latestByName[key]; ok {
			result[index] = call
			delete(latestByName, key)
		}
	}
	return result
}

func nextSuccessfulTool(messages []domain.Message, toolCalls map[int]domain.Message, start int, name string) string {
	for index := start; index < len(messages); index++ {
		message := messages[index]
		if message.Role != "tool" || !message.ToolResult || message.Failed {
			continue
		}
		call, ok := toolCalls[index]
		if !ok {
			continue
		}
		if name == "" || message.ToolName == "" || strings.EqualFold(name, message.ToolName) {
			return call.Text
		}
	}
	return ""
}

func isValidation(value string) bool {
	lower := strings.ToLower(value)
	markers := []string{"go test", "golangci-lint", "nix flake check", "pytest", "cargo test", "npm test", "pnpm test", "just test"}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func validationCommand(value string) string {
	lower := strings.ToLower(value)
	commands := []string{"go test", "golangci-lint", "nix flake check", "pytest", "cargo test", "npm test", "pnpm test", "just test"}
	for _, command := range commands {
		if strings.Contains(lower, command) {
			return command
		}
	}
	return "validation"
}

func stableID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:16])
}

func deduplicate(cards []domain.LearningCard) []domain.LearningCard {
	seen := map[string]struct{}{}
	result := make([]domain.LearningCard, 0, len(cards))
	for _, card := range cards {
		key := fmt.Sprintf("%s:%s", card.Kind, card.Fingerprint)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, card)
	}
	return result
}
