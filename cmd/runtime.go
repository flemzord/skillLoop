package cmd

import (
	"context"
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
