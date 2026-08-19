package cmd

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flemzord/skillloop/internal/config"
	"github.com/flemzord/skillloop/internal/domain"
	"github.com/flemzord/skillloop/internal/pipeline"
)

func TestProposalAndPromotionCommandsAreRegistered(t *testing.T) {
	command := NewRootCommand()

	for _, path := range []string{
		"proposal list",
		"proposal create",
		"proposal show",
		"proposal evaluate",
		"proposal approve",
		"proposal reject",
		"rollback",
		"monitor",
	} {
		if found, _, err := command.Find(strings.Fields(path)); err != nil || found.CommandPath() != "skillloop "+path {
			t.Fatalf("find %q: command=%v error=%v", path, found, err)
		}
	}
}

func TestProposalCommandsValidateArgumentsBeforeOpeningRuntime(t *testing.T) {
	for _, args := range [][]string{
		{"proposal", "list", "unexpected"},
		{"proposal", "create"},
		{"proposal", "show"},
		{"proposal", "evaluate"},
		{"proposal", "approve"},
		{"proposal", "reject"},
		{"rollback"},
		{"monitor", "unexpected"},
	} {
		command := NewRootCommand()
		command.SetOut(bytes.NewBuffer(nil))
		command.SetErr(bytes.NewBuffer(nil))
		command.SetArgs(args)
		if err := command.Execute(); err == nil {
			t.Fatalf("execute %q unexpectedly succeeded", strings.Join(args, " "))
		}
	}
}

func TestProposalListEmptyOutput(t *testing.T) {
	configPath := writeCLIConfig(t)
	command := NewRootCommand()
	output := bytes.NewBuffer(nil)
	command.SetOut(output)
	command.SetErr(bytes.NewBuffer(nil))
	command.SetArgs([]string{"--config", configPath, "proposal", "list"})

	if err := command.Execute(); err != nil {
		t.Fatalf("list proposals: %v", err)
	}
	if output.String() != "" {
		t.Fatalf("unexpected output: %q", output.String())
	}
}

func TestMonitorOnceEmptyOutput(t *testing.T) {
	configPath := writeCLIConfig(t)
	command := NewRootCommand()
	output := bytes.NewBuffer(nil)
	command.SetOut(output)
	command.SetErr(bytes.NewBuffer(nil))
	command.SetArgs([]string{"--config", configPath, "monitor", "--once"})

	if err := command.Execute(); err != nil {
		t.Fatalf("monitor once: %v", err)
	}
	if output.String() != "checked=0 healthy=0 regressing=0 rolled_back=0 failed=0\n" {
		t.Fatalf("unexpected output: %q", output.String())
	}
}

func TestProposalRejectReasonDefault(t *testing.T) {
	command := newProposalRejectCommand(&rootOptions{})
	flag := command.Flag("reason")
	if flag == nil || flag.DefValue != "rejected by user" {
		t.Fatalf("unexpected reject reason default: %#v", flag)
	}
}

func TestProposalShowOutputIncludesCandidateDiff(t *testing.T) {
	view := pipeline.ProposalView{
		Proposal: domain.Proposal{
			ID: "proposal-1", Status: domain.ProposalEvaluated,
			SkillID: "skill-1", ClusterID: "cluster-1",
			BaseCommit: "base-sha", CandidateCommit: "candidate-sha",
			BaselineScore: 0, CandidateScore: 1,
		},
		Diff:                  "diff --git a/SKILL.md b/SKILL.md\n+managed improvement",
		RequiresHumanApproval: true,
	}
	output := bytes.NewBuffer(nil)
	if err := writeProposalView(output, view); err != nil {
		t.Fatalf("write proposal view: %v", err)
	}
	for _, expected := range []string{
		"commits\tbase=base-sha\tcandidate=candidate-sha",
		"scores\tbaseline=0.00\tcandidate=1.00",
		"requires_human_approval\ttrue",
		"diff_begin\ndiff --git a/SKILL.md b/SKILL.md\n+managed improvement\ndiff_end",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("show output does not contain %q: %q", expected, output.String())
		}
	}
}

func TestRollbackReasonDefault(t *testing.T) {
	command := newRollbackCommand(&rootOptions{})
	flag := command.Flag("reason")
	if flag == nil || flag.DefValue != "manual rollback" {
		t.Fatalf("unexpected rollback reason default: %#v", flag)
	}
}

func writeCLIConfig(t *testing.T) string {
	t.Helper()
	settings, err := config.Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	settings.DataDir = t.TempDir()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := config.WriteInitial(path, settings); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
