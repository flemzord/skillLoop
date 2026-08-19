package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/flemzord/skillloop/internal/domain"
)

const fileName = "config.yaml"

type Config struct {
	Version       int                 `yaml:"version"`
	Mode          domain.AutonomyMode `yaml:"mode"`
	DataDir       string              `yaml:"data_dir"`
	PollInterval  time.Duration       `yaml:"poll_interval"`
	Aggregation   Aggregation         `yaml:"aggregation"`
	Evaluation    Evaluation          `yaml:"evaluation"`
	ExcludedPaths []string            `yaml:"excluded_paths,omitempty"`
}

type Aggregation struct {
	MinimumSessions int `yaml:"minimum_sessions"`
}

type Evaluation struct {
	Command            []string `yaml:"command,omitempty"`
	MinimumImprovement float64  `yaml:"minimum_improvement"`
	AllowAutopilot     bool     `yaml:"allow_autopilot"`
}

func Default() (Config, error) {
	dataDir, err := defaultDataDir()
	if err != nil {
		return Config{}, err
	}
	return Config{
		Version:      1,
		Mode:         domain.ModePropose,
		DataDir:      dataDir,
		PollInterval: 5 * time.Second,
		Aggregation:  Aggregation{MinimumSessions: 3},
		Evaluation:   Evaluation{MinimumImprovement: 0.1},
	}, nil
}

func DefaultPath() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve config directory: %w", err)
	}
	return filepath.Join(root, "skillloop", fileName), nil
}

func Load(path string) (Config, error) {
	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return Config{}, err
		}
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	config := Config{}
	if err := yaml.Unmarshal(contents, &config); err != nil {
		return Config{}, fmt.Errorf("decode config %s: %w", path, err)
	}
	if err := config.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate config %s: %w", path, err)
	}
	return config, nil
}

func (config Config) Validate() error {
	if config.Version != 1 {
		return fmt.Errorf("unsupported config version %d", config.Version)
	}
	if !config.Mode.Valid() {
		return fmt.Errorf("invalid autonomy mode %q", config.Mode)
	}
	if config.DataDir == "" {
		return errors.New("data_dir is required")
	}
	if config.PollInterval <= 0 {
		return errors.New("poll_interval must be positive")
	}
	if config.Aggregation.MinimumSessions < 2 {
		return errors.New("aggregation.minimum_sessions must be at least 2")
	}
	if config.Evaluation.MinimumImprovement < 0 {
		return errors.New("evaluation.minimum_improvement cannot be negative")
	}
	if config.Mode == domain.ModeAutopilot && (!config.Evaluation.AllowAutopilot || len(config.Evaluation.Command) == 0) {
		return errors.New("autopilot requires allow_autopilot and an external evaluation command")
	}
	return nil
}

func Encode(config Config) ([]byte, error) {
	contents, err := yaml.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	return contents, nil
}

func WriteInitial(path string, config Config) (string, error) {
	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return "", err
		}
	}
	if err := config.Validate(); err != nil {
		return "", fmt.Errorf("validate config: %w", err)
	}
	contents, err := Encode(config)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create config directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create config %s: %w", path, err)
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(contents); err != nil {
		return "", fmt.Errorf("write config %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("sync config %s: %w", path, err)
	}
	remove = false
	return path, nil
}

// Save atomically replaces an existing SkillLoop configuration.
func Save(path string, config Config) (string, error) {
	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return "", err
		}
	}
	if err := config.Validate(); err != nil {
		return "", fmt.Errorf("validate config: %w", err)
	}
	contents, err := Encode(config)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("create config directory: %w", err)
	}
	temporary, err := os.CreateTemp(parent, ".skillloop-config-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create config temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return "", fmt.Errorf("set config permissions: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		cleanup()
		return "", fmt.Errorf("write config: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return "", fmt.Errorf("sync config: %w", err)
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return "", fmt.Errorf("close config: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		cleanup()
		return "", fmt.Errorf("publish config %s: %w", path, err)
	}
	return path, nil
}

func defaultDataDir() (string, error) {
	if root := os.Getenv("XDG_DATA_HOME"); root != "" {
		return filepath.Join(root, "skillloop"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve data directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "skillloop"), nil
}
