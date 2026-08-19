package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/flemzord/skillloop/internal/activation"
	"github.com/flemzord/skillloop/internal/domain"
)

type skillPlatformOptions struct {
	codexOnly  bool
	claudeOnly bool
}

func newSkillInstallCommand(options *rootOptions) *cobra.Command {
	targets := skillPlatformOptions{}
	command := &cobra.Command{
		Use:   "install <skill-id>",
		Short: "Expose a promoted skill release to Codex and Claude",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			platforms, err := targets.platforms()
			if err != nil {
				return err
			}
			state, err := openRuntime(command.Context(), options.configPath)
			if err != nil {
				return err
			}
			defer state.close()
			skill, err := resolveSkill(command, state, args[0])
			if err != nil {
				return err
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("resolve home directory: %w", err)
			}
			results, err := (activation.Service{DataDir: state.config.DataDir, HomeDir: home}).Install(skill, platforms)
			if err != nil {
				return err
			}
			for _, result := range results {
				status := "already installed"
				if result.Changed {
					status = "installed"
				}
				if _, err := fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\n", result.Platform, status, result.Destination); err != nil {
					return err
				}
			}
			return nil
		},
	}
	targets.addFlags(command)
	return command
}

func newSkillUninstallCommand(options *rootOptions) *cobra.Command {
	targets := skillPlatformOptions{}
	command := &cobra.Command{
		Use:   "uninstall <skill-id>",
		Short: "Remove SkillLoop-managed Codex and Claude skill links",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			platforms, err := targets.platforms()
			if err != nil {
				return err
			}
			state, err := openRuntime(command.Context(), options.configPath)
			if err != nil {
				return err
			}
			defer state.close()
			skill, err := resolveSkill(command, state, args[0])
			if err != nil {
				return err
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("resolve home directory: %w", err)
			}
			results, err := (activation.Service{DataDir: state.config.DataDir, HomeDir: home}).Uninstall(skill, platforms)
			if err != nil {
				return err
			}
			for _, result := range results {
				status := "not installed"
				if result.Changed {
					status = "uninstalled"
				}
				if _, err := fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\n", result.Platform, status, result.Destination); err != nil {
					return err
				}
			}
			return nil
		},
	}
	targets.addFlags(command)
	return command
}

func (options *skillPlatformOptions) addFlags(command *cobra.Command) {
	command.Flags().BoolVar(&options.codexOnly, "codex-only", false, "target only Codex")
	command.Flags().BoolVar(&options.claudeOnly, "claude-only", false, "target only Claude Code")
}

func (options skillPlatformOptions) platforms() ([]activation.Platform, error) {
	if options.codexOnly && options.claudeOnly {
		return nil, errors.New("--codex-only and --claude-only are mutually exclusive")
	}
	if options.codexOnly {
		return []activation.Platform{activation.PlatformCodex}, nil
	}
	if options.claudeOnly {
		return []activation.Platform{activation.PlatformClaude}, nil
	}
	return []activation.Platform{activation.PlatformCodex, activation.PlatformClaude}, nil
}

func resolveSkill(command *cobra.Command, state *runtimeState, skillID string) (domain.Skill, error) {
	skill, err := state.store.Skill(command.Context(), skillID)
	if err != nil {
		return domain.Skill{}, fmt.Errorf("resolve skill %q: %w", skillID, err)
	}
	return skill, nil
}
