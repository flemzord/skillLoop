package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	"github.com/flemzord/skillloop/internal/domain"
)

type Installer struct {
	HomeDir    string
	Executable string
}

func (installer Installer) Install(source domain.Source) (bool, error) {
	path, err := installer.path(source)
	if err != nil {
		return false, err
	}
	document, err := readDocument(path)
	if err != nil {
		return false, err
	}
	changed := false
	if source == domain.SourceCodex {
		if _, exists := document["description"]; !exists {
			description, _ := json.Marshal("SkillLoop local-first capture hooks")
			document["description"] = description
			changed = true
		}
	}
	for _, event := range []Event{EventStop, EventSessionEnd} {
		handler, handlerErr := installer.handler(source, event)
		if handlerErr != nil {
			return false, handlerErr
		}
		eventChanged, updateErr := addHandler(document, upstreamName(event), handler)
		if updateErr != nil {
			return false, fmt.Errorf("install %s %s hook: %w", source, event, updateErr)
		}
		changed = changed || eventChanged
	}
	if !changed {
		return false, nil
	}
	if err := writeDocument(path, document); err != nil {
		return false, err
	}
	return true, nil
}

func (installer Installer) Uninstall(source domain.Source) (bool, error) {
	path, err := installer.path(source)
	if err != nil {
		return false, err
	}
	document, err := readDocument(path)
	if err != nil {
		return false, err
	}
	changed := false
	for _, event := range []Event{EventStop, EventSessionEnd} {
		handler, handlerErr := installer.handler(source, event)
		if handlerErr != nil {
			return false, handlerErr
		}
		eventChanged, updateErr := removeHandler(document, upstreamName(event), handler)
		if updateErr != nil {
			return false, fmt.Errorf("uninstall %s %s hook: %w", source, event, updateErr)
		}
		changed = changed || eventChanged
	}
	if !changed {
		return false, nil
	}
	if err := writeDocument(path, document); err != nil {
		return false, err
	}
	return true, nil
}

func (installer Installer) Path(source domain.Source) (string, error) {
	return installer.path(source)
}

func (installer Installer) path(source domain.Source) (string, error) {
	if installer.HomeDir == "" {
		return "", errors.New("home directory is required")
	}
	switch source {
	case domain.SourceCodex:
		return filepath.Join(installer.HomeDir, ".codex", "hooks.json"), nil
	case domain.SourceClaude:
		return filepath.Join(installer.HomeDir, ".claude", "settings.json"), nil
	default:
		return "", fmt.Errorf("unsupported hook provider %q", source)
	}
}

func (installer Installer) handler(source domain.Source, event Event) (json.RawMessage, error) {
	if installer.Executable == "" {
		return nil, errors.New("executable path is required")
	}
	if _, err := upstreamEventName(event); err != nil {
		return nil, err
	}
	var handler map[string]any
	switch source {
	case domain.SourceCodex:
		command := shellQuote(installer.Executable) + " hook --provider " + string(source) + " --event " + string(event)
		handler = map[string]any{
			"type":    "command",
			"command": command,
			"timeout": 1,
		}
		if runtime.GOOS == "windows" {
			handler["commandWindows"] = powershellQuote(installer.Executable) + " hook --provider " + string(source) + " --event " + string(event)
		}
	case domain.SourceClaude:
		handler = map[string]any{
			"type":    "command",
			"command": installer.Executable,
			"args": []string{
				"hook",
				"--provider",
				string(source),
				"--event",
				string(event),
			},
			"timeout": 1,
		}
	default:
		return nil, fmt.Errorf("unsupported hook provider %q", source)
	}
	contents, err := json.Marshal(handler)
	if err != nil {
		return nil, fmt.Errorf("encode hook handler: %w", err)
	}
	return contents, nil
}

func addHandler(document map[string]json.RawMessage, eventName string, handler json.RawMessage) (bool, error) {
	hookEvents, err := hookEvents(document)
	if err != nil {
		return false, err
	}
	groups, err := rawArray(hookEvents[eventName])
	if err != nil {
		return false, fmt.Errorf("decode %s groups: %w", eventName, err)
	}
	for _, rawGroup := range groups {
		group := map[string]json.RawMessage{}
		if err := json.Unmarshal(rawGroup, &group); err != nil {
			return false, fmt.Errorf("decode %s group: %w", eventName, err)
		}
		handlers, err := rawArray(group["hooks"])
		if err != nil {
			return false, fmt.Errorf("decode %s handlers: %w", eventName, err)
		}
		for _, existing := range handlers {
			if jsonEqual(existing, handler) {
				return false, nil
			}
		}
	}
	group, err := json.Marshal(map[string][]json.RawMessage{"hooks": {handler}})
	if err != nil {
		return false, fmt.Errorf("encode %s group: %w", eventName, err)
	}
	groups = append(groups, group)
	encodedGroups, err := json.Marshal(groups)
	if err != nil {
		return false, fmt.Errorf("encode %s groups: %w", eventName, err)
	}
	hookEvents[eventName] = encodedGroups
	return true, setHookEvents(document, hookEvents)
}

func removeHandler(document map[string]json.RawMessage, eventName string, handler json.RawMessage) (bool, error) {
	hookEvents, err := hookEvents(document)
	if err != nil {
		return false, err
	}
	rawGroups, exists := hookEvents[eventName]
	if !exists {
		return false, nil
	}
	groups, err := rawArray(rawGroups)
	if err != nil {
		return false, fmt.Errorf("decode %s groups: %w", eventName, err)
	}
	changed := false
	keptGroups := make([]json.RawMessage, 0, len(groups))
	for _, rawGroup := range groups {
		group := map[string]json.RawMessage{}
		if err := json.Unmarshal(rawGroup, &group); err != nil {
			return false, fmt.Errorf("decode %s group: %w", eventName, err)
		}
		handlers, err := rawArray(group["hooks"])
		if err != nil {
			return false, fmt.Errorf("decode %s handlers: %w", eventName, err)
		}
		keptHandlers := make([]json.RawMessage, 0, len(handlers))
		for _, existing := range handlers {
			if jsonEqual(existing, handler) {
				changed = true
				continue
			}
			keptHandlers = append(keptHandlers, existing)
		}
		if len(keptHandlers) == 0 && len(group) == 1 && len(keptHandlers) != len(handlers) {
			continue
		}
		if len(keptHandlers) != len(handlers) {
			encodedHandlers, marshalErr := json.Marshal(keptHandlers)
			if marshalErr != nil {
				return false, fmt.Errorf("encode %s handlers: %w", eventName, marshalErr)
			}
			group["hooks"] = encodedHandlers
			encodedGroup, marshalErr := json.Marshal(group)
			if marshalErr != nil {
				return false, fmt.Errorf("encode %s group: %w", eventName, marshalErr)
			}
			keptGroups = append(keptGroups, encodedGroup)
			continue
		}
		keptGroups = append(keptGroups, rawGroup)
	}
	if !changed {
		return false, nil
	}
	if len(keptGroups) == 0 {
		delete(hookEvents, eventName)
	} else {
		encodedGroups, marshalErr := json.Marshal(keptGroups)
		if marshalErr != nil {
			return false, fmt.Errorf("encode %s groups: %w", eventName, marshalErr)
		}
		hookEvents[eventName] = encodedGroups
	}
	return true, setHookEvents(document, hookEvents)
}

func hookEvents(document map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	hookEvents := map[string]json.RawMessage{}
	rawHooks, exists := document["hooks"]
	if !exists || len(bytes.TrimSpace(rawHooks)) == 0 || bytes.Equal(bytes.TrimSpace(rawHooks), []byte("null")) {
		return hookEvents, nil
	}
	if err := json.Unmarshal(rawHooks, &hookEvents); err != nil {
		return nil, fmt.Errorf("decode hooks: %w", err)
	}
	return hookEvents, nil
}

func setHookEvents(document map[string]json.RawMessage, hookEvents map[string]json.RawMessage) error {
	contents, err := json.Marshal(hookEvents)
	if err != nil {
		return fmt.Errorf("encode hooks: %w", err)
	}
	document["hooks"] = contents
	return nil
}

func rawArray(contents json.RawMessage) ([]json.RawMessage, error) {
	if len(bytes.TrimSpace(contents)) == 0 || bytes.Equal(bytes.TrimSpace(contents), []byte("null")) {
		return []json.RawMessage{}, nil
	}
	values := []json.RawMessage{}
	if err := json.Unmarshal(contents, &values); err != nil {
		return nil, err
	}
	return values, nil
}

func jsonEqual(left, right json.RawMessage) bool {
	var leftValue any
	var rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func readDocument(path string) (map[string]json.RawMessage, error) {
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read hooks config %s: %w", path, err)
	}
	if len(bytes.TrimSpace(contents)) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	document := map[string]json.RawMessage{}
	if err := json.Unmarshal(contents, &document); err != nil {
		return nil, fmt.Errorf("decode hooks config %s: %w", path, err)
	}
	return document, nil
}

func writeDocument(path string, document map[string]json.RawMessage) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create hooks config directory %s: %w", parent, err)
	}
	contents, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("encode hooks config %s: %w", path, err)
	}
	contents = append(contents, '\n')
	temporary, err := os.CreateTemp(parent, ".skillloop-hooks-*.tmp")
	if err != nil {
		return fmt.Errorf("create hooks config temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("set hooks config permissions: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		cleanup()
		return fmt.Errorf("write hooks config: %w", err)
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close hooks config: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		cleanup()
		return fmt.Errorf("publish hooks config %s: %w", path, err)
	}
	return nil
}

func upstreamName(event Event) string {
	name, _ := upstreamEventName(event)
	return name
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func powershellQuote(value string) string {
	return "& '" + strings.ReplaceAll(value, "'", "''") + "'"
}
