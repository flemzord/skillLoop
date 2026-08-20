package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

func newLearningCommand(options *rootOptions) *cobra.Command {
	command := &cobra.Command{Use: "learning", Short: "Inspect learning cards"}
	command.AddCommand(newLearningListCommand(options))
	return command
}

func newLearningListCommand(options *rootOptions) *cobra.Command {
	skillID := ""
	jsonOutput := false
	command := &cobra.Command{
		Use:   "list",
		Short: "List sanitized learning cards",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			state, err := openRuntime(command.Context(), options.configPath)
			if err != nil {
				return err
			}
			defer state.close()
			cards, err := state.store.ListLearningCards(command.Context(), skillID)
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(command.OutOrStdout()).Encode(cards)
			}
			for _, card := range cards {
				if _, err := fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%.2f\t%s\n", terminalSafe(card.ID), terminalSafe(string(card.Kind)), card.Confidence, terminalSafe(card.Summary)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().StringVar(&skillID, "skill", "", "filter by skill id")
	command.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON")
	return command
}

func newClusterCommand(options *rootOptions) *cobra.Command {
	command := &cobra.Command{Use: "cluster", Short: "Inspect recurring frictions"}
	command.AddCommand(newClusterListCommand(options))
	return command
}

func newClusterListCommand(options *rootOptions) *cobra.Command {
	eligible := false
	jsonOutput := false
	command := &cobra.Command{
		Use:   "list",
		Short: "List aggregated multi-session clusters",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			state, err := openRuntime(command.Context(), options.configPath)
			if err != nil {
				return err
			}
			defer state.close()
			minimum := 0
			if eligible {
				minimum = state.config.Aggregation.MinimumSessions
			}
			clusters, err := state.store.ListClusters(command.Context(), minimum)
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(command.OutOrStdout()).Encode(clusters)
			}
			for _, cluster := range clusters {
				if _, err := fmt.Fprintf(command.OutOrStdout(), "%s\t%s\tsessions=%d\t%s\n", terminalSafe(cluster.ID), terminalSafe(string(cluster.Status)), cluster.SessionCount, terminalSafe(cluster.Summary)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&eligible, "eligible", false, "show only clusters meeting the configured threshold")
	command.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON")
	return command
}
