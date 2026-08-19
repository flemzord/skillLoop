package hooks

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/flemzord/skillloop/internal/domain"
)

type Event string

const (
	EventStop       Event = "stop"
	EventSessionEnd Event = "session-end"
)

func ParseSource(value string) (domain.Source, error) {
	source := domain.Source(value)
	if !source.Valid() {
		return "", fmt.Errorf("unsupported hook provider %q", value)
	}
	return source, nil
}

func ParseEvent(value string) (Event, error) {
	event := Event(value)
	if event != EventStop && event != EventSessionEnd {
		return "", fmt.Errorf("unsupported hook event %q", value)
	}
	return event, nil
}

type input struct {
	SessionID      string  `json:"session_id"`
	PromptID       string  `json:"prompt_id"`
	TranscriptPath *string `json:"transcript_path"`
	WorkingDir     string  `json:"cwd"`
	PermissionMode string  `json:"permission_mode"`
	HookEventName  string  `json:"hook_event_name"`
	Model          string  `json:"model"`
	Effort         struct {
		Level string `json:"level"`
	} `json:"effort"`
	TurnID          string            `json:"turn_id"`
	StopHookActive  *bool             `json:"stop_hook_active"`
	Reason          string            `json:"reason"`
	BackgroundTasks []json.RawMessage `json:"background_tasks"`
	SessionCrons    []json.RawMessage `json:"session_crons"`
}

func Normalize(source domain.Source, event Event, contents []byte) (domain.HookEvent, error) {
	if !source.Valid() {
		return domain.HookEvent{}, errors.New("invalid hook provider")
	}
	expectedName, err := upstreamEventName(event)
	if err != nil {
		return domain.HookEvent{}, err
	}
	payload := input{}
	if err := json.Unmarshal(contents, &payload); err != nil {
		return domain.HookEvent{}, fmt.Errorf("decode hook input: %w", err)
	}
	if payload.HookEventName != expectedName {
		return domain.HookEvent{}, fmt.Errorf("hook event mismatch: expected %q, got %q", expectedName, payload.HookEventName)
	}
	if payload.SessionID == "" {
		return domain.HookEvent{}, errors.New("hook input is missing session_id")
	}
	if payload.WorkingDir == "" {
		return domain.HookEvent{}, errors.New("hook input is missing cwd")
	}

	normalized := domain.HookEvent{
		SchemaVersion:  1,
		Source:         source,
		SessionID:      payload.SessionID,
		TurnID:         payload.TurnID,
		PromptID:       payload.PromptID,
		WorkingDir:     payload.WorkingDir,
		HookEventName:  string(event),
		PermissionMode: payload.PermissionMode,
		Model:          payload.Model,
		Effort:         payload.Effort.Level,
		StopHookActive: payload.StopHookActive,
		Reason:         payload.Reason,
	}
	if payload.TranscriptPath != nil {
		normalized.TranscriptPath = *payload.TranscriptPath
	}
	if payload.BackgroundTasks != nil {
		count := len(payload.BackgroundTasks)
		normalized.BackgroundTaskCount = &count
	}
	if payload.SessionCrons != nil {
		count := len(payload.SessionCrons)
		normalized.SessionCronCount = &count
	}
	return normalized, nil
}

func upstreamEventName(event Event) (string, error) {
	switch event {
	case EventStop:
		return "Stop", nil
	case EventSessionEnd:
		return "SessionEnd", nil
	default:
		return "", fmt.Errorf("unsupported hook event %q", event)
	}
}
