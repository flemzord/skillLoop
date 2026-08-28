package learning

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/flemzord/skillloop/internal/activation"
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
		case message.Role == "user" && !injectedContext(message.Text) && correctionPattern.MatchString(message.Text):
			lesson := sanitize.Text(message.Text)
			cards = append(cards, analyzer.card(
				sessionRef, skill.ID, domain.CardCorrection, lesson, "Explicit user correction", lesson, 0.9,
			))
		case message.Role == "tool" && message.ToolResult && message.Failed:
			call, correlated := toolCalls[index]
			if !correlated {
				continue
			}
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
			evidence, validation := validationToolCall(session.Source, call, message)
			if correlated && validation {
				cards = append(cards, analyzer.card(
					sessionRef, skill.ID, domain.CardValidation,
					evidence.identity, "Successful validation step", evidence.command, 0.75,
				))
			}
		}
	}
	return deduplicate(cards)
}

func injectedContext(value string) bool {
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, "# AGENTS.md instructions") && strings.Contains(trimmed, "<INSTRUCTIONS>") {
		return true
	}
	for _, marker := range []string{
		"<skills_instructions>", "<environment_context>", "<permissions instructions>",
	} {
		if strings.HasPrefix(trimmed, marker) {
			return true
		}
	}
	return false
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
	instructionPaths := make(map[string][]string, len(skills))
	for _, skill := range skills {
		if skill.Enabled {
			instructionPaths[skill.ID] = trustedInstructionPaths(session.Source, skill)
		}
	}
	for index, result := range session.Messages {
		if result.Role != "tool" || !result.ToolResult || result.Failed || !readResultProvesContent(session.Source, result.Text) {
			continue
		}
		call, correlated := toolCalls[index]
		if !correlated {
			continue
		}
		for _, skill := range skills {
			if skill.Enabled && isInstructionRead(session.Source, call, skill, session.WorkingDir, instructionPaths[skill.ID]) {
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

func isInstructionRead(source domain.Source, call domain.Message, skill domain.Skill, workingDir string, expectedPaths []string) bool {
	toolName := strings.TrimSpace(call.ToolName)
	if source == domain.SourceClaude && toolName == "Skill" {
		loadedSkill := strings.TrimSpace(stringArgument(call.Text, "skill", "name"))
		return loadedSkill != "" && strings.EqualFold(loadedSkill, strings.TrimSpace(skill.Name))
	}

	if source == domain.SourceClaude && toolName == "Read" {
		for _, path := range stringArguments(call.Text, "file_path", "path", "filename") {
			if matchesInstructionPath(path, expectedPaths, workingDir) {
				return true
			}
		}
		return matchesInstructionPath(strings.TrimSpace(call.Text), expectedPaths, workingDir)
	}
	if !isProviderShellTool(source, toolName) {
		return false
	}
	for _, expected := range expectedPaths {
		if shellReadsInstruction(commandText(call.Text), expected, workingDir) {
			return true
		}
	}
	return false
}

func trustedInstructionPaths(source domain.Source, skill domain.Skill) []string {
	expected := skill.InstructionPath
	if !filepath.IsAbs(expected) {
		expected = filepath.Join(skill.RepositoryPath, expected)
	}
	expected = filepath.Clean(expected)
	paths := []string{expected}
	name, err := activation.SafeName(skill.Name)
	if err != nil {
		return paths
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return paths
	}
	roots := []string{filepath.Join(home, ".agents", "skills")}
	switch source {
	case domain.SourceCodex:
		root := os.Getenv("CODEX_HOME")
		if root == "" {
			root = filepath.Join(home, ".codex")
		}
		roots = append(roots, filepath.Join(root, "skills"))
	case domain.SourceClaude:
		root := os.Getenv("CLAUDE_CONFIG_DIR")
		if root == "" {
			root = filepath.Join(home, ".claude")
		}
		roots = append(roots, filepath.Join(root, "skills"))
	}
	for _, root := range roots {
		candidate := filepath.Join(root, name, "SKILL.md")
		if sameInstructionContents(expected, candidate) {
			paths = append(paths, candidate)
		}
	}
	return paths
}

func matchesInstructionPath(value string, expectedPaths []string, workingDir string) bool {
	for _, expected := range expectedPaths {
		if sameInstructionPath(value, expected, workingDir) {
			return true
		}
	}
	return false
}

func sameInstructionContents(source, installed string) bool {
	const maximumInstructionSize = 8 << 20
	sourceContents, ok := readInstructionContents(source, maximumInstructionSize)
	if !ok {
		return false
	}
	installedContents, ok := readInstructionContents(installed, maximumInstructionSize)
	return ok && bytes.Equal(sourceContents, installedContents)
}

func readInstructionContents(path string, maximumSize int64) ([]byte, bool) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maximumSize {
		return nil, false
	}
	contents, err := io.ReadAll(io.LimitReader(file, maximumSize+1))
	return contents, err == nil && int64(len(contents)) <= maximumSize
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

func readResultProvesContent(source domain.Source, value string) bool {
	if resultReportsReadFailure(value) {
		return false
	}
	if source == domain.SourceCodex {
		value = codexFinalOutput(value)
	}
	return strings.TrimSpace(value) != ""
}

func codexFinalOutput(value string) string {
	lines := strings.Split(value, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "Process exited with code 0" {
		return value
	}
	for index, line := range lines[1:] {
		if strings.TrimSpace(line) == "Final output:" {
			return strings.Join(lines[index+2:], "\n")
		}
	}
	return ""
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
	program, ok := trustedExecutableName(words[0])
	if !ok {
		return false
	}
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
		program, ok := trustedExecutableName(words[0])
		if !ok {
			return nil
		}
		switch program {
		case "command":
			words = words[1:]
			if len(words) > 0 && words[0] == "--" {
				words = words[1:]
			}
			if len(words) > 0 && strings.HasPrefix(words[0], "-") {
				return nil
			}
		case "env":
			words, ok = unwrapEnvironment(words[1:])
			if !ok {
				return nil
			}
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
	inputs, ok := sedInputPaths(arguments)
	return ok && containsInstructionPath(inputs, expected, workingDir)
}

func sedInputPaths(arguments []string) ([]string, bool) {
	programProvided := false
	optionsEnded := false
	inputs := make([]string, 0, len(arguments))
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if !optionsEnded && argument == "--" {
			optionsEnded = true
			continue
		}
		if !optionsEnded && strings.HasPrefix(argument, "--") {
			name, value, assigned := strings.Cut(argument, "=")
			switch name {
			case "--quiet", "--silent", "--debug", "--follow-symlinks", "--posix", "--regexp-extended",
				"--separate", "--unbuffered", "--null-data", "--sandbox":
				if assigned {
					return nil, false
				}
				continue
			case "--expression", "--file":
				if !assigned {
					index++
					if index >= len(arguments) {
						return nil, false
					}
					value = arguments[index]
				}
				if value == "" {
					return nil, false
				}
				programProvided = true
				continue
			case "--line-length":
				if !assigned {
					index++
					if index >= len(arguments) {
						return nil, false
					}
					value = arguments[index]
				}
				if _, err := strconv.ParseUint(value, 10, 64); err != nil {
					return nil, false
				}
				continue
			default:
				return nil, false
			}
		}
		if !optionsEnded && strings.HasPrefix(argument, "-") && argument != "-" {
			provided, consumed, ok := parseSedShortOptions(argument[1:], arguments[index+1:])
			if !ok {
				return nil, false
			}
			programProvided = programProvided || provided
			index += consumed
			continue
		}
		if !programProvided {
			programProvided = true
			continue
		}
		inputs = append(inputs, argument)
	}
	return inputs, programProvided && len(inputs) > 0
}

func parseSedShortOptions(options string, remaining []string) (bool, int, bool) {
	for index := 0; index < len(options); index++ {
		switch options[index] {
		case 'b', 'n', 'E', 'r', 's', 'u', 'z':
			continue
		case 'e', 'f':
			if index+1 < len(options) {
				return true, 0, true
			}
			if len(remaining) == 0 || remaining[0] == "" {
				return false, 0, false
			}
			return true, 1, true
		case 'l':
			value := options[index+1:]
			consumed := 0
			if value == "" {
				if len(remaining) == 0 {
					return false, 0, false
				}
				value = remaining[0]
				consumed = 1
			}
			if _, err := strconv.ParseUint(value, 10, 64); err != nil {
				return false, 0, false
			}
			return false, consumed, true
		default:
			return false, 0, false
		}
	}
	return false, 0, true
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
		return validation.identity
	}
	if validationCommandShape(command) {
		return ""
	}
	words, ok := conservativeShellWords(command)
	if !ok {
		return ""
	}
	words, ok = unwrapEvidenceCommand(words)
	if !ok || len(words) == 0 {
		return ""
	}
	if len(words) == 1 {
		return strings.ToLower(words[0])
	}
	return strings.ToLower(words[0] + " " + words[1])
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

type validationEvidence struct {
	command                 string
	identity                string
	kind                    string
	requiresDetailedGoProof bool
}

type validationInvocation struct {
	arguments        []string
	clearEnvironment bool
	environment      []string
}

func validationToolCall(source domain.Source, call, result domain.Message) (validationEvidence, bool) {
	if !isProviderShellTool(source, strings.TrimSpace(call.ToolName)) {
		return validationEvidence{}, false
	}
	command, ok := structuredCommandText(source, call.Text)
	if !ok {
		return validationEvidence{}, false
	}
	evidence, ok := recognizedValidationCommand(command)
	if !ok {
		return validationEvidence{}, false
	}
	if evidence.kind == "go" && !goTestResultProvesExecution(result.Text, evidence.requiresDetailedGoProof) {
		return validationEvidence{}, false
	}
	return evidence, true
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

func recognizedValidationCommand(value string) (validationEvidence, bool) {
	words, ok := conservativeShellWords(value)
	if !ok {
		return validationEvidence{}, false
	}
	invocation, ok := unwrapValidationCommand(words)
	if !ok || len(invocation.arguments) == 0 {
		return validationEvidence{}, false
	}
	program, ok := trustedExecutableName(invocation.arguments[0])
	if !ok {
		return validationEvidence{}, false
	}
	validationKind, validationArguments, supported := supportedValidationInvocation(program, invocation.arguments[1:])
	if !supported || !validationArgumentsExecute(validationKind, validationArguments) {
		return validationEvidence{}, false
	}

	encoded, err := json.Marshal(struct {
		Arguments        []string `json:"arguments"`
		ClearEnvironment bool     `json:"clear_environment"`
		Environment      []string `json:"environment,omitempty"`
	}{
		Arguments: invocation.arguments, ClearEnvironment: invocation.clearEnvironment,
		Environment: invocation.environment,
	})
	if err != nil {
		return validationEvidence{}, false
	}
	return validationEvidence{
		command:                 canonicalValidationCommand(invocation),
		identity:                "validation-command-" + stableID(string(encoded)),
		kind:                    validationKind,
		requiresDetailedGoProof: validationKind == "go" && goTestHasSelector(validationArguments),
	}, true
}

func validationCommandShape(value string) bool {
	words, ok := conservativeShellWords(value)
	if !ok {
		return false
	}
	invocation, ok := unwrapValidationCommand(words)
	if !ok || len(invocation.arguments) == 0 {
		return false
	}
	program, ok := trustedExecutableName(invocation.arguments[0])
	if !ok {
		return false
	}
	_, _, supported := supportedValidationInvocation(program, invocation.arguments[1:])
	return supported
}

func supportedValidationInvocation(program string, arguments []string) (string, []string, bool) {
	switch {
	case program == "go" && firstArgumentIs(arguments, "test"):
		return program, arguments[1:], true
	case program == "golangci-lint" && firstArgumentIs(arguments, "run"):
		return program, arguments[1:], true
	case program == "nix" && len(arguments) >= 2 && arguments[0] == "flake" && arguments[1] == "check":
		return "nix flake check", arguments[2:], true
	case program == "pytest":
		return program, arguments, true
	case (program == "cargo" || program == "npm" || program == "pnpm" || program == "just") &&
		firstArgumentIs(arguments, "test"):
		return program, arguments[1:], true
	default:
		return "", nil, false
	}
}

func validationArgumentsExecute(program string, arguments []string) bool {
	for _, argument := range arguments {
		name, _, _ := strings.Cut(argument, "=")
		switch {
		case name == "-h", name == "-V",
			strings.EqualFold(name, "--help"), strings.EqualFold(name, "--version"):
			return false
		}
	}

	switch program {
	case "go":
		return goTestArgumentsExecute(arguments)
	case "golangci-lint":
		return !optionEquals(arguments, "--issues-exit-code", "0")
	case "nix flake check":
		return !containsOption(arguments, "--no-build", "--dry-run")
	case "pytest":
		return !containsOption(arguments,
			"--collect-only", "--co", "--setup-plan", "--fixtures", "--fixtures-per-test", "--markers", "--trace-config",
		)
	case "cargo":
		return !containsOption(arguments, "--no-run", "--list")
	case "npm", "pnpm":
		return !containsOption(arguments,
			"--if-present", "--ignore-scripts", "--dry-run", "--list", "--listtests", "--list-tests",
			"--passwithnotests", "--pass-with-no-tests",
		)
	default:
		return true
	}
}

func goTestArgumentsExecute(arguments []string) bool {
	afterArgs := false
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "-args" && !afterArgs {
			afterArgs = true
			continue
		}
		if argument == "--" || !strings.HasPrefix(argument, "-") || argument == "-" {
			if afterArgs || argument == "--" {
				return false
			}
			continue
		}
		name, value, assigned := strings.Cut(argument, "=")
		name = strings.ToLower(name)
		if slices.Contains([]string{"-c", "-n", "-list", "-test.list", "-exec", "-toolexec"}, name) {
			return false
		}
		if slices.Contains(goTestBooleanOptions, name) {
			if assigned {
				if _, err := strconv.ParseBool(value); err != nil {
					return false
				}
			}
			continue
		}
		if !slices.Contains(goTestValueOptions, name) {
			return false
		}
		if !assigned {
			index++
			if index >= len(arguments) {
				return false
			}
			value = arguments[index]
		}
		if (name == "-count" || name == "-test.count") && !positiveInteger(value) {
			return false
		}
	}
	return true
}

var goTestBooleanOptions = []string{
	"-a", "-asan", "-benchmem", "-cover", "-failfast", "-fullpath", "-json", "-linkshared",
	"-modcacherw", "-msan", "-race", "-short", "-trimpath", "-v", "-work", "-x",
	"-test.benchmem", "-test.failfast", "-test.fullpath", "-test.paniconexit0", "-test.short", "-test.v",
}

var goTestValueOptions = []string{
	"-asmflags", "-bench", "-benchtime", "-blockprofile", "-blockprofilerate", "-buildmode", "-buildvcs",
	"-compiler", "-count", "-covermode", "-coverpkg", "-coverprofile", "-cpu", "-cpuprofile", "-fuzz",
	"-fuzzminimizetime", "-fuzztime", "-gccgoflags", "-gcflags", "-installsuffix", "-ldflags", "-memprofile",
	"-memprofilerate", "-mod", "-modfile", "-mutexprofile", "-mutexprofilefraction", "-o", "-outputdir",
	"-overlay", "-p", "-parallel", "-pgo", "-pkgdir", "-run", "-shuffle", "-skip", "-tags", "-timeout",
	"-trace", "-vet",
	"-test.bench", "-test.benchtime", "-test.blockprofile", "-test.blockprofilerate", "-test.count",
	"-test.coverprofile", "-test.cpu", "-test.cpuprofile", "-test.fuzz", "-test.fuzzcachedir",
	"-test.fuzzminimizetime", "-test.fuzztime", "-test.memprofile", "-test.memprofilerate", "-test.mutexprofile",
	"-test.mutexprofilefraction", "-test.outputdir", "-test.parallel", "-test.run", "-test.shuffle", "-test.skip",
	"-test.testlogfile", "-test.timeout", "-test.trace",
}

func positiveInteger(value string) bool {
	parsed, err := strconv.ParseUint(value, 10, 64)
	return err == nil && parsed > 0
}

func goTestHasSelector(arguments []string) bool {
	return containsOption(arguments,
		"-run", "-test.run", "-skip", "-test.skip",
		"-bench", "-test.bench", "-fuzz", "-test.fuzz",
	)
}

func goTestResultProvesExecution(value string, requiresDetailedProof bool) bool {
	packageSucceeded := false
	for line := range strings.SplitSeq(value, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "ok" &&
			!strings.Contains(lower, "[no tests to run]") && !strings.Contains(lower, "[no test files]") {
			packageSucceeded = true
		}
		if strings.HasPrefix(line, "--- PASS:") || benchmarkResultLine(fields) || strings.HasPrefix(lower, "fuzz: elapsed:") {
			return true
		}
		var event struct {
			Action string `json:"Action"`
			Test   string `json:"Test"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && event.Action == "pass" && event.Test != "" {
			return true
		}
	}
	return packageSucceeded && !requiresDetailedProof
}

func benchmarkResultLine(fields []string) bool {
	if len(fields) < 3 || !strings.HasPrefix(fields[0], "Benchmark") {
		return false
	}
	iterations, err := strconv.ParseUint(fields[1], 10, 64)
	return err == nil && iterations > 0
}

func containsOption(arguments []string, options ...string) bool {
	for _, argument := range arguments {
		name, _, _ := strings.Cut(strings.ToLower(argument), "=")
		if slices.Contains(options, name) {
			return true
		}
	}
	return false
}

func optionEquals(arguments []string, option, expected string) bool {
	value, found := optionValue(arguments, option)
	return found && strings.EqualFold(strings.TrimSpace(value), expected)
}

func optionValue(arguments []string, options ...string) (string, bool) {
	for index, argument := range arguments {
		name, value, assigned := strings.Cut(argument, "=")
		for _, option := range options {
			if !strings.EqualFold(name, option) {
				continue
			}
			if assigned {
				return value, true
			}
			if index+1 < len(arguments) {
				return arguments[index+1], true
			}
			return "", true
		}
	}
	return "", false
}

func firstArgumentIs(arguments []string, expected string) bool {
	return len(arguments) > 0 && arguments[0] == expected
}

func unwrapValidationCommand(words []string) (validationInvocation, bool) {
	invocation := validationInvocation{}
	environment := map[string]string{}
	for len(words) > 0 {
		program, ok := trustedExecutableName(words[0])
		if !ok {
			return validationInvocation{}, false
		}
		switch program {
		case "command":
			words = words[1:]
			if len(words) > 0 && words[0] == "--" {
				words = words[1:]
			}
			if len(words) > 0 && strings.HasPrefix(words[0], "-") {
				return validationInvocation{}, false
			}
		case "env":
			var clear bool
			words, clear, ok = unwrapValidationEnvironment(words[1:], environment)
			if !ok {
				return validationInvocation{}, false
			}
			if clear {
				invocation.clearEnvironment = true
			}
		case "nix":
			if len(words) >= 3 && words[1] == "flake" && words[2] == "check" {
				invocation.arguments = words
				invocation.environment = sortedEnvironment(environment)
				return invocation, true
			}
			return validationInvocation{}, false
		default:
			invocation.arguments = words
			invocation.environment = sortedEnvironment(environment)
			return invocation, true
		}
	}
	return validationInvocation{}, false
}

func unwrapValidationEnvironment(words []string, environment map[string]string) ([]string, bool, bool) {
	cleared := false
	for len(words) > 0 {
		switch {
		case words[0] == "--":
			return words[1:], cleared, true
		case words[0] == "-i" || words[0] == "--ignore-environment":
			clear(environment)
			cleared = true
			words = words[1:]
		case isSafeEnvironmentAssignment(words[0]):
			name, value, _ := environmentAssignment(words[0])
			environment[name] = value
			words = words[1:]
		case isEnvironmentAssignment(words[0]) || strings.HasPrefix(words[0], "-"):
			return nil, false, false
		default:
			return words, cleared, true
		}
	}
	return nil, false, false
}

func sortedEnvironment(environment map[string]string) []string {
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+environment[name])
	}
	return result
}

func canonicalValidationCommand(invocation validationInvocation) string {
	words := make([]string, 0, len(invocation.arguments)+len(invocation.environment)+2)
	if invocation.clearEnvironment || len(invocation.environment) > 0 {
		words = append(words, "env")
		if invocation.clearEnvironment {
			words = append(words, "-i")
		}
		words = append(words, invocation.environment...)
	}
	words = append(words, invocation.arguments...)
	for index, word := range words {
		words[index] = quoteShellWord(word)
	}
	return strings.Join(words, " ")
}

func quoteShellWord(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\r\n'\"\\$`;&|<>*?[]{}()!") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func unwrapEnvironment(words []string) ([]string, bool) {
	for len(words) > 0 {
		switch {
		case words[0] == "--":
			return words[1:], true
		case words[0] == "-i" || words[0] == "--ignore-environment":
			words = words[1:]
		case isSafeEnvironmentAssignment(words[0]):
			words = words[1:]
		case isEnvironmentAssignment(words[0]):
			return nil, false
		case strings.HasPrefix(words[0], "-"):
			return nil, false
		default:
			return words, true
		}
	}
	return nil, false
}

func isEnvironmentAssignment(value string) bool {
	_, ok := environmentAssignmentName(value)
	return ok
}

func isSafeEnvironmentAssignment(value string) bool {
	name, assignedValue, ok := environmentAssignment(value)
	if !ok {
		return false
	}
	switch name {
	case "CGO_ENABLED":
		return assignedValue == "0" || assignedValue == "1"
	case "CI":
		return assignedValue == "0" || assignedValue == "1" ||
			strings.EqualFold(assignedValue, "true") || strings.EqualFold(assignedValue, "false")
	case "LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "NO_COLOR", "TERM", "TZ":
		return len(assignedValue) <= 128
	default:
		return false
	}
}

func environmentAssignmentName(value string) (string, bool) {
	name, _, ok := environmentAssignment(value)
	return name, ok
}

func environmentAssignment(value string) (string, string, bool) {
	name, assignedValue, found := strings.Cut(value, "=")
	if !found || name == "" {
		return "", "", false
	}
	for index, character := range name {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || character == '_' ||
			(index > 0 && character >= '0' && character <= '9') {
			continue
		}
		return "", "", false
	}
	return name, assignedValue, true
}

func unwrapEvidenceCommand(words []string) ([]string, bool) {
	for len(words) > 0 {
		program, ok := trustedExecutableName(words[0])
		if !ok {
			return nil, false
		}
		switch program {
		case "command":
			words = words[1:]
			if len(words) > 0 && words[0] == "--" {
				words = words[1:]
			}
			if len(words) > 0 && strings.HasPrefix(words[0], "-") {
				return nil, false
			}
		case "env":
			words, ok = unwrapEnvironment(words[1:])
			if !ok {
				return nil, false
			}
		case "nix":
			if len(words) >= 3 && words[1] == "flake" && words[2] == "check" {
				return words, true
			}
			return nil, false
		case "bash", "sh", "sudo", "zsh":
			return nil, false
		default:
			return words, true
		}
	}
	return nil, false
}

func trustedExecutableName(value string) (string, bool) {
	// Provider transcripts preserve the command token, not the resolved binary.
	// A bare allowlisted name is attribution evidence, not executable provenance.
	// Promotion safety remains anchored in the exact external baseline/candidate
	// evaluator rather than this session-level heuristic.
	if value == "" || value == "." || value == ".." || filepath.IsAbs(value) || strings.ContainsAny(value, `/\\`) {
		return "", false
	}
	return value, true
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
