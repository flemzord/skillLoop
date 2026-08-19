package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flemzord/skillloop/internal/capture"
	"github.com/flemzord/skillloop/internal/config"
	"github.com/flemzord/skillloop/internal/domain"
	"github.com/flemzord/skillloop/internal/store"
)

func TestDrainCreatesOneClusterFromThreeSessions(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(dataDir, "skillloop.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	repository := filepath.Join(dataDir, "skills", "go-service")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "SKILL.md"), []byte("---\nname: go-service\ndescription: Go service workflow\n---\n\n# Go service\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	daemonGit(t, repository, "init", "-b", "main")
	daemonGit(t, repository, "config", "user.name", "Test User")
	daemonGit(t, repository, "config", "user.email", "test@example.invalid")
	daemonGit(t, repository, "add", "SKILL.md")
	daemonGit(t, repository, "commit", "-m", "feat: add skill")
	skill := domain.Skill{
		ID: "go-service", Name: "go-service", RepositoryPath: repository,
		InstructionPath: "SKILL.md", Enabled: true, CreatedAt: time.Now(),
	}
	if _, err := database.RegisterSkill(ctx, skill); err != nil {
		t.Fatalf("register skill: %v", err)
	}
	workingDir := filepath.Join(dataDir, "project")
	if err := os.MkdirAll(workingDir, 0o700); err != nil {
		t.Fatalf("create working dir: %v", err)
	}

	for index := 1; index <= 3; index++ {
		path := filepath.Join(dataDir, fmt.Sprintf("transcript-%d.jsonl", index))
		contents := fmt.Sprintf("{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":%q}]}}\n{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"Non, il faut lancer les tests avec Nix.\"}]}}\n", "Loaded "+skill.RepositoryPath+"/SKILL.md")
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatalf("write transcript: %v", err)
		}
		event := domain.HookEvent{
			ID: fmt.Sprintf("event-%d", index), Source: domain.SourceCodex, SessionID: fmt.Sprintf("session-%d", index),
			TranscriptPath: path, WorkingDir: workingDir, HookEventName: "stop", CapturedAt: time.Now(),
		}
		if _, err := (capture.Spool{DataDir: dataDir}).Write(event); err != nil {
			t.Fatalf("spool event: %v", err)
		}
	}

	settings, err := config.Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	settings.DataDir = dataDir
	settings.Aggregation.MinimumSessions = 3
	processor := Processor{Config: settings, Store: database}
	result, err := processor.Drain(ctx, 100)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if result.Processed != 3 || result.CardsCreated != 3 || len(result.EligibleClusters) != 1 ||
		result.ProposalsCreated != 1 || result.ProposalsEvaluated != 1 || result.ProposalFailures != 0 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if result.EligibleClusters[0].SessionCount != 3 {
		t.Fatalf("expected 3 distinct sessions, got %#v", result.EligibleClusters[0])
	}

	duplicate := domain.HookEvent{
		ID: "event-3", Source: domain.SourceCodex, SessionID: "session-3",
		TranscriptPath: filepath.Join(dataDir, "transcript-3.jsonl"), WorkingDir: workingDir,
		HookEventName: "stop", CapturedAt: time.Now(),
	}
	if _, err := (capture.Spool{DataDir: dataDir}).Write(duplicate); err != nil {
		t.Fatalf("spool duplicate: %v", err)
	}
	result, err = processor.Drain(ctx, 100)
	if err != nil {
		t.Fatalf("drain duplicate: %v", err)
	}
	if result.CardsCreated != 0 || len(result.EligibleClusters) != 1 || result.EligibleClusters[0].SessionCount != 3 {
		t.Fatalf("duplicate changed durable state: %#v", result)
	}
	if result.ProposalsCreated != 0 || result.ProposalsEvaluated != 0 || result.ProposalFailures != 0 {
		t.Fatalf("duplicate reran proposal pipeline: %#v", result)
	}
	proposals, err := database.ListProposals(ctx, "")
	if err != nil || len(proposals) != 1 || proposals[0].Status != domain.ProposalEvaluated {
		t.Fatalf("durable proposals=%#v err=%v", proposals, err)
	}
}

func daemonGit(t *testing.T, repository string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), output, err)
	}
}

func TestDrainExcludesSkillLoopMarkedWorkspace(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(dataDir, "skillloop.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	workingDir := filepath.Join(dataDir, "self", "nested")
	if err := os.MkdirAll(workingDir, 0o700); err != nil {
		t.Fatalf("create working dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "self", ".skillloop-ignore"), nil, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if _, err := (capture.Spool{DataDir: dataDir}).Write(domain.HookEvent{
		ID: "self-event", Source: domain.SourceClaude, SessionID: "self-session", WorkingDir: workingDir,
		HookEventName: "stop", CapturedAt: time.Now(),
	}); err != nil {
		t.Fatalf("spool event: %v", err)
	}
	settings, err := config.Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	settings.DataDir = dataDir
	result, err := (Processor{Config: settings, Store: database}).Drain(ctx, 10)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if result.Excluded != 1 || result.Failed != 0 {
		t.Fatalf("unexpected exclusion result: %#v", result)
	}
}

func TestDrainClaimsTheJobMatchingTheCurrentSpoolEvent(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(dataDir, "skillloop.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	oldJob := domain.Job{
		ID:             "older-job",
		Kind:           ingestJobKind,
		IdempotencyKey: "hook:older-job",
		Payload:        "older-job",
		Status:         domain.JobQueued,
		AvailableAt:    time.Now().Add(-time.Hour),
	}
	if created, err := database.EnqueueJob(ctx, oldJob); err != nil || !created {
		t.Fatalf("enqueue older job: created=%v err=%v", created, err)
	}

	workingDir := filepath.Join(dataDir, "excluded")
	if err := os.MkdirAll(workingDir, 0o700); err != nil {
		t.Fatalf("create working dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workingDir, ".skillloop-ignore"), nil, 0o600); err != nil {
		t.Fatalf("write exclusion marker: %v", err)
	}
	if _, err := (capture.Spool{DataDir: dataDir}).Write(domain.HookEvent{
		ID: "current-event", Source: domain.SourceCodex, SessionID: "current-session",
		WorkingDir: workingDir, HookEventName: "stop", CapturedAt: time.Now(),
	}); err != nil {
		t.Fatalf("spool current event: %v", err)
	}

	settings, err := config.Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	settings.DataDir = dataDir
	result, err := (Processor{Config: settings, Store: database}).Drain(ctx, 1)
	if err != nil {
		t.Fatalf("drain current event: %v", err)
	}
	if result.Excluded != 1 || result.Failed != 0 {
		t.Fatalf("current event result = %#v, want one exclusion without failure", result)
	}

	claimed, ok, err := database.ClaimJob(ctx, oldJob.ID, time.Minute)
	if err != nil || !ok || claimed.ID != oldJob.ID {
		t.Fatalf("older job should remain queued: job=%#v ok=%v err=%v", claimed, ok, err)
	}
}
