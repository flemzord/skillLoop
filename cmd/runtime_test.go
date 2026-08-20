package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/flemzord/skillloop/internal/config"
	"github.com/flemzord/skillloop/internal/domain"
)

func TestRuntimeReloadConfigAppliesPolicyAndRejectsDataDirectorySwitch(t *testing.T) {
	settings, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	settings.DataDir = t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := config.WriteInitial(configPath, settings); err != nil {
		t.Fatalf("write config: %v", err)
	}
	state := runtimeState{config: settings}

	settings.Mode = domain.ModeObserve
	if _, err := config.Save(configPath, settings); err != nil {
		t.Fatalf("save observe mode: %v", err)
	}
	reloaded, err := state.reloadConfig(configPath)
	if err != nil || reloaded.Mode != domain.ModeObserve || state.config.Mode != domain.ModeObserve {
		t.Fatalf("reloaded=%#v state=%#v err=%v", reloaded, state.config, err)
	}

	settings.DataDir = t.TempDir()
	if _, err := config.Save(configPath, settings); err != nil {
		t.Fatalf("save changed data directory: %v", err)
	}
	if _, err := state.reloadConfig(configPath); err == nil || !strings.Contains(err.Error(), "data_dir changed") {
		t.Fatalf("reload error=%v, want restart requirement", err)
	}
}
