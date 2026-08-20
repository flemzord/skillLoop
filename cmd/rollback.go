package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/flemzord/skillloop/internal/pipeline"
)

func newRollbackCommand(options *rootOptions) *cobra.Command {
	reason := "manual rollback"
	jsonOutput := false
	command := &cobra.Command{
		Use:   "rollback <skill-id>",
		Short: "Roll back a skill's active promotion",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			state, err := openRuntime(command.Context(), options.configPath)
			if err != nil {
				return err
			}
			defer state.close()
			rollback, err := pipeline.New(state.config, state.store).Rollback(command.Context(), args[0], cliActor, reason)
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(command.OutOrStdout()).Encode(rollback)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "rolled_back\t%s\t%s\t%s\n", terminalSafe(args[0]), terminalSafe(rollback.FromCommit), terminalSafe(rollback.ToCommit))
			return err
		},
	}
	command.Flags().StringVar(&reason, "reason", reason, "reason for rolling back the promotion")
	command.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON")
	return command
}
