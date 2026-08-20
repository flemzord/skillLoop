package activation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/flemzord/skillloop/internal/domain"
	"github.com/flemzord/skillloop/internal/improvement"
)

var (
	ErrUnsafeName   = errors.New("activation: unsafe skill name")
	ErrUnmanaged    = errors.New("activation: destination is not managed by SkillLoop")
	ErrNoRelease    = errors.New("activation: skill has no valid current release")
	validSkillID    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	validTargetName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

type Platform string

const (
	PlatformCodex  Platform = "codex"
	PlatformClaude Platform = "claude"
)

type Service struct {
	DataDir string
	HomeDir string
}

type Result struct {
	Platform    Platform
	Destination string
	Source      string
	Changed     bool
}

func (service Service) Install(skill domain.Skill, platforms []Platform) ([]Result, error) {
	if !skill.Enabled {
		return nil, errors.New("activation: skill is not enabled")
	}
	name, err := SafeName(skill.Name)
	if err != nil {
		return nil, err
	}
	source, err := service.currentSource(skill)
	if err != nil {
		return nil, err
	}
	plans, err := service.plan(name, source, platforms)
	if err != nil {
		return nil, err
	}

	results := make([]Result, 0, len(plans))
	created := make([]plannedLink, 0, len(plans))
	for _, plan := range plans {
		result := Result{Platform: plan.platform, Destination: plan.destination, Source: source}
		if plan.managed {
			results = append(results, result)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(plan.destination), 0o700); err != nil {
			return nil, rollback(created, source, fmt.Errorf("activation: create skills directory: %w", err))
		}
		// Creating a symlink publishes one complete directory entry and fails with
		// EEXIST instead of replacing a path created after the preflight check.
		if err := os.Symlink(source, plan.destination); err != nil {
			return nil, rollback(created, source, fmt.Errorf("activation: create %s skill link: %w", plan.platform, err))
		}
		created = append(created, plan)
		result.Changed = true
		results = append(results, result)
	}
	return results, nil
}

func (service Service) Uninstall(skill domain.Skill, platforms []Platform) ([]Result, error) {
	name, err := SafeName(skill.Name)
	if err != nil {
		return nil, err
	}
	source, err := service.sourcePath(skill.ID)
	if err != nil {
		return nil, err
	}
	plans, err := service.plan(name, source, platforms)
	if err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(plans))
	for _, plan := range plans {
		result := Result{Platform: plan.platform, Destination: plan.destination, Source: source}
		if !plan.managed {
			results = append(results, result)
			continue
		}
		managed, inspectErr := inspectLink(plan.destination, source)
		if inspectErr != nil {
			return nil, inspectErr
		}
		if !managed {
			results = append(results, result)
			continue
		}
		if err := os.Remove(plan.destination); err != nil {
			if os.IsNotExist(err) {
				results = append(results, result)
				continue
			}
			return nil, fmt.Errorf("activation: remove %s skill link: %w", plan.platform, err)
		}
		result.Changed = true
		results = append(results, result)
	}
	return results, nil
}

func SafeName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, `/\`) || strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("%w: %q", ErrUnsafeName, value)
	}
	var name strings.Builder
	separator := false
	for _, character := range strings.ToLower(value) {
		switch {
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9':
			if separator && name.Len() > 0 {
				name.WriteByte('-')
			}
			separator = false
			name.WriteRune(character)
		case character == '.', character == '-', character == '_':
			if name.Len() > 0 {
				separator = true
			}
		default:
			if name.Len() > 0 {
				separator = true
			}
		}
		if name.Len() >= 64 {
			break
		}
	}
	safe := strings.Trim(name.String(), ".-_")
	if len(safe) > 64 {
		safe = strings.TrimRight(safe[:64], ".-_")
	}
	if !validTargetName.MatchString(safe) {
		return "", fmt.Errorf("%w: %q", ErrUnsafeName, value)
	}
	return safe, nil
}

type plannedLink struct {
	platform    Platform
	destination string
	managed     bool
}

func (service Service) plan(name, source string, platforms []Platform) ([]plannedLink, error) {
	platforms, err := normalizePlatforms(platforms)
	if err != nil {
		return nil, err
	}
	plans := make([]plannedLink, 0, len(platforms))
	for _, platform := range platforms {
		root, rootErr := service.platformRoot(platform)
		if rootErr != nil {
			return nil, rootErr
		}
		destination := filepath.Join(root, name)
		managed, inspectErr := inspectLink(destination, source)
		if inspectErr != nil {
			return nil, inspectErr
		}
		plans = append(plans, plannedLink{platform: platform, destination: destination, managed: managed})
	}
	return plans, nil
}

func (service Service) currentSource(skill domain.Skill) (string, error) {
	current, err := service.sourcePath(skill.ID)
	if err != nil {
		return "", err
	}
	if _, err := (improvement.Service{StateDir: service.DataDir}).CurrentRelease(skill); err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoRelease, err)
	}
	return current, nil
}

func (service Service) sourcePath(skillID string) (string, error) {
	if service.DataDir == "" {
		return "", errors.New("activation: data directory is required")
	}
	if !validSkillID.MatchString(skillID) {
		return "", fmt.Errorf("activation: invalid skill ID %q", skillID)
	}
	dataDir, err := filepath.Abs(service.DataDir)
	if err != nil {
		return "", fmt.Errorf("activation: resolve data directory: %w", err)
	}
	return filepath.Join(dataDir, "releases", skillID, "current"), nil
}

func (service Service) platformRoot(platform Platform) (string, error) {
	if service.HomeDir == "" {
		return "", errors.New("activation: home directory is required")
	}
	home, err := filepath.Abs(service.HomeDir)
	if err != nil {
		return "", fmt.Errorf("activation: resolve home directory: %w", err)
	}
	switch platform {
	case PlatformCodex:
		return filepath.Join(home, ".codex", "skills"), nil
	case PlatformClaude:
		return filepath.Join(home, ".claude", "skills"), nil
	default:
		return "", fmt.Errorf("activation: unsupported platform %q", platform)
	}
}

func normalizePlatforms(platforms []Platform) ([]Platform, error) {
	if len(platforms) == 0 {
		platforms = []Platform{PlatformCodex, PlatformClaude}
	}
	seen := map[Platform]bool{}
	normalized := make([]Platform, 0, len(platforms))
	for _, platform := range platforms {
		if platform != PlatformCodex && platform != PlatformClaude {
			return nil, fmt.Errorf("activation: unsupported platform %q", platform)
		}
		if seen[platform] {
			continue
		}
		seen[platform] = true
		normalized = append(normalized, platform)
	}
	return normalized, nil
}

func inspectLink(destination, source string) (bool, error) {
	info, err := os.Lstat(destination)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("activation: inspect destination %s: %w", destination, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return false, fmt.Errorf("%w: %s", ErrUnmanaged, destination)
	}
	target, err := os.Readlink(destination)
	if err != nil {
		return false, fmt.Errorf("activation: read destination %s: %w", destination, err)
	}
	if target != source {
		return false, fmt.Errorf("%w: %s points to %s", ErrUnmanaged, destination, target)
	}
	return true, nil
}

func rollback(created []plannedLink, source string, cause error) error {
	errorsList := []error{cause}
	for _, plan := range slices.Backward(created) {
		managed, err := inspectLink(plan.destination, source)
		if err == nil && managed {
			err = os.Remove(plan.destination)
		}
		if err != nil {
			errorsList = append(errorsList, fmt.Errorf("activation: rollback %s link: %w", plan.platform, err))
		}
	}
	return errors.Join(errorsList...)
}
