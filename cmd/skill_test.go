package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

func gitForSkillTest(t *testing.T, repository string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
