package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/flemzord/skillloop/internal/config"
	"github.com/flemzord/skillloop/internal/store"
)

type runtimeState struct {
	config config.Config
	store  *store.Store
}

func openRuntime(ctx context.Context, configPath string) (*runtimeState, error) {
	settings, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	database, err := store.Open(ctx, filepath.Join(settings.DataDir, "skillloop.db"))
	if err != nil {
		return nil, fmt.Errorf("open SkillLoop state: %w", err)
	}
	return &runtimeState{config: settings, store: database}, nil
}

func (state *runtimeState) close() {
	if state != nil {
		_ = state.store.Close()
	}
}

func (state *runtimeState) reloadConfig(configPath string) (config.Config, error) {
	settings, err := config.Load(configPath)
	if err != nil {
		return config.Config{}, err
	}
	openedDataDir, err := filepath.Abs(state.config.DataDir)
	if err != nil {
		return config.Config{}, fmt.Errorf("resolve open data directory: %w", err)
	}
	reloadedDataDir, err := filepath.Abs(settings.DataDir)
	if err != nil {
		return config.Config{}, fmt.Errorf("resolve reloaded data directory: %w", err)
	}
	if filepath.Clean(openedDataDir) != filepath.Clean(reloadedDataDir) {
		return config.Config{}, errors.New("data_dir changed while the process was running; restart SkillLoop to use the new state directory")
	}
	state.config = settings
	return settings, nil
}
