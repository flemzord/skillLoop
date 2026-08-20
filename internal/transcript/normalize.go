package transcript

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/flemzord/skillloop/internal/domain"
)

const (
	maximumLineSize              = 8 << 20
	defaultMaximumTranscriptSize = 64 << 20
	defaultMaximumRecords        = 100_000
	defaultMaximumMessages       = 20_000
)

var failedExitPattern = regexp.MustCompile(`(?i)(?:exit(?:ed)?(?: with)? (?:status|code)|exit_code["']?\s*:)\s*[1-9][0-9]*`)

// Limits bounds the aggregate work retained while parsing one transcript. Zero
// values select the safe defaults; they never disable a limit.
type Limits struct {
	MaximumBytes    int64
	MaximumRecords  int
	MaximumMessages int
}

// Normalizer reads transcripts only from provider-owned roots. AllowedRoots is
// an injection point for tests and installations with non-standard layouts.
// When a provider has no injected roots, its standard local root is used.
type Normalizer struct {
	AllowedRoots map[domain.Source][]string
	Limits       Limits
}

func (normalizer Normalizer) Normalize(ctx context.Context, event domain.HookEvent) (domain.Session, error) {
	if !event.Source.Valid() {
		return domain.Session{}, fmt.Errorf("unsupported source %q", event.Source)
	}
	if event.TranscriptPath == "" {
		return domain.Session{}, errors.New("transcript path is empty")
	}
	limits, err := normalizer.effectiveLimits()
	if err != nil {
		return domain.Session{}, err
	}
	roots, err := normalizer.roots(event.Source)
	if err != nil {
		return domain.Session{}, err
	}
	file, canonicalPath, err := openTranscript(event.TranscriptPath, roots)
	if err != nil {
		return domain.Session{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return domain.Session{}, fmt.Errorf("inspect transcript size: %w", err)
	}
	if info.Size() > limits.MaximumBytes {
		return domain.Session{}, fmt.Errorf("transcript exceeds maximum size of %d bytes", limits.MaximumBytes)
	}

	messages, err := normalizeReader(ctx, event.Source, file, limits)
	if err != nil {
		return domain.Session{}, err
	}
	return domain.Session{
		Reference:      string(event.Source) + ":" + event.SessionID,
		Source:         event.Source,
		ExternalID:     event.SessionID,
		TurnID:         event.TurnID,
		WorkingDir:     event.WorkingDir,
		TranscriptPath: canonicalPath,
		Outcome:        sessionOutcome(messages),
		Messages:       messages,
	}, nil
}

func (normalizer Normalizer) effectiveLimits() (Limits, error) {
	limits := normalizer.Limits
	if limits.MaximumBytes < 0 || limits.MaximumRecords < 0 || limits.MaximumMessages < 0 {
		return Limits{}, errors.New("transcript limits cannot be negative")
	}
	if limits.MaximumBytes == 0 {
		limits.MaximumBytes = defaultMaximumTranscriptSize
	}
	if limits.MaximumRecords == 0 {
		limits.MaximumRecords = defaultMaximumRecords
	}
	if limits.MaximumMessages == 0 {
		limits.MaximumMessages = defaultMaximumMessages
	}
	return limits, nil
}

func (normalizer Normalizer) roots(source domain.Source) ([]string, error) {
	if roots := normalizer.AllowedRoots[source]; len(roots) > 0 {
		return roots, nil
	}
	switch source {
	case domain.SourceCodex:
		root := os.Getenv("CODEX_HOME")
		if root == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("resolve transcript root: %w", err)
			}
			root = filepath.Join(home, ".codex")
		}
		return []string{
			filepath.Join(root, "sessions"),
			filepath.Join(root, "archived_sessions"),
		}, nil
	case domain.SourceClaude:
		root := os.Getenv("CLAUDE_CONFIG_DIR")
		if root == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("resolve transcript root: %w", err)
			}
			root = filepath.Join(home, ".claude")
		}
		return []string{filepath.Join(root, "projects")}, nil
	default:
		return nil, fmt.Errorf("unsupported source %q", source)
	}
}

func openTranscript(path string, roots []string) (*os.File, string, error) {
	if !filepath.IsAbs(path) {
		return nil, "", errors.New("transcript path must be absolute")
	}
	expected, err := os.Lstat(path)
	if err != nil {
		return nil, "", fmt.Errorf("inspect transcript: %w", err)
	}
	if expected.Mode()&os.ModeSymlink != 0 {
		return nil, "", errors.New("transcript path must not be a symlink")
	}
	if !expected.Mode().IsRegular() {
		return nil, "", fmt.Errorf("transcript path is not a regular file: %s", expected.Mode().Type())
	}
	canonicalPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, "", fmt.Errorf("resolve transcript path: %w", err)
	}
	canonicalPath, err = filepath.Abs(canonicalPath)
	if err != nil {
		return nil, "", fmt.Errorf("resolve absolute transcript path: %w", err)
	}

	for _, root := range roots {
		canonicalRoot, relative, ok := containedPath(canonicalPath, root)
		if !ok {
			continue
		}
		file, openErr := openBelowRoot(canonicalRoot, relative, expected)
		if openErr != nil {
			return nil, "", openErr
		}
		return file, canonicalPath, nil
	}
	return nil, "", errors.New("transcript path is outside the allowed provider roots")
}

func containedPath(canonicalPath, root string) (string, string, bool) {
	if root == "" {
		return "", "", false
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", "", false
	}
	canonicalRoot, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return "", "", false
	}
	info, err := os.Stat(canonicalRoot)
	if err != nil || !info.IsDir() {
		return "", "", false
	}
	relative, err := filepath.Rel(canonicalRoot, canonicalPath)
	if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", false
	}
	return canonicalRoot, relative, true
}

// openBelowRoot walks from an already-authorized directory descriptor. Using
// O_NOFOLLOW for every component prevents a path swap from escaping the root;
// O_NONBLOCK on the final component ensures a special file can never stall the
// daemon before its type is verified.
func openBelowRoot(root, relative string, expected os.FileInfo) (*os.File, error) {
	current, err := unix.Open(root, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open transcript root: %w", err)
	}
	components := strings.Split(relative, string(filepath.Separator))
	for index, component := range components {
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
		if index < len(components)-1 {
			flags |= unix.O_DIRECTORY
		} else {
			flags |= unix.O_NONBLOCK
		}
		next, openErr := unix.Openat(current, component, flags, 0)
		_ = unix.Close(current)
		if openErr != nil {
			return nil, fmt.Errorf("open transcript safely: %w", openErr)
		}
		current = next
	}
	file := os.NewFile(uintptr(current), relative)
	if file == nil {
		_ = unix.Close(current)
		return nil, errors.New("open transcript safely: invalid file descriptor")
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("inspect opened transcript: %w", err)
	}
	if !opened.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("opened transcript is not a regular file: %s", opened.Mode().Type())
	}
	if !os.SameFile(expected, opened) {
		_ = file.Close()
		return nil, errors.New("transcript changed while it was being opened")
	}
	return file, nil
}

func sessionOutcome(messages []domain.Message) domain.SessionOutcome {
	knownCalls := make(map[string]struct{})
	outcome := domain.SessionOutcomeUnknown
	for _, message := range messages {
		if message.ToolCallID == "" {
			continue
		}
		if !message.ToolResult {
			knownCalls[message.ToolCallID] = struct{}{}
			continue
		}
		if _, correlated := knownCalls[message.ToolCallID]; !correlated {
			continue
		}
		if message.Failed {
			outcome = domain.SessionOutcomeFailed
		} else {
			outcome = domain.SessionOutcomeSucceeded
		}
	}
	return outcome
}

func normalizeReader(ctx context.Context, source domain.Source, reader io.Reader, limits Limits) ([]domain.Message, error) {
	limited := &io.LimitedReader{R: reader, N: limits.MaximumBytes}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64*1024), maximumLineSize)
	toolNames := map[string]string{}
	messages := make([]domain.Message, 0, 32)
	records := 0
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		records++
		if records > limits.MaximumRecords {
			return nil, fmt.Errorf("transcript exceeds maximum record count of %d", limits.MaximumRecords)
		}
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var value map[string]any
		if err := json.Unmarshal(line, &value); err != nil {
			// Transcripts are append-only. A process can observe an incomplete final
			// line, so malformed records are ignored rather than blocking ingestion.
			continue
		}
		var parsed []domain.Message
		switch source {
		case domain.SourceCodex:
			parsed = parseCodex(value, toolNames)
		case domain.SourceClaude:
			parsed = parseClaude(value, toolNames)
		}
		if len(parsed) > limits.MaximumMessages-len(messages) {
			return nil, fmt.Errorf("transcript exceeds maximum retained message count of %d", limits.MaximumMessages)
		}
		messages = append(messages, parsed...)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan transcript: %w", err)
	}
	if limited.N == 0 {
		var extra [1]byte
		count, err := reader.Read(extra[:])
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("check transcript size boundary: %w", err)
		}
		if count != 0 {
			return nil, fmt.Errorf("transcript exceeds maximum size of %d bytes", limits.MaximumBytes)
		}
	}
	return compact(messages), nil
}

func parseCodex(record map[string]any, toolNames map[string]string) []domain.Message {
	typeName, _ := record["type"].(string)
	payload := asMap(record["payload"])
	if payload == nil {
		payload = record
	}
	payloadType, _ := payload["type"].(string)

	switch {
	case typeName == "response_item" && payloadType == "message":
		return textMessages(stringValue(payload["role"]), payload["content"])
	case typeName == "event_msg" && payloadType == "agent_message":
		return oneText("assistant", stringValue(payload["message"]))
	case typeName == "event_msg" && payloadType == "user_message":
		return oneText("user", stringValue(payload["message"]))
	case typeName == "response_item" && (payloadType == "function_call" || payloadType == "custom_tool_call"):
		name := stringValue(payload["name"])
		callID := firstString(payload, "call_id", "id")
		if callID != "" {
			toolNames[callID] = name
		}
		return []domain.Message{{Role: "tool", ToolName: name, ToolCallID: callID, Text: firstString(payload, "arguments", "input")}}
	case typeName == "response_item" && (payloadType == "function_call_output" || payloadType == "custom_tool_call_output"):
		output := firstString(payload, "output", "content")
		callID := firstString(payload, "call_id", "id")
		name := toolNames[callID]
		return []domain.Message{{Role: "tool", ToolName: name, ToolCallID: callID, ToolResult: true, Text: output, Failed: looksFailed(output)}}
	default:
		return nil
	}
}

func parseClaude(record map[string]any, toolNames map[string]string) []domain.Message {
	message := asMap(record["message"])
	if message == nil {
		message = record
	}
	role := firstString(message, "role")
	if role == "" {
		role = firstString(record, "type")
	}
	contents := asSlice(message["content"])
	if contents == nil {
		return oneText(role, stringValue(message["content"]))
	}
	parsed := make([]domain.Message, 0, len(contents))
	for _, item := range contents {
		content := asMap(item)
		if content == nil {
			if text := stringValue(item); text != "" {
				parsed = append(parsed, domain.Message{Role: role, Text: text})
			}
			continue
		}
		switch stringValue(content["type"]) {
		case "text", "input_text", "output_text":
			parsed = append(parsed, domain.Message{Role: role, Text: stringValue(content["text"])})
		case "tool_use":
			name := stringValue(content["name"])
			id := stringValue(content["id"])
			if id != "" {
				toolNames[id] = name
			}
			parsed = append(parsed, domain.Message{Role: "tool", ToolName: name, ToolCallID: id, Text: jsonText(content["input"])})
		case "tool_result":
			text := contentText(content["content"])
			failed, _ := content["is_error"].(bool)
			callID := stringValue(content["tool_use_id"])
			name := toolNames[callID]
			parsed = append(parsed, domain.Message{Role: "tool", ToolName: name, ToolCallID: callID, ToolResult: true, Text: text, Failed: failed || looksFailed(text)})
		}
	}
	return parsed
}

func textMessages(role string, value any) []domain.Message {
	contents := asSlice(value)
	if contents == nil {
		return oneText(role, stringValue(value))
	}
	messages := make([]domain.Message, 0, len(contents))
	for _, item := range contents {
		content := asMap(item)
		if content == nil {
			messages = append(messages, domain.Message{Role: role, Text: stringValue(item)})
			continue
		}
		if text := firstString(content, "text", "content"); text != "" {
			messages = append(messages, domain.Message{Role: role, Text: text})
		}
	}
	return messages
}

func oneText(role, text string) []domain.Message {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return []domain.Message{{Role: role, Text: text}}
}

func compact(messages []domain.Message) []domain.Message {
	result := make([]domain.Message, 0, len(messages))
	for _, message := range messages {
		message.Text = strings.TrimSpace(message.Text)
		if message.Text == "" && message.ToolName == "" {
			continue
		}
		if len(result) > 0 {
			previous := result[len(result)-1]
			if previous.Role == message.Role && previous.Text == message.Text && previous.ToolName == message.ToolName &&
				previous.ToolCallID == message.ToolCallID && previous.ToolResult == message.ToolResult && previous.Failed == message.Failed {
				continue
			}
		}
		result = append(result, message)
	}
	return result
}

func contentText(value any) string {
	if text := stringValue(value); text != "" {
		return text
	}
	var parts []string
	for _, item := range asSlice(value) {
		if content := asMap(item); content != nil {
			if text := firstString(content, "text", "content"); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func looksFailed(text string) bool {
	lower := strings.ToLower(text)
	return failedExitPattern.MatchString(text) ||
		strings.Contains(lower, "iserror\":true") ||
		strings.Contains(lower, "command failed")
}

func asMap(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func asSlice(value any) []any {
	result, _ := value.([]any)
	return result
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func firstString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text := stringValue(value[key]); text != "" {
			return text
		}
	}
	return ""
}

func jsonText(value any) string {
	if value == nil {
		return ""
	}
	contents, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(contents)
}
