package cmd

import (
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/flemzord/skillloop/internal/config"
	"github.com/flemzord/skillloop/internal/domain"
)

func newStatusCommand(options *rootOptions) *cobra.Command {
	jsonOutput := false
	command := &cobra.Command{
		Use:   "status",
		Short: "Show the local learning loop state",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			state, err := openRuntime(command.Context(), options.configPath)
			if err != nil {
				return err
			}
			defer state.close()
			counts, err := state.store.Counts(command.Context(), state.config.Aggregation.MinimumSessions)
			if err != nil {
				return err
			}
			if jsonOutput {
				encoder := json.NewEncoder(command.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(map[string]any{"mode": state.config.Mode, "data_dir": state.config.DataDir, "counts": counts})
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "Mode: %s\nSkills: %d\nSessions: %d\nLearning cards: %d\nClusters: %d (%d eligible)\nProposals: %d\nActive promotions: %d\n",
				terminalSafe(string(state.config.Mode)), counts.Skills, counts.Sessions, counts.Cards, counts.Clusters, counts.EligibleClusters,
				proposalCount(counts.Proposals), counts.ActivePromotions)
			return err
		},
	}
	command.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON")
	return command
}

func proposalCount(values map[domain.ProposalStatus]int) int {
	total := 0
	for _, count := range values {
		total += count
	}
	return total
}

func newDoctorCommand(options *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check configuration and local dependencies",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			settings, err := config.Load(options.configPath)
			if err != nil {
				return err
			}
			state, err := openRuntime(command.Context(), options.configPath)
			if err != nil {
				return err
			}
			state.close()
			if _, err := exec.LookPath("git"); err != nil {
				return fmt.Errorf("git is required: %w", err)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "OK config=%s mode=%s data=%s git=available sqlite=ready\n", terminalSafe(configPathLabel(options.configPath)), terminalSafe(string(settings.Mode)), terminalSafe(settings.DataDir))
			return err
		},
	}
}

func configPathLabel(path string) string {
	if path != "" {
		return path
	}
	resolved, err := config.DefaultPath()
	if err != nil {
		return "default"
	}
	return resolved
}
