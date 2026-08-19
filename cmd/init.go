package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/flemzord/skillloop/internal/config"
	"github.com/flemzord/skillloop/internal/domain"
	"github.com/flemzord/skillloop/internal/store"
)

func newInitCommand(options *rootOptions) *cobra.Command {
	mode := string(domain.ModePropose)
	command := &cobra.Command{
		Use:   "init",
		Short: "Initialize private local SkillLoop state",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			settings, err := config.Default()
			if err != nil {
				return err
			}
			settings.Mode = domain.AutonomyMode(mode)
			if !settings.Mode.Valid() {
				return fmt.Errorf("invalid mode %q", mode)
			}
			path, err := config.WriteInitial(options.configPath, settings)
			if err != nil {
				return err
			}
			database, err := store.Open(command.Context(), filepath.Join(settings.DataDir, "skillloop.db"))
			if err != nil {
				return err
			}
			if err := database.Close(); err != nil {
				return err
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "Initialized SkillLoop in %s (config: %s, mode: %s)\n", settings.DataDir, path, settings.Mode)
			return err
		},
	}
	command.Flags().StringVar(&mode, "mode", mode, "autonomy mode: observe, propose, or autopilot")
	return command
}

func newModeCommand(options *rootOptions) *cobra.Command {
	command := &cobra.Command{Use: "mode", Short: "Inspect SkillLoop autonomy"}
	command.AddCommand(&cobra.Command{
		Use:   "get",
		Short: "Print the configured autonomy mode",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			settings, err := config.Load(options.configPath)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), settings.Mode)
			return err
		},
	})
	command.AddCommand(&cobra.Command{
		Use:   "set <observe|propose|autopilot>",
		Short: "Change the configured autonomy mode",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			settings, err := config.Load(options.configPath)
			if err != nil {
				return err
			}
			settings.Mode = domain.AutonomyMode(args[0])
			if !settings.Mode.Valid() {
				return fmt.Errorf("invalid mode %q", args[0])
			}
			path, err := config.Save(options.configPath, settings)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "Mode set to %s in %s\n", settings.Mode, path)
			return err
		},
	})
	return command
}
