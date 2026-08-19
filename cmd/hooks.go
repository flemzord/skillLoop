package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/flemzord/skillloop/internal/domain"
	"github.com/flemzord/skillloop/internal/hooks"
	"github.com/spf13/cobra"
)

func newHooksCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "hooks",
		Short: "Manage user-scoped Codex and Claude hooks",
	}
	command.AddCommand(newHooksInstallCommand(), newHooksUninstallCommand())
	return command
}

func newHooksInstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "install [codex|claude]",
		Short:        "Install SkillLoop capture hooks",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			installer, err := currentInstaller()
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

func newHooksUninstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "uninstall [codex|claude]",
		Short:        "Remove SkillLoop capture hooks",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			installer, err := currentInstaller()
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

func currentInstaller() (hooks.Installer, error) {
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
	return hooks.Installer{HomeDir: home, Executable: executable}, nil
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
