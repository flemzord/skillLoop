package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/flemzord/skillloop/internal/domain"
)

func TestValidateSkillRepository(t *testing.T) {
	tests := []struct {
		name        string
		instruction func(repository string) string
		prepare     func(t *testing.T, repository string)
		repository  func(repository string) string
		wantPath    string
		wantError   string
	}{
		{
			name:        "repository root",
			instruction: func(string) string { return "SKILL.md" },
			prepare:     func(t *testing.T, repository string) { writeAndTrack(t, repository, "SKILL.md") },
			wantPath:    "SKILL.md",
		},
		{
			name:        "nested repository path",
			instruction: func(string) string { return "SKILL.md" },
			prepare:     func(t *testing.T, repository string) { writeAndTrack(t, repository, "nested/SKILL.md") },
			repository:  func(repository string) string { return filepath.Join(repository, "nested") },
			wantError:   "register the repository root",
		},
		{
			name:        "parent traversal",
			instruction: func(string) string { return filepath.Join("..", "SKILL.md") },
			wantError:   "instruction file must stay inside the repository",
		},
		{
			name:        "absolute instruction path",
			instruction: func(repository string) string { return filepath.Join(repository, "SKILL.md") },
			wantError:   "instruction file must stay inside the repository",
		},
		{
			name:        "symlink instruction file",
			instruction: func(string) string { return "SKILL.md" },
			prepare: func(t *testing.T, repository string) {
				t.Helper()
				writeAndTrack(t, repository, "target.md")
				if err := os.Symlink("target.md", filepath.Join(repository, "SKILL.md")); err != nil {
					t.Fatalf("create instruction symlink: %v", err)
				}
				gitForSkillTest(t, repository, "add", "SKILL.md")
			},
			wantError: "instruction file must be a regular non-symlink file",
		},
		{
			name:        "untracked instruction file",
			instruction: func(string) string { return "SKILL.md" },
			prepare:     func(t *testing.T, repository string) { writeSkillTestFile(t, repository, "SKILL.md") },
			wantError:   "instruction file must be tracked by Git",
		},
		{
			name:        "tracked nested instruction file",
			instruction: func(string) string { return filepath.Join("nested", "SKILL.md") },
			prepare:     func(t *testing.T, repository string) { writeAndTrack(t, repository, "nested/SKILL.md") },
			wantPath:    filepath.Join("nested", "SKILL.md"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := initSkillTestRepository(t)
			if test.prepare != nil {
				test.prepare(t, repository)
			}
			inputRepository := repository
			if test.repository != nil {
				inputRepository = test.repository(repository)
			}

			gotRepository, gotPath, err := validateSkillRepository(
				context.Background(), inputRepository, test.instruction(repository),
			)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("validateSkillRepository() error = %v, want containing %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateSkillRepository() error = %v", err)
			}
			if gotRepository != repository || gotPath != test.wantPath {
				t.Fatalf("validateSkillRepository() = (%q, %q), want (%q, %q)", gotRepository, gotPath, repository, test.wantPath)
			}
		})
	}
}

func TestValidateSkillRepositoryDisablesLocalFSMonitor(t *testing.T) {
	repository := initSkillTestRepository(t)
	writeAndTrack(t, repository, "SKILL.md")
	monitor, marker := writeSkillGitMonitor(t)
	gitForSkillTest(t, repository, "config", "--local", "core.fsmonitor", monitor)
	assertUnhardenedGitInvokesMonitor(t, repository, marker)

	if _, _, err := validateSkillRepository(context.Background(), repository, "SKILL.md"); err != nil {
		t.Fatalf("validate legitimate repository: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("local core.fsmonitor executed during validation: %v", err)
	}
}

func TestValidateSkillRepositoryDisablesFSMonitorFromIncludeIf(t *testing.T) {
	repository := initSkillTestRepository(t)
	writeAndTrack(t, repository, "SKILL.md")
	monitor, marker := writeSkillGitMonitor(t)
	includedConfig := filepath.Join(t.TempDir(), "included.gitconfig")
	if err := os.WriteFile(includedConfig, []byte("[core]\n\tfsmonitor = "+monitor+"\n"), 0o600); err != nil {
		t.Fatalf("write included Git config: %v", err)
	}
	gitDirectory := filepath.ToSlash(filepath.Join(repository, ".git"))
	gitForSkillTest(t, repository, "config", "--local", "includeIf.gitdir:"+gitDirectory+".path", includedConfig)
	command := exec.Command("git", "-C", repository, "config", "--get", "core.fsmonitor")
	output, err := command.Output()
	if err != nil || strings.TrimSpace(string(output)) != monitor {
		t.Fatalf("included fsmonitor was not active: output=%q error=%v", output, err)
	}
	assertUnhardenedGitInvokesMonitor(t, repository, marker)

	if _, _, err := validateSkillRepository(context.Background(), repository, "SKILL.md"); err != nil {
		t.Fatalf("validate legitimate repository: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("includeIf core.fsmonitor executed during validation: %v", err)
	}
}

func TestValidateSkillRepositoryDropsInheritedGitConfig(t *testing.T) {
	repository := initSkillTestRepository(t)
	writeAndTrack(t, repository, "SKILL.md")
	monitor, marker := writeSkillGitMonitor(t)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.fsmonitor")
	t.Setenv("GIT_CONFIG_VALUE_0", monitor)

	if _, _, err := validateSkillRepository(context.Background(), repository, "SKILL.md"); err != nil {
		t.Fatalf("validate legitimate repository: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("inherited Git configuration executed during validation: %v", err)
	}
}

func TestSkillHumanOutputEscapesControlsAndJSONStaysExact(t *testing.T) {
	configPath := writeCLIConfig(t)
	repository := initSkillTestRepository(t)
	writeAndTrack(t, repository, "SKILL.md")
	name := "owned\x1b]0;forged\x07\r\n\t\u0085\u202e\u2066skill"

	addOutput := executeSkillCommand(t, "--config", configPath, "skill", "add", "--name", name, repository)
	assertNoLowerTrustTerminalControls(t, addOutput)
	if !strings.Contains(addOutput, terminalSafe(name)) {
		t.Fatalf("skill add output = %q, want encoded name %q", addOutput, terminalSafe(name))
	}

	listOutput := executeSkillCommand(t, "--config", configPath, "skill", "list")
	assertNoLowerTrustTerminalControls(t, listOutput)
	if !strings.Contains(listOutput, terminalSafe(name)) {
		t.Fatalf("skill list output = %q, want encoded name %q", listOutput, terminalSafe(name))
	}

	jsonOutput := executeSkillCommand(t, "--config", configPath, "skill", "list", "--json")
	var skills []domain.Skill
	if err := json.Unmarshal([]byte(jsonOutput), &skills); err != nil {
		t.Fatalf("decode skill JSON %q: %v", jsonOutput, err)
	}
	if len(skills) != 1 || skills[0].Name != name || skills[0].RepositoryPath != repository {
		t.Fatalf("JSON changed registered skill: %#v", skills)
	}

	statusOutput := executeSkillCommand(t, "--config", configPath, "status")
	if !strings.Contains(statusOutput, "Skills: 1\n") {
		t.Fatalf("status output = %q, want registered skill count", statusOutput)
	}
}

func initSkillTestRepository(t *testing.T) string {
	t.Helper()
	repository := filepath.Join(t.TempDir(), "skill")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatalf("create repository: %v", err)
	}
	repository, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatalf("resolve repository symlinks: %v", err)
	}
	gitForSkillTest(t, repository, "init")
	return repository
}

func writeAndTrack(t *testing.T, repository, relativePath string) {
	t.Helper()
	writeSkillTestFile(t, repository, relativePath)
	gitForSkillTest(t, repository, "add", "--", relativePath)
}

func writeSkillTestFile(t *testing.T, repository, relativePath string) {
	t.Helper()
	path := filepath.Join(repository, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create instruction directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("---\nname: test\n---\n"), 0o600); err != nil {
		t.Fatalf("write instruction file: %v", err)
	}
}

func writeSkillGitMonitor(t *testing.T) (string, string) {
	t.Helper()
	monitor := filepath.Join(t.TempDir(), "fsmonitor.sh")
	if err := os.WriteFile(monitor, []byte("#!/bin/sh\n: > \"$0.invoked\"\nexit 1\n"), 0o700); err != nil {
		t.Fatalf("write fsmonitor: %v", err)
	}
	return monitor, monitor + ".invoked"
}

func assertUnhardenedGitInvokesMonitor(t *testing.T, repository, marker string) {
	t.Helper()
	command := exec.Command("git", "-C", repository, "ls-files", "--error-unmatch", "--", "SKILL.md")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("exercise unhardened Git query: %v\n%s", err, output)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("malicious fsmonitor fixture did not execute: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("reset fsmonitor marker: %v", err)
	}
}

func executeSkillCommand(t *testing.T, arguments ...string) string {
	t.Helper()
	output := bytes.NewBuffer(nil)
	command := NewRootCommand()
	command.SetOut(output)
	command.SetErr(bytes.NewBuffer(nil))
	command.SetArgs(arguments)
	if err := command.Execute(); err != nil {
		t.Fatalf("skillloop %s: %v", strings.Join(arguments, " "), err)
	}
	return output.String()
}

func assertNoLowerTrustTerminalControls(t *testing.T, value string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(value, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("lower-trust value injected output lines: %q", value)
	}
	for _, character := range lines[0] {
		if character == 0x7f || character != '\t' && (unicode.IsControl(character) || unicode.Is(unicode.Cf, character)) {
			t.Fatalf("human output retained terminal control U+%04X: %q", character, value)
		}
	}
}

func gitForSkillTest(t *testing.T, repository string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
