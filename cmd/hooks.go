package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/flemzord/skillloop/internal/domain"
	"github.com/flemzord/skillloop/internal/hooks"
)

func newHooksCommand(options *rootOptions) *cobra.Command {
	command := &cobra.Command{
		Use:   "hooks",
		Short: "Manage user-scoped Codex and Claude hooks",
	}
	command.AddCommand(newHooksInstallCommand(options), newHooksUninstallCommand(options))
	return command
}

func newHooksInstallCommand(options *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:          "install [codex|claude]",
		Short:        "Install SkillLoop capture hooks",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			installer, err := currentInstaller(options.configPath)
			if err != nil {
				return err
			}
			sources, err := selectedSources(args)
			if err != nil {
				return err
			}
			for _, source := range sources {
				if _, err := installer.Install(source); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newHooksUninstallCommand(options *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:          "uninstall [codex|claude]",
		Short:        "Remove SkillLoop capture hooks",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			installer, err := currentInstaller(options.configPath)
			if err != nil {
				return err
			}
			sources, err := selectedSources(args)
			if err != nil {
				return err
			}
			for _, source := range sources {
				if _, err := installer.Uninstall(source); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func currentInstaller(configPath string) (hooks.Installer, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return hooks.Installer{}, fmt.Errorf("resolve home directory: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return hooks.Installer{}, fmt.Errorf("resolve SkillLoop executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return hooks.Installer{}, fmt.Errorf("make SkillLoop executable path absolute: %w", err)
	}
	if configPath != "" {
		configPath, err = filepath.Abs(configPath)
		if err != nil {
			return hooks.Installer{}, fmt.Errorf("make config path absolute: %w", err)
		}
	}
	return hooks.Installer{HomeDir: home, Executable: executable, ConfigPath: configPath}, nil
}

func selectedSources(args []string) ([]domain.Source, error) {
	if len(args) == 0 {
		return []domain.Source{domain.SourceCodex, domain.SourceClaude}, nil
	}
	source, err := hooks.ParseSource(args[0])
	if err != nil {
		return nil, err
	}
	return []domain.Source{source}, nil
}
