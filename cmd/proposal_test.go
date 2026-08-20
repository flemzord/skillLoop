package cmd

import (
	"bytes"
	"encoding/json"
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

func TestProposalHumanOutputEscapesTerminalControlsButJSONStaysExact(t *testing.T) {
	diff := "diff --git a/SKILL.md b/SKILL.md\n+safe\tcolumn\n+\x1b[2Jforged\rstatus\n+\u202efilename"
	auditDetails := "{\"message\":\"\x1b[31mforged\rstatus\"}"
	view := pipeline.ProposalView{
		Proposal: domain.Proposal{ID: "proposal-1"},
		Diff:     diff,
		Audit: []domain.AuditEntry{{
			Action: "proposal.evaluated", Actor: "pipeline", Details: auditDetails,
		}},
	}

	output := bytes.NewBuffer(nil)
	if err := writeProposalView(output, view); err != nil {
		t.Fatalf("write human proposal: %v", err)
	}
	for _, forbidden := range []string{"\x1b", "\r", "\u202e"} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatalf("human output retained terminal control %q: %q", forbidden, output.String())
		}
	}
	for _, visible := range []string{`\x1b[2J`, `\x0dstatus`, `\u202efilename`, "safe\tcolumn"} {
		if !strings.Contains(output.String(), visible) {
			t.Fatalf("human output missing visible encoding %q: %q", visible, output.String())
		}
	}

	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal JSON proposal: %v", err)
	}
	var decoded pipeline.ProposalView
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal JSON proposal: %v", err)
	}
	if decoded.Diff != diff || len(decoded.Audit) != 1 || decoded.Audit[0].Details != auditDetails {
		t.Fatalf("JSON changed original controls: %#v", decoded)
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
