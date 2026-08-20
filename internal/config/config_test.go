package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

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

func TestLoadAcceptsRegularConfiguration(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	settings, err := Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := WriteInitial(path, settings); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load regular config: %v", err)
	}
	if loaded.Mode != settings.Mode || loaded.DataDir != settings.DataDir {
		t.Fatalf("loaded config = %#v, want mode %q and data dir %q", loaded, settings.Mode, settings.DataDir)
	}
}

func TestLoadRejectsSymlink(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	settings, err := Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	directory := t.TempDir()
	target := filepath.Join(directory, "target.yaml")
	if _, err := WriteInitial(target, settings); err != nil {
		t.Fatalf("write target config: %v", err)
	}
	path := filepath.Join(directory, "config.yaml")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("create config symlink: %v", err)
	}

	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink error = %v, want non-regular file rejection", err)
	}
}

func TestLoadRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("create config FIFO: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := Load(path)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("FIFO error = %v, want non-regular file rejection", err)
		}
	case <-time.After(time.Second):
		t.Fatal("loading a FIFO blocked")
	}
}

func TestLoadRejectsDevice(t *testing.T) {
	if _, err := os.Stat("/dev/null"); err != nil {
		t.Skipf("device unavailable: %v", err)
	}
	if _, err := Load("/dev/null"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("device error = %v, want non-regular file rejection", err)
	}
}

func TestLoadRejectsOversizedConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := make([]byte, maxConfigFileBytes+1)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write oversized config: %v", err)
	}

	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized error = %v, want size-limit rejection", err)
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

func TestLoadRejectsNonFiniteMinimumImprovement(t *testing.T) {
	for _, value := range []string{".nan", ".inf"} {
		t.Run(value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			contents := "version: 1\nmode: propose\ndata_dir: /tmp/skillloop\npoll_interval: 5s\naggregation:\n  minimum_sessions: 3\nevaluation:\n  minimum_improvement: " + value + "\n  allow_autopilot: false\n"
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "must be finite") {
				t.Fatalf("load error=%v, want finite-value rejection", err)
			}
		})
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
