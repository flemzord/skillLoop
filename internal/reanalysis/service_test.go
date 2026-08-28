package reanalysis

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/flemzord/skillloop/internal/domain"
	"github.com/flemzord/skillloop/internal/learning"
	"github.com/flemzord/skillloop/internal/store"
	"github.com/flemzord/skillloop/internal/transcript"
)

func TestRunReanalyzesCurrentAndArchivedTranscriptsIdempotently(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	codexHome := filepath.Join(home, ".codex")
	sessionsRoot := filepath.Join(codexHome, "sessions")
	archiveRoot := filepath.Join(codexHome, "archived_sessions")
	for _, path := range []string{sessionsRoot, archiveRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)

	database, err := store.Open(ctx, filepath.Join(root, "state", "skillloop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	repository := filepath.Join(root, "repository", "demo")
	installed := filepath.Join(home, ".agents", "skills", "demo")
	for _, path := range []string{repository, installed} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte("# Demo\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	skill := domain.Skill{ID: "skill-demo", Name: "demo", RepositoryPath: repository, InstructionPath: "SKILL.md", Enabled: true}
	if created, err := database.RegisterSkill(ctx, skill); err != nil || !created {
		t.Fatalf("register skill: created=%v err=%v", created, err)
	}

	workingDir := filepath.Join(root, "project")
	if err := os.MkdirAll(workingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	currentID := "11111111-1111-1111-1111-111111111111"
	archivedID := "22222222-2222-2222-2222-222222222222"
	currentPath := filepath.Join(sessionsRoot, "rollout-"+currentID+".jsonl")
	originalArchivePath := filepath.Join(sessionsRoot, "rollout-"+archivedID+".jsonl")
	archivedPath := filepath.Join(archiveRoot, "rollout-2026-08-28-"+archivedID+".jsonl")
	writeCodexTranscript(t, currentPath, currentID, workingDir, filepath.Join(installed, "SKILL.md"))
	writeCodexTranscript(t, archivedPath, archivedID, workingDir, filepath.Join(installed, "SKILL.md"))
	for _, session := range []domain.Session{
		{Reference: "codex:" + currentID, Source: domain.SourceCodex, ExternalID: currentID, WorkingDir: workingDir, TranscriptPath: currentPath},
		{Reference: "codex:" + archivedID, Source: domain.SourceCodex, ExternalID: archivedID, WorkingDir: workingDir, TranscriptPath: originalArchivePath},
	} {
		if created, err := database.RecordSession(ctx, session); err != nil || !created {
			t.Fatalf("record session: created=%v err=%v", created, err)
		}
	}

	service := Service{Store: database, Normalizer: transcript.Normalizer{}, Analyzer: learning.NewAnalyzer()}
	dryRun, err := service.Run(ctx, Options{DryRun: true, MinimumSessions: 2})
	if err != nil {
		t.Fatal(err)
	}
	if dryRun.Sessions != 2 || dryRun.Resolved != 2 || dryRun.CardsNew != 2 || dryRun.CardsCreated != 0 || dryRun.EligibleClusters != 1 {
		t.Fatalf("unexpected dry run: %#v", dryRun)
	}
	if cards, err := database.ListLearningCards(ctx, ""); err != nil || len(cards) != 0 {
		t.Fatalf("dry run persisted cards: cards=%#v err=%v", cards, err)
	}

	result, err := service.Run(ctx, Options{MinimumSessions: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.CardsCreated != 2 || result.EligibleClusters != 1 {
		t.Fatalf("unexpected write result: %#v", result)
	}
	repeated, err := service.Run(ctx, Options{MinimumSessions: 2})
	if err != nil {
		t.Fatal(err)
	}
	if repeated.CardsNew != 0 || repeated.CardsCreated != 0 {
		t.Fatalf("reanalysis was not idempotent: %#v", repeated)
	}
}

func TestRunReportsMissingTranscriptWithoutAborting(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(root, "state", "skillloop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	missing := filepath.Join(root, "provider", "missing.jsonl")
	if created, err := database.RecordSession(ctx, domain.Session{
		Reference: "codex:missing", Source: domain.SourceCodex, ExternalID: "missing",
		WorkingDir: root, TranscriptPath: missing,
	}); err != nil || !created {
		t.Fatalf("record session: created=%v err=%v", created, err)
	}
	service := Service{
		Store: database,
		Normalizer: transcript.Normalizer{AllowedRoots: map[domain.Source][]string{
			domain.SourceCodex: {filepath.Join(root, "provider")},
		}},
		Analyzer: learning.NewAnalyzer(),
	}
	result, err := service.Run(ctx, Options{DryRun: true, MinimumSessions: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.Missing != 1 || result.Failed != 0 || len(result.Issues) != 1 {
		t.Fatalf("unexpected missing result: %#v", result)
	}
}

func writeCodexTranscript(t *testing.T, path, sessionID, workingDir, instructionPath string) {
	t.Helper()
	wrapper := fmt.Sprintf(`const r = await tools.exec_command({cmd:%q,workdir:%q}); text(r.output);`, "cat "+instructionPath, workingDir)
	contents := fmt.Sprintf(
		"{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cwd\":%q}}\n"+
			"{\"type\":\"response_item\",\"payload\":{\"type\":\"custom_tool_call\",\"name\":\"exec\",\"call_id\":\"read-skill\",\"input\":%q}}\n"+
			"{\"type\":\"response_item\",\"payload\":{\"type\":\"custom_tool_call_output\",\"call_id\":\"read-skill\",\"output\":\"# Demo\"}}\n"+
			"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"Non, il faut lancer les tests avec Nix.\"}]}}\n",
		sessionID, workingDir, wrapper,
	)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
