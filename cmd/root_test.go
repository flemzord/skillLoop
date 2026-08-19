package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	command := NewRootCommand()
	output := bytes.NewBuffer(nil)
	command.SetOut(output)
	command.SetArgs([]string{"version"})

	if err := command.Execute(); err != nil {
		t.Fatalf("execute version: %v", err)
	}
	if !strings.Contains(output.String(), "skillloop dev") {
		t.Fatalf("unexpected output: %q", output.String())
	}
}
