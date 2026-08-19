package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/flemzord/skillloop/internal/domain"
	"github.com/spf13/cobra"
)

func newSkillCommand(options *rootOptions) *cobra.Command {
	command := &cobra.Command{Use: "skill", Short: "Manage owned and versioned skills"}
	command.AddCommand(newSkillAddCommand(options), newSkillListCommand(options))
	return command
}

func newSkillAddCommand(options *rootOptions) *cobra.Command {
	name := ""
	instructionPath := "SKILL.md"
	command := &cobra.Command{
		Use:   "add <repository>",
		Short: "Register an owned skill repository",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			repositoryPath, relativePath, err := validateSkillRepository(command.Context(), args[0], instructionPath)
			if err != nil {
				return err
			}
			if name == "" {
				name = filepath.Base(repositoryPath)
			}
			state, err := openRuntime(command.Context(), options.configPath)
			if err != nil {
				return err
			}
			defer state.close()
			skill := domain.Skill{
				ID: stableSkillID(repositoryPath, relativePath), Name: name, RepositoryPath: repositoryPath,
				InstructionPath: relativePath, Enabled: true, CreatedAt: time.Now().UTC(),
			}
			created, err := state.store.RegisterSkill(command.Context(), skill)
			if err != nil {
				return err
			}
			if !created {
				return fmt.Errorf("skill %s is already registered", skill.ID)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "Registered %s (%s)\n", skill.Name, skill.ID)
			return err
		},
	}
	command.Flags().StringVar(&name, "name", "", "skill name (default: repository directory)")
	command.Flags().StringVar(&instructionPath, "file", instructionPath, "instruction file relative to the repository")
	return command
}

func newSkillListCommand(options *rootOptions) *cobra.Command {
	jsonOutput := false
	command := &cobra.Command{
		Use:   "list",
		Short: "List registered skills",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			state, err := openRuntime(command.Context(), options.configPath)
			if err != nil {
				return err
			}
			defer state.close()
			skills, err := state.store.ListSkills(command.Context())
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(command.OutOrStdout()).Encode(skills)
			}
			for _, skill := range skills {
				if _, err := fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\n", skill.ID, skill.Name, filepath.Join(skill.RepositoryPath, skill.InstructionPath)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON")
	return command
}

func validateSkillRepository(ctx context.Context, repository, instruction string) (string, string, error) {
	absolute, err := filepath.Abs(repository)
	if err != nil {
		return "", "", fmt.Errorf("resolve repository: %w", err)
	}
	absolute = filepath.Clean(absolute)
	relative := filepath.Clean(instruction)
	if relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", errors.New("instruction file must stay inside the repository")
	}
	rootOutput, err := exec.CommandContext(ctx, "git", "-C", absolute, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", "", fmt.Errorf("repository is not a Git worktree: %w", err)
	}
	root := filepath.Clean(strings.TrimSpace(string(rootOutput)))
	if root != absolute {
		return "", "", fmt.Errorf("register the repository root %s instead of %s", root, absolute)
	}
	path := filepath.Join(absolute, relative)
	info, err := os.Lstat(path)
	if err != nil {
		return "", "", fmt.Errorf("inspect instruction file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", errors.New("instruction file must be a regular non-symlink file")
	}
	if err := exec.CommandContext(ctx, "git", "-C", absolute, "ls-files", "--error-unmatch", "--", relative).Run(); err != nil {
		return "", "", fmt.Errorf("instruction file must be tracked by Git: %w", err)
	}
	return absolute, relative, nil
}

func stableSkillID(repository, instruction string) string {
	sum := sha256.Sum256([]byte(repository + "\x00" + instruction))
	return "skill-" + hex.EncodeToString(sum[:8])
}
