package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"

	"github.com/flemzord/skillloop/internal/domain"
)

const (
	fileName           = "config.yaml"
	maxConfigFileBytes = 1 << 20
)

type Config struct {
	Version       int                 `yaml:"version"`
	Mode          domain.AutonomyMode `yaml:"mode"`
	DataDir       string              `yaml:"data_dir"`
	PollInterval  time.Duration       `yaml:"poll_interval"`
	Aggregation   Aggregation         `yaml:"aggregation"`
	Evaluation    Evaluation          `yaml:"evaluation"`
	Retention     Retention           `yaml:"retention"`
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

// Retention controls cleanup of ephemeral operational data. A zero duration
// explicitly means that the corresponding data is retained indefinitely.
// Learning cards, clusters, proposals, releases, and audit entries are durable
// learning state and are never covered by these settings.
type Retention struct {
	TranscriptLocators time.Duration `yaml:"transcript_locators"`
	FailedSpool        time.Duration `yaml:"failed_spool"`
	CompletedJobs      time.Duration `yaml:"completed_jobs"`
	FailedJobs         time.Duration `yaml:"failed_jobs"`
}

func defaultRetention() Retention {
	return Retention{
		TranscriptLocators: 30 * 24 * time.Hour,
		FailedSpool:        7 * 24 * time.Hour,
		CompletedJobs:      7 * 24 * time.Hour,
		FailedJobs:         30 * 24 * time.Hour,
	}
}

func (retention *Retention) UnmarshalYAML(node *yaml.Node) error {
	type plainRetention Retention
	*retention = defaultRetention()
	return node.Decode((*plainRetention)(retention))
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
		Retention:    defaultRetention(),
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
	contents, err := readConfigFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	config := Config{Retention: defaultRetention()}
	if err := yaml.Unmarshal(contents, &config); err != nil {
		return Config{}, fmt.Errorf("decode config %s: %w", path, err)
	}
	if err := config.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate config %s: %w", path, err)
	}
	return config, nil
}

// readConfigFile opens only the regular file that was inspected, without
// following a final symlink or blocking on a special file. The size checks
// before and after opening, plus the bounded read, keep untrusted input bounded
// before it reaches the YAML decoder.
func readConfigFile(path string) ([]byte, error) {
	expected, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !expected.Mode().IsRegular() {
		return nil, fmt.Errorf("config is not a regular file: %s", expected.Mode().Type())
	}
	if expected.Size() < 0 || expected.Size() > maxConfigFileBytes {
		return nil, fmt.Errorf("config exceeds %d bytes", maxConfigFileBytes)
	}

	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open config safely: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open config safely: invalid file descriptor")
	}
	defer func() { _ = file.Close() }()

	opened, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened config: %w", err)
	}
	if !opened.Mode().IsRegular() {
		return nil, fmt.Errorf("opened config is not a regular file: %s", opened.Mode().Type())
	}
	if !os.SameFile(expected, opened) {
		return nil, errors.New("config changed while it was being opened")
	}
	if opened.Size() < 0 || opened.Size() > maxConfigFileBytes {
		return nil, fmt.Errorf("config exceeds %d bytes", maxConfigFileBytes)
	}

	contents, err := io.ReadAll(io.LimitReader(file, maxConfigFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read config safely: %w", err)
	}
	if int64(len(contents)) > maxConfigFileBytes {
		return nil, fmt.Errorf("config exceeds %d bytes", maxConfigFileBytes)
	}
	return contents, nil
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
	if math.IsNaN(config.Evaluation.MinimumImprovement) || math.IsInf(config.Evaluation.MinimumImprovement, 0) {
		return errors.New("evaluation.minimum_improvement must be finite")
	}
	if config.Evaluation.MinimumImprovement < 0 {
		return errors.New("evaluation.minimum_improvement cannot be negative")
	}
	if config.Retention.TranscriptLocators < 0 {
		return errors.New("retention.transcript_locators cannot be negative (zero retains indefinitely)")
	}
	if config.Retention.FailedSpool < 0 {
		return errors.New("retention.failed_spool cannot be negative (zero retains indefinitely)")
	}
	if config.Retention.CompletedJobs < 0 {
		return errors.New("retention.completed_jobs cannot be negative (zero retains indefinitely)")
	}
	if config.Retention.FailedJobs < 0 {
		return errors.New("retention.failed_jobs cannot be negative (zero retains indefinitely)")
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
	unlock, err := acquirePolicyWriteLock(context.Background(), path)
	if err != nil {
		return "", err
	}
	defer func() { _ = unlock() }()
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
