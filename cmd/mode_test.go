package cmd

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flemzord/skillloop/internal/config"
	"github.com/flemzord/skillloop/internal/domain"
)

func TestModeSetPersistsValidatedMode(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	settings, err := config.Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := config.WriteInitial(path, settings); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	output := bytes.NewBuffer(nil)
	command := NewRootCommand()
	command.SetOut(output)
	command.SetErr(bytes.NewBuffer(nil))
	command.SetArgs([]string{"--config", path, "mode", "set", "observe"})
	if err := command.Execute(); err != nil {
		t.Fatalf("set mode: %v", err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("load updated config: %v", err)
	}
	if loaded.Mode != domain.ModeObserve {
		t.Fatalf("mode = %q, want observe", loaded.Mode)
	}
	if !strings.Contains(output.String(), "Mode set to observe") {
		t.Fatalf("unexpected output: %q", output.String())
	}
}

func TestModeSetRefusesInvalidAutopilotConfiguration(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	settings, err := config.Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := config.WriteInitial(path, settings); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	command := NewRootCommand()
	command.SetOut(bytes.NewBuffer(nil))
	command.SetErr(bytes.NewBuffer(nil))
	command.SetArgs([]string{"--config", path, "mode", "set", "autopilot"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "autopilot requires") {
		t.Fatalf("expected autopilot guard, got %v", err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("load unchanged config: %v", err)
	}
	if loaded.Mode != domain.ModePropose {
		t.Fatalf("invalid update changed mode to %q", loaded.Mode)
	}
}
