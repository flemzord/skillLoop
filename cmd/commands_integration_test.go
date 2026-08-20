package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flemzord/skillloop/internal/domain"
)

func TestNominalCLIWorkflow(t *testing.T) {
	dataHome := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	configPath := filepath.Join(configHome, "skillloop", "integration.yaml")
	initOutput := executeIntegrationCommand(t, "--config", configPath, "init")
	if !strings.Contains(initOutput, "Initialized SkillLoop") || !strings.Contains(initOutput, configPath) {
		t.Fatalf("unexpected init output: %q", initOutput)
	}
	if info, err := os.Stat(configPath); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("initialized config is not a regular file: info=%v error=%v", info, err)
	}

	dataDir := filepath.Join(dataHome, "skillloop")
	databasePath := filepath.Join(dataDir, "skillloop.db")
	if info, err := os.Stat(databasePath); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("initialized database is not a regular file: info=%v error=%v", info, err)
	}

	var status struct {
		Mode    domain.AutonomyMode `json:"mode"`
		DataDir string              `json:"data_dir"`
		Counts  struct {
			Skills   int
			Sessions int
			Cards    int
		} `json:"counts"`
	}
	decodeIntegrationJSON(t, executeIntegrationCommand(t, "--config", configPath, "status", "--json"), &status)
	if status.Mode != domain.ModePropose || status.DataDir != dataDir {
		t.Fatalf("unexpected status identity: mode=%q data_dir=%q", status.Mode, status.DataDir)
	}
	if status.Counts.Skills != 0 || status.Counts.Sessions != 0 || status.Counts.Cards != 0 {
		t.Fatalf("unexpected initial status counts: %+v", status.Counts)
	}

	var cards []domain.LearningCard
	decodeIntegrationJSON(t, executeIntegrationCommand(t, "--config", configPath, "learning", "list", "--json"), &cards)
	if len(cards) != 0 {
		t.Fatalf("expected no learning cards, got %d", len(cards))
	}

	repository := createIntegrationSkillRepository(t)
	addOutput := executeIntegrationCommand(t, "--config", configPath, "skill", "add", repository)
	if !strings.Contains(addOutput, "Registered ") || !strings.Contains(addOutput, filepath.Base(repository)) {
		t.Fatalf("unexpected skill add output: %q", addOutput)
	}

	var skills []domain.Skill
	decodeIntegrationJSON(t, executeIntegrationCommand(t, "--config", configPath, "skill", "list", "--json"), &skills)
	if len(skills) != 1 {
		t.Fatalf("expected one durable skill registration, got %d", len(skills))
	}
	if skills[0].RepositoryPath != repository || skills[0].InstructionPath != "SKILL.md" || !skills[0].Enabled {
		t.Fatalf("unexpected registered skill: %+v", skills[0])
	}

	daemonOutput := executeIntegrationCommand(t, "--config", configPath, "daemon", "--once")
	for _, expected := range []string{"processed=0", "failed=0", "cards=0"} {
		if !strings.Contains(daemonOutput, expected) {
			t.Fatalf("daemon output does not contain %q: %q", expected, daemonOutput)
		}
	}
	for _, directory := range []string{"incoming", "processing", "failed"} {
		path := filepath.Join(dataDir, "spool", directory)
		entries, err := os.ReadDir(path)
		if err != nil {
			t.Fatalf("read daemon spool directory %s: %v", path, err)
		}
		if len(entries) != 0 {
			t.Fatalf("expected empty daemon spool directory %s, got %d entries", path, len(entries))
		}
	}
}

func executeIntegrationCommand(t *testing.T, args ...string) string {
	t.Helper()
	output := bytes.NewBuffer(nil)
	command := NewRootCommand()
	command.SetOut(output)
	command.SetErr(bytes.NewBuffer(nil))
	command.SetArgs(args)
	if err := command.Execute(); err != nil {
		t.Fatalf("skillloop %s: %v", strings.Join(args, " "), err)
	}
	return output.String()
}

func decodeIntegrationJSON(t *testing.T, output string, target any) {
	t.Helper()
	if err := json.Unmarshal([]byte(output), target); err != nil {
		t.Fatalf("decode JSON output %q: %v", output, err)
	}
}

func createIntegrationSkillRepository(t *testing.T) string {
	t.Helper()
	repository := filepath.Join(t.TempDir(), "integration skill")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatalf("create skill repository: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatalf("resolve skill repository: %v", err)
	}
	runIntegrationGit(t, resolved, "init", "--quiet")
	runIntegrationGit(t, resolved, "config", "user.name", "SkillLoop Integration Test")
	runIntegrationGit(t, resolved, "config", "user.email", "skillloop@example.invalid")
	if err := os.WriteFile(filepath.Join(resolved, "SKILL.md"), []byte("---\nname: integration\n---\n"), 0o600); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	runIntegrationGit(t, resolved, "add", "--", "SKILL.md")
	runIntegrationGit(t, resolved, "commit", "--quiet", "-m", "test: add skill")
	return resolved
}

func runIntegrationGit(t *testing.T, repository string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
