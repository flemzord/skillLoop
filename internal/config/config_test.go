package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/flemzord/skillloop/internal/domain"
)

func TestDefaultConfigIsValid(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	config, err := Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if config.Mode != domain.ModePropose {
		t.Fatalf("expected propose mode, got %q", config.Mode)
	}
	if filepath.Base(config.DataDir) != "skillloop" {
		t.Fatalf("unexpected data directory: %s", config.DataDir)
	}
}

func TestAutopilotRequiresExternalEvaluation(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	config, err := Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	config.Mode = domain.ModeAutopilot
	config.Evaluation.AllowAutopilot = true
	if err := config.Validate(); err == nil {
		t.Fatal("expected autopilot validation error")
	}
}

func TestWriteInitialDoesNotOverwrite(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	config, err := Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := WriteInitial(path, config); err != nil {
		t.Fatalf("write initial: %v", err)
	}
	if _, err := WriteInitial(path, config); err == nil {
		t.Fatal("expected existing config error")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected mode 0600, got %o", info.Mode().Perm())
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if loaded.Mode != domain.ModePropose {
		t.Fatalf("unexpected loaded mode: %s", loaded.Mode)
	}
}

func TestSaveAtomicallyUpdatesExistingConfig(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	settings, err := Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := WriteInitial(path, settings); err != nil {
		t.Fatalf("write initial: %v", err)
	}
	settings.Mode = domain.ModeObserve
	if _, err := Save(path, settings); err != nil {
		t.Fatalf("save config: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if loaded.Mode != domain.ModeObserve {
		t.Fatalf("expected observe mode, got %q", loaded.Mode)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected mode 0600, got %o", info.Mode().Perm())
	}
}
