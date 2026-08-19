package cmd

import (
	"fmt"
	"time"

	"github.com/flemzord/skillloop/internal/daemon"
	"github.com/spf13/cobra"
)

func newDaemonCommand(options *rootOptions) *cobra.Command {
	once := false
	limit := 100
	command := &cobra.Command{
		Use:   "daemon",
		Short: "Process captured sessions asynchronously",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			state, err := openRuntime(command.Context(), options.configPath)
			if err != nil {
				return err
			}
			defer state.close()
			processor := daemon.Processor{Config: state.config, Store: state.store}
			for {
				result, err := processor.Drain(command.Context(), limit)
				if err != nil {
					return err
				}
				if result.Captured > 0 || once {
					if _, err := fmt.Fprintf(command.OutOrStdout(), "processed=%d excluded=%d failed=%d cards=%d eligible_clusters=%d\n", result.Processed, result.Excluded, result.Failed, result.CardsCreated, len(result.EligibleClusters)); err != nil {
						return err
					}
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
	command.Flags().BoolVar(&once, "once", false, "drain the current spool and exit")
	command.Flags().IntVar(&limit, "limit", limit, "maximum events processed per drain")
	return command
}
