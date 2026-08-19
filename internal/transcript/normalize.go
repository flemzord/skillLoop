package transcript

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/flemzord/skillloop/internal/domain"
)

const maximumLineSize = 8 << 20

var failedExitPattern = regexp.MustCompile(`(?i)(?:exit(?:ed)?(?: with)? (?:status|code)|exit_code["']?\s*:)\s*[1-9][0-9]*`)

type Normalizer struct{}

func (Normalizer) Normalize(ctx context.Context, event domain.HookEvent) (domain.Session, error) {
	if !event.Source.Valid() {
		return domain.Session{}, fmt.Errorf("unsupported source %q", event.Source)
	}
	if event.TranscriptPath == "" {
		return domain.Session{}, errors.New("transcript path is empty")
	}
	file, err := os.Open(event.TranscriptPath)
	if err != nil {
		return domain.Session{}, fmt.Errorf("open transcript: %w", err)
	}
	defer func() { _ = file.Close() }()

	messages, err := normalizeReader(ctx, event.Source, file)
	if err != nil {
		return domain.Session{}, err
	}
	return domain.Session{
		Reference:      string(event.Source) + ":" + event.SessionID,
		Source:         event.Source,
		ExternalID:     event.SessionID,
		TurnID:         event.TurnID,
		WorkingDir:     event.WorkingDir,
		TranscriptPath: event.TranscriptPath,
		Messages:       messages,
	}, nil
}

func normalizeReader(ctx context.Context, source domain.Source, reader io.Reader) ([]domain.Message, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maximumLineSize)
	toolNames := map[string]string{}
	messages := make([]domain.Message, 0, 32)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
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
		messages = append(messages, parsed...)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan transcript: %w", err)
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
