package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

type rootOptions struct {
	configPath string
}

func Execute(ctx context.Context) error {
	return NewRootCommand().ExecuteContext(ctx)
}

func NewRootCommand() *cobra.Command {
	opts := rootOptions{}

	root := &cobra.Command{
		Use:           "skillloop",
		Short:         "Continuously improve Codex and Claude Code skills",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.PersistentFlags().StringVar(&opts.configPath, "config", "", "configuration file (default: XDG config directory)")
	root.AddCommand(
		newVersionCommand(), newInitCommand(&opts), newDoctorCommand(&opts), newStatusCommand(&opts),
		newHookCommand(&opts), newHooksCommand(&opts), newSkillCommand(&opts), newDaemonCommand(&opts),
		newLearningCommand(&opts), newClusterCommand(&opts), newModeCommand(&opts),
	)

	return root
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		RunE: func(command *cobra.Command, _ []string) error {
			return writeVersion(command.OutOrStdout())
		},
	}
}

func writeVersion(writer io.Writer) error {
	_, err := fmt.Fprintf(writer, "skillloop %s (commit=%s, built=%s)\n", Version, Commit, BuildDate)
	return err
}
