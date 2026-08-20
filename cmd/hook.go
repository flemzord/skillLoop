package cmd

import (
	"errors"
	"flag"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/flemzord/skillloop/internal/capture"
	"github.com/flemzord/skillloop/internal/config"
	"github.com/flemzord/skillloop/internal/hooks"
)

func newHookCommand(options *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:                "hook",
		Short:              "Capture a Codex or Claude lifecycle event",
		DisableFlagParsing: true,
		Run: func(command *cobra.Command, args []string) {
			runHook(command.InOrStdin(), args, options.configPath)
		},
	}
}

func runHook(input io.Reader, args []string, configPath string) {
	flags := flag.NewFlagSet("hook", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	providerValue := flags.String("provider", "", "")
	eventValue := flags.String("event", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return
	}
	source, err := hooks.ParseSource(*providerValue)
	if err != nil {
		return
	}
	event, err := hooks.ParseEvent(*eventValue)
	if err != nil {
		return
	}
	contents, err := capture.ReadHookInput(input)
	if err != nil {
		return
	}
	normalized, err := hooks.Normalize(source, event, contents)
	if err != nil {
		return
	}
	dataDir, err := hookDataDir(configPath)
	if err != nil {
		return
	}
	_, _ = (capture.Spool{DataDir: dataDir}).Write(normalized)
}

func hookDataDir(configPath string) (string, error) {
	if configPath != "" {
		loaded, err := config.Load(configPath)
		if err != nil {
			return "", err
		}
		return loaded.DataDir, nil
	}
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return "", err
	}
	loaded, err := config.Load(defaultPath)
	if err == nil {
		return loaded.DataDir, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	defaults, err := config.Default()
	if err != nil {
		return "", err
	}
	return defaults.DataDir, nil
}
