package cmd

import (
	"context"
	"strings"
	"testing"
)

func TestLearningReanalyzeRequiresExplicitAll(t *testing.T) {
	command := NewRootCommand()
	command.SetArgs([]string{"learning", "reanalyze", "--dry-run"})
	err := command.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "requires --all") {
		t.Fatalf("error = %v, want explicit --all requirement", err)
	}
}
