package cmd

import (
	"bytes"
	"errors"
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

func TestTerminalErrorEscapesHumanStderrAndPreservesCause(t *testing.T) {
	marker := errors.New("forged\x1b]0;title\x07\r\n\t\u0085\u202e")
	err := terminalError{cause: marker}
	if !errors.Is(err, marker) {
		t.Fatal("terminal error did not preserve its cause")
	}
	if err.Error() != terminalSafe(marker.Error()) {
		t.Fatalf("terminal error = %q, want %q", err.Error(), terminalSafe(marker.Error()))
	}
}
