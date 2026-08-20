package learning

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
			cards = append(cards, analyzer.card(
				sessionRef, skill.ID, domain.CardCorrection, lesson, "Explicit user correction", lesson, 0.9,
			))
		case message.Role == "tool" && message.ToolResult && message.Failed:
			call := toolCalls[index]
			recovery := nextSuccessfulTool(session.Messages, toolCalls, index+1, message.ToolName, call)
			fact := sanitize.Text(message.ToolName + " " + call.Text + " " + message.Text)
			lesson := "Handle the recurring failure before continuing"
			if recovery != "" {
				lesson = "After this failure, use the validated recovery: " + sanitize.Text(recovery)
			}
			cards = append(cards, analyzer.card(
				sessionRef, skill.ID, domain.CardFailure, fact, "Recurring tool failure", lesson, 0.8,
			))
		case message.Role == "tool" && message.ToolResult && !message.Failed:
			call, correlated := toolCalls[index]
			command, validation := validationToolCall(session.Source, call)
			if correlated && validation {
				cards = append(cards, analyzer.card(
					sessionRef, skill.ID, domain.CardValidation, command, "Successful validation step", command, 0.75,
				))
			}
		}
	}
	return deduplicate(cards)
}

func (analyzer Analyzer) card(
	sessionRef, skillID string,
	kind domain.CardKind,
	fact, summary, lesson string,
	confidence float64,
) domain.LearningCard {
	now := analyzer.now
	if now == nil {
		now = time.Now
	}
	fact = sanitize.Text(fact)
	lesson = sanitize.Text(lesson)
	fingerprint := learningFingerprint(kind, fact, lesson)
	id := stableID(sessionRef, skillID, string(kind), fingerprint)
	return domain.LearningCard{
		ID:          id,
		SessionRef:  sessionRef,
		SkillID:     skillID,
		Kind:        kind,
		Fingerprint: fingerprint,
		Summary:     summary,
		Lesson:      lesson,
		Confidence:  confidence,
		CreatedAt:   now().UTC(),
	}
}

func learningFingerprint(kind domain.CardKind, fact, lesson string) string {
	base := sanitize.Fingerprint(string(kind) + " " + fact)
	normalizedLesson := strings.ToLower(sanitize.Text(lesson))
	return base + " lesson-" + stableID(normalizedLesson)
}

func attributedSkill(session domain.Session, skills []domain.Skill) (domain.Skill, bool) {
	toolCalls := correlateToolCalls(session.Messages)
	matches := make(map[string]domain.Skill, len(skills))
	for index, result := range session.Messages {
		if result.Role != "tool" || !result.ToolResult || result.Failed || resultReportsReadFailure(result.Text) {
			continue
		}
		call, correlated := toolCalls[index]
		if !correlated {
			continue
		}
		for _, skill := range skills {
			if skill.Enabled && isInstructionRead(session.Source, call, skill, session.WorkingDir) {
				matches[skill.ID] = skill
			}
		}
	}
	if len(matches) != 1 {
		return domain.Skill{}, false
	}
	for _, skill := range matches {
		return skill, true
	}
	return domain.Skill{}, false
}

func isInstructionRead(source domain.Source, call domain.Message, skill domain.Skill, workingDir string) bool {
	toolName := strings.TrimSpace(call.ToolName)
	if source == domain.SourceClaude && toolName == "Skill" {
		loadedSkill := strings.TrimSpace(stringArgument(call.Text, "skill", "name"))
		return loadedSkill != "" && strings.EqualFold(loadedSkill, strings.TrimSpace(skill.Name))
	}

	expected := skill.InstructionPath
	if !filepath.IsAbs(expected) {
		expected = filepath.Join(skill.RepositoryPath, expected)
	}
	expected = filepath.Clean(expected)

	if source == domain.SourceClaude && toolName == "Read" {
		for _, path := range stringArguments(call.Text, "file_path", "path", "filename") {
			if sameInstructionPath(path, expected, workingDir) {
				return true
			}
		}
		return sameInstructionPath(strings.TrimSpace(call.Text), expected, workingDir)
	}
	if !isProviderShellTool(source, toolName) {
		return false
	}
	return shellReadsInstruction(commandText(call.Text), expected, workingDir)
}

func stringArgument(value string, keys ...string) string {
	values := stringArguments(value, keys...)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func stringArguments(value string, keys ...string) []string {
	var payload map[string]any
	if err := json.Unmarshal([]byte(value), &payload); err != nil {
		return nil
	}
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		switch typed := payload[key].(type) {
		case string:
			result = append(result, typed)
		case []any:
			for _, item := range typed {
				if text, ok := item.(string); ok {
					result = append(result, text)
				}
			}
		}
	}
	return result
}

func isProviderShellTool(source domain.Source, name string) bool {
	switch source {
	case domain.SourceCodex:
		return name == "exec_command"
	case domain.SourceClaude:
		return name == "Bash"
	default:
		return false
	}
}

func resultReportsReadFailure(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	for _, marker := range []string{
		"enoent", "no such file or directory", "permission denied", "iserror\":true", "is_error\":true",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func sameInstructionPath(value, expected, workingDir string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsRune(value, '\x00') {
		return false
	}
	if !filepath.IsAbs(value) {
		if workingDir == "" {
			return false
		}
		value = filepath.Join(workingDir, value)
	}
	value = filepath.Clean(value)
	if value == expected {
		return true
	}
	resolvedValue, valueErr := filepath.EvalSymlinks(value)
	resolvedExpected, expectedErr := filepath.EvalSymlinks(expected)
	return valueErr == nil && expectedErr == nil && resolvedValue == resolvedExpected
}

func shellReadsInstruction(command, expected, workingDir string) bool {
	words, ok := conservativeShellWords(command)
	if !ok || len(words) < 2 {
		return false
	}
	words = unwrapReadCommand(words)
	if len(words) < 2 {
		return false
	}
	program := filepath.Base(words[0])
	arguments := words[1:]
	switch program {
	case "cat":
		return catReadsInstruction(arguments, expected, workingDir)
	case "bat", "nl":
		return plainReaderReadsInstruction(arguments, expected, workingDir)
	case "sed":
		return sedReadsInstruction(arguments, expected, workingDir)
	case "grep", "rg":
		return searchReadsInstruction(arguments, expected, workingDir)
	default:
		return false
	}
}

func unwrapReadCommand(words []string) []string {
	for len(words) > 0 {
		switch strings.ToLower(filepath.Base(words[0])) {
		case "command", "sudo", "env":
			words = words[1:]
			for len(words) > 0 && (strings.HasPrefix(words[0], "-") || strings.Contains(words[0], "=")) {
				words = words[1:]
			}
		case "sh", "bash", "zsh":
			if len(words) != 3 || words[1] != "-c" {
				return nil
			}
			nested, ok := conservativeShellWords(words[2])
			if !ok {
				return nil
			}
			words = nested
		case "nix":
			commandIndex := -1
			for index, word := range words[1:] {
				if word == "--command" {
					commandIndex = index + 1
					break
				}
			}
			if commandIndex < 0 || commandIndex+1 >= len(words) {
				return nil
			}
			words = words[commandIndex+1:]
		default:
			return words
		}
	}
	return nil
}

func containsInstructionPath(arguments []string, expected, workingDir string) bool {
	for _, argument := range arguments {
		if sameInstructionPath(argument, expected, workingDir) {
			return true
		}
	}
	return false
}

func catReadsInstruction(arguments []string, expected, workingDir string) bool {
	allowedLongFlags := map[string]bool{
		"--number": true, "--number-nonblank": true, "--show-all": true,
		"--show-ends": true, "--show-nonprinting": true, "--show-tabs": true,
		"--squeeze-blank": true,
	}
	for _, argument := range arguments {
		if strings.HasPrefix(argument, "--") && argument != "--" && !allowedLongFlags[argument] {
			return false
		}
		if strings.HasPrefix(argument, "-") && !strings.HasPrefix(argument, "--") {
			for _, flag := range strings.TrimPrefix(argument, "-") {
				if !strings.ContainsRune("AbEnstuv", flag) {
					return false
				}
			}
		}
	}
	return containsInstructionPath(arguments, expected, workingDir)
}

func plainReaderReadsInstruction(arguments []string, expected, workingDir string) bool {
	for _, argument := range arguments {
		if strings.HasPrefix(argument, "-") && argument != "--" {
			return false
		}
	}
	return containsInstructionPath(arguments, expected, workingDir)
}

func sedReadsInstruction(arguments []string, expected, workingDir string) bool {
	programProvided := false
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "-i" || argument == "--in-place" || strings.HasPrefix(argument, "-i") {
			return false
		}
		if argument == "-e" || argument == "--expression" || argument == "-f" || argument == "--file" {
			if index+1 >= len(arguments) {
				return false
			}
			index++
			programProvided = true
			continue
		}
		if strings.HasPrefix(argument, "-") {
			continue
		}
		if !programProvided {
			programProvided = true
			continue
		}
		if sameInstructionPath(argument, expected, workingDir) {
			return true
		}
	}
	return false
}

func searchReadsInstruction(arguments []string, expected, workingDir string) bool {
	allowedLongFlags := map[string]bool{
		"--fixed-strings": true, "--ignore-case": true, "--line-number": true,
		"--multiline": true, "--smart-case": true, "--with-filename": true,
		"--word-regexp": true,
	}
	for _, argument := range arguments {
		if strings.HasPrefix(argument, "--") && !allowedLongFlags[argument] {
			return false
		}
		if strings.HasPrefix(argument, "-") && !strings.HasPrefix(argument, "--") {
			for _, flag := range strings.TrimPrefix(argument, "-") {
				if !strings.ContainsRune("FinHhSUwx", flag) {
					return false
				}
			}
		}
	}
	return positionalSearchReadsInstruction(arguments, expected, workingDir)
}

func positionalSearchReadsInstruction(arguments []string, expected, workingDir string) bool {
	patternProvided := false
	for _, argument := range arguments {
		if strings.HasPrefix(argument, "-") {
			continue
		}
		if !patternProvided {
			patternProvided = true
			continue
		}
		if sameInstructionPath(argument, expected, workingDir) {
			return true
		}
	}
	return false
}

func conservativeShellWords(value string) ([]string, bool) {
	var words []string
	var word strings.Builder
	var quote rune
	escaped := false
	flush := func() {
		if word.Len() > 0 {
			words = append(words, word.String())
			word.Reset()
		}
	}
	for _, character := range value {
		if escaped {
			word.WriteRune(character)
			escaped = false
			continue
		}
		if character == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			} else {
				if character == '$' || character == '`' {
					return nil, false
				}
				word.WriteRune(character)
			}
			continue
		}
		switch character {
		case '\'', '"':
			quote = character
		case ' ', '\t':
			flush()
		case '\n', '\r', ';', '|', '&', '<', '>', '$', '`', '#':
			return nil, false
		default:
			word.WriteRune(character)
		}
	}
	if escaped || quote != 0 {
		return nil, false
	}
	flush()
	return words, len(words) > 0
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

func nextSuccessfulTool(
	messages []domain.Message,
	toolCalls map[int]domain.Message,
	start int,
	name string,
	failedCall domain.Message,
) string {
	for index := start; index < len(messages); index++ {
		message := messages[index]
		if message.Role != "tool" || !message.ToolResult || message.Failed {
			continue
		}
		call, ok := toolCalls[index]
		if !ok {
			continue
		}
		if (name == "" || message.ToolName == "" || strings.EqualFold(name, message.ToolName)) &&
			relatedToolCalls(failedCall.Text, call.Text) {
			return call.Text
		}
	}
	return ""
}

func relatedToolCalls(failed, candidate string) bool {
	failedFamily := commandFamily(failed)
	return failedFamily != "" && failedFamily == commandFamily(candidate)
}

func commandFamily(value string) string {
	command := commandText(value)
	if validation, ok := recognizedValidationCommand(command); ok {
		return validation
	}
	fields := strings.Fields(strings.ToLower(command))
	for len(fields) > 0 && strings.Contains(fields[0], "=") {
		fields = fields[1:]
	}
	for len(fields) > 0 && (fields[0] == "sudo" || fields[0] == "env") {
		fields = fields[1:]
		for len(fields) > 0 && strings.HasPrefix(fields[0], "-") {
			fields = fields[1:]
		}
		for len(fields) > 0 && strings.Contains(fields[0], "=") {
			fields = fields[1:]
		}
	}
	if len(fields) >= 3 && (fields[0] == "sh" || fields[0] == "bash" || fields[0] == "zsh") && fields[1] == "-c" {
		return commandFamily(strings.Trim(strings.Join(fields[2:], " "), "'\""))
	}
	if len(fields) == 0 {
		return ""
	}
	if len(fields) == 1 {
		return strings.Trim(fields[0], "'\"")
	}
	return strings.Trim(fields[0], "'\"") + " " + strings.Trim(fields[1], "'\"")
}

func commandText(value string) string {
	var payload map[string]any
	if err := json.Unmarshal([]byte(value), &payload); err == nil {
		for _, key := range []string{"command", "cmd"} {
			if command, ok := payload[key].(string); ok {
				return command
			}
		}
	}
	return value
}

func validationToolCall(source domain.Source, call domain.Message) (string, bool) {
	if !isProviderShellTool(source, strings.TrimSpace(call.ToolName)) {
		return "", false
	}
	command, ok := structuredCommandText(source, call.Text)
	if !ok {
		return "", false
	}
	return recognizedValidationCommand(command)
}

func structuredCommandText(source domain.Source, value string) (string, bool) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &payload); err != nil {
		return "", false
	}
	key := ""
	switch source {
	case domain.SourceCodex:
		key = "cmd"
	case domain.SourceClaude:
		key = "command"
	default:
		return "", false
	}
	var command string
	if err := json.Unmarshal(payload[key], &command); err != nil || strings.TrimSpace(command) == "" {
		return "", false
	}
	return command, true
}

func recognizedValidationCommand(value string) (string, bool) {
	words, ok := conservativeShellWords(value)
	if !ok {
		return "", false
	}
	words, ok = unwrapValidationCommand(words)
	if !ok || len(words) == 0 {
		return "", false
	}
	program := filepath.Base(words[0])
	arguments := words[1:]
	switch {
	case program == "go" && firstArgumentIs(arguments, "test"):
		return "go test", true
	case program == "golangci-lint" && firstArgumentIs(arguments, "run"):
		return "golangci-lint", true
	case program == "nix" && len(arguments) >= 2 && arguments[0] == "flake" && arguments[1] == "check":
		return "nix flake check", true
	case program == "pytest":
		return "pytest", true
	case (program == "cargo" || program == "npm" || program == "pnpm" || program == "just") &&
		firstArgumentIs(arguments, "test"):
		return program + " test", true
	default:
		return "", false
	}
}

func firstArgumentIs(arguments []string, expected string) bool {
	return len(arguments) > 0 && arguments[0] == expected
}

func unwrapValidationCommand(words []string) ([]string, bool) {
	for len(words) > 0 {
		switch filepath.Base(words[0]) {
		case "command":
			words = words[1:]
			if len(words) > 0 && words[0] == "--" {
				words = words[1:]
			}
			if len(words) > 0 && strings.HasPrefix(words[0], "-") {
				return nil, false
			}
		case "sudo":
			words = words[1:]
			for len(words) > 0 && strings.HasPrefix(words[0], "-") {
				switch words[0] {
				case "--":
					words = words[1:]
				case "-n", "--non-interactive", "-E", "--preserve-env", "-H", "--set-home", "-k", "--reset-timestamp":
					words = words[1:]
				default:
					return nil, false
				}
			}
		case "env":
			var ok bool
			words, ok = unwrapEnvironment(words[1:])
			if !ok {
				return nil, false
			}
		case "sh", "bash", "zsh":
			if len(words) != 3 || words[1] != "-c" {
				return nil, false
			}
			nested, ok := conservativeShellWords(words[2])
			if !ok {
				return nil, false
			}
			words = nested
		case "nix":
			if len(words) >= 3 && words[1] == "flake" && words[2] == "check" {
				return words, true
			}
			if len(words) < 2 || words[1] != "develop" {
				return nil, false
			}
			commandIndex := -1
			for index, word := range words[2:] {
				if word == "--command" {
					commandIndex = index + 2
					break
				}
			}
			if commandIndex < 0 || commandIndex+1 >= len(words) {
				return nil, false
			}
			words = words[commandIndex+1:]
		default:
			return words, true
		}
	}
	return nil, false
}

func unwrapEnvironment(words []string) ([]string, bool) {
	for len(words) > 0 {
		switch {
		case words[0] == "--":
			return words[1:], true
		case words[0] == "-i" || words[0] == "--ignore-environment":
			words = words[1:]
		case isEnvironmentAssignment(words[0]):
			words = words[1:]
		case strings.HasPrefix(words[0], "-"):
			return nil, false
		default:
			return words, true
		}
	}
	return nil, false
}

func isEnvironmentAssignment(value string) bool {
	name, _, found := strings.Cut(value, "=")
	if !found || name == "" {
		return false
	}
	for index, character := range name {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || character == '_' ||
			(index > 0 && character >= '0' && character <= '9') {
			continue
		}
		return false
	}
	return true
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
