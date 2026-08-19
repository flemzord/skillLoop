package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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
	if config.Retention.TranscriptLocators <= 0 || config.Retention.FailedSpool <= 0 ||
		config.Retention.CompletedJobs <= 0 || config.Retention.FailedJobs <= 0 {
		t.Fatalf("expected finite safe retention defaults, got %#v", config.Retention)
	}
}

func TestRetentionAllowsExplicitIndefiniteDurationsAndRejectsNegativeValues(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	settings, err := Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	settings.Retention = Retention{}
	if err := settings.Validate(); err != nil {
		t.Fatalf("zero retention should mean indefinite retention: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := WriteInitial(path, settings); err != nil {
		t.Fatalf("write config: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if loaded.Retention != (Retention{}) {
		t.Fatalf("explicit zero retention was not preserved: %#v", loaded.Retention)
	}

	settings.Retention.FailedJobs = -time.Second
	if err := settings.Validate(); err == nil {
		t.Fatal("expected negative retention validation error")
	}
}

func TestLoadAppliesRetentionDefaultsWhenV1SectionIsAbsent(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := []byte("version: 1\nmode: propose\ndata_dir: /tmp/skillloop\npoll_interval: 5s\naggregation:\n  minimum_sessions: 3\nevaluation:\n  minimum_improvement: 0.1\n  allow_autopilot: false\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load legacy v1 config: %v", err)
	}
	if loaded.Retention != defaultRetention() {
		t.Fatalf("retention defaults = %#v, want %#v", loaded.Retention, defaultRetention())
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
