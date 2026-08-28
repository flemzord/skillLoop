package cmd

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/flemzord/skillloop/internal/learning"
	"github.com/flemzord/skillloop/internal/reanalysis"
	"github.com/flemzord/skillloop/internal/transcript"
)

func newLearningCommand(options *rootOptions) *cobra.Command {
	command := &cobra.Command{Use: "learning", Short: "Inspect learning cards"}
	command.AddCommand(newLearningListCommand(options), newLearningReanalyzeCommand(options))
	return command
}

func newLearningReanalyzeCommand(options *rootOptions) *cobra.Command {
	all := false
	dryRun := false
	jsonOutput := false
	command := &cobra.Command{
		Use:   "reanalyze",
		Short: "Reanalyze stored sessions, including archived transcripts",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if !all {
				return errors.New("learning reanalyze requires --all")
			}
			state, err := openRuntime(command.Context(), options.configPath)
			if err != nil {
				return err
			}
			defer state.close()

			service := reanalysis.Service{
				Store:      state.store,
				Normalizer: transcript.Normalizer{},
				Analyzer:   learning.NewAnalyzer(),
			}
			result, err := service.Run(command.Context(), reanalysis.Options{
				DryRun:          dryRun,
				MinimumSessions: state.config.Aggregation.MinimumSessions,
			})
			if err != nil {
				return err
			}
			reanalysis.SortIssues(result.Issues)
			if jsonOutput {
				encoder := json.NewEncoder(command.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(result)
			}
			if _, err := fmt.Fprintf(command.OutOrStdout(),
				"dry_run=%t sessions=%d resolved=%d missing=%d failed=%d sessions_with_cards=%d cards_found=%d cards_new=%d cards_created=%d clusters=%d eligible_clusters=%d\n",
				result.DryRun, result.Sessions, result.Resolved, result.Missing, result.Failed,
				result.SessionsWithCards, result.CardsFound, result.CardsNew, result.CardsCreated,
				result.Clusters, result.EligibleClusters,
			); err != nil {
				return err
			}
			for _, issue := range result.Issues {
				if _, err := fmt.Fprintf(command.ErrOrStderr(), "skip %s: %s\n", terminalSafe(issue.SessionRef), terminalSafe(issue.Reason)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&all, "all", false, "reanalyze every stored session")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "analyze without writing cards or rebuilding clusters")
	command.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON")
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
