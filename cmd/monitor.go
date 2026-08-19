package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/flemzord/skillloop/internal/pipeline"
)

func newMonitorCommand(options *rootOptions) *cobra.Command {
	once := false
	command := &cobra.Command{
		Use:   "monitor",
		Short: "Monitor active skill promotions",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			state, err := openRuntime(command.Context(), options.configPath)
			if err != nil {
				return err
			}
			defer state.close()
			manager := pipeline.New(state.config, state.store)
			for {
				result, err := manager.Monitor(command.Context())
				if err != nil {
					return err
				}
				if _, err := fmt.Fprintf(command.OutOrStdout(), "checked=%d healthy=%d regressing=%d rolled_back=%d failed=%d\n", result.Checked, result.Healthy, result.Regressing, result.RolledBack, len(result.Failures)); err != nil {
					return err
				}
				if once {
					return nil
				}
				timer := time.NewTimer(state.config.PollInterval)
				select {
				case <-command.Context().Done():
					timer.Stop()
					return command.Context().Err()
				case <-timer.C:
				}
			}
		},
	}
	command.Flags().BoolVar(&once, "once", false, "run one monitoring pass and exit")
	return command
}
