package daemon

import (
	"context"
	"errors"
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

func TestDrainPrunesOnlyExpiredOperationalData(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(dataDir, "skillloop.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	transcriptPath := filepath.Join(dataDir, "source-transcript.jsonl")
	if err := os.WriteFile(transcriptPath, []byte("source remains local"), 0o600); err != nil {
		t.Fatalf("write source transcript: %v", err)
	}
	session := domain.Session{
		Reference: "retained-session", Source: domain.SourceCodex, ExternalID: "retained-session",
		TranscriptPath: transcriptPath, Outcome: domain.SessionOutcomeSucceeded,
	}
	if created, err := database.RecordSession(ctx, session); err != nil || !created {
		t.Fatalf("record session: created=%v err=%v", created, err)
	}
	job := domain.Job{ID: "old-completed", Kind: ingestJobKind, IdempotencyKey: "hook:old-completed", Payload: "old"}
	if created, err := database.EnqueueJob(ctx, job); err != nil || !created {
		t.Fatalf("enqueue job: created=%v err=%v", created, err)
	}
	if _, claimed, err := database.ClaimJob(ctx, job.ID, time.Minute); err != nil || !claimed {
		t.Fatalf("claim job: claimed=%v err=%v", claimed, err)
	}
	if err := database.CompleteJob(ctx, job.ID); err != nil {
		t.Fatalf("complete job: %v", err)
	}

	directories, err := ensureSpoolDirectories(dataDir)
	if err != nil {
		t.Fatalf("create spool: %v", err)
	}
	oldJSON := filepath.Join(directories.failed, "old.json")
	recentJSON := filepath.Join(directories.failed, "recent.json")
	oldNonJSON := filepath.Join(directories.failed, "old.txt")
	for _, path := range []string{oldJSON, recentJSON, oldNonJSON} {
		if err := os.WriteFile(path, []byte("failed event"), 0o600); err != nil {
			t.Fatalf("write failed spool fixture: %v", err)
		}
	}
	realTarget := filepath.Join(dataDir, "outside-target")
	if err := os.WriteFile(realTarget, []byte("do not follow"), 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	linkedJSON := filepath.Join(directories.failed, "linked.json")
	if err := os.Symlink(realTarget, linkedJSON); err != nil {
		t.Fatalf("create failed spool symlink: %v", err)
	}

	recordedAt := time.Now()
	oldTime := recordedAt.Add(-48 * time.Hour)
	processorNow := recordedAt.Add(48 * time.Hour)
	if err := os.Chtimes(oldJSON, oldTime, oldTime); err != nil {
		t.Fatalf("age old json: %v", err)
	}
	if err := os.Chtimes(recentJSON, processorNow.Add(-time.Hour), processorNow.Add(-time.Hour)); err != nil {
		t.Fatalf("set recent json time: %v", err)
	}
	if err := os.Chtimes(oldNonJSON, oldTime, oldTime); err != nil {
		t.Fatalf("age old non-json: %v", err)
	}

	settings, err := config.Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	settings.DataDir = dataDir
	settings.Retention = config.Retention{
		TranscriptLocators: 24 * time.Hour,
		FailedSpool:        24 * time.Hour,
		CompletedJobs:      24 * time.Hour,
		FailedJobs:         24 * time.Hour,
	}
	result, err := (Processor{Config: settings, Store: database, Now: func() time.Time { return processorNow }}).Drain(ctx, 10)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if result.PrunedTranscriptLocators != 1 || result.PrunedCompletedJobs != 1 || result.PrunedFailedJobs != 0 || result.PrunedFailedSpool != 1 {
		t.Fatalf("retention result = %#v", result)
	}
	counts, err := database.Counts(ctx, settings.Aggregation.MinimumSessions)
	if err != nil || counts.Sessions != 1 || len(counts.Jobs) != 0 {
		t.Fatalf("durable counts=%#v err=%v", counts, err)
	}
	for _, path := range []string{transcriptPath, recentJSON, oldNonJSON, linkedJSON, realTarget} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("expected retained path %s: %v", path, err)
		}
	}
	if _, err := os.Lstat(oldJSON); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired failed json still exists: %v", err)
	}
}

func TestDrainRetentionZeroKeepsOperationalDataIndefinitely(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(dataDir, "skillloop.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	session := domain.Session{
		Reference: "indefinite", Source: domain.SourceClaude, ExternalID: "indefinite",
		TranscriptPath: filepath.Join(dataDir, "transcript.jsonl"),
	}
	if created, err := database.RecordSession(ctx, session); err != nil || !created {
		t.Fatalf("record session: created=%v err=%v", created, err)
	}
	directories, err := ensureSpoolDirectories(dataDir)
	if err != nil {
		t.Fatalf("create spool: %v", err)
	}
	failedPath := filepath.Join(directories.failed, "forever.json")
	if err := os.WriteFile(failedPath, []byte("failed"), 0o600); err != nil {
		t.Fatalf("write failed spool fixture: %v", err)
	}
	veryOld := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(failedPath, veryOld, veryOld); err != nil {
		t.Fatalf("age failed fixture: %v", err)
	}
	settings, err := config.Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	settings.DataDir = dataDir
	settings.Retention = config.Retention{}
	result, err := (Processor{Config: settings, Store: database, Now: func() time.Time {
		return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	}}).Drain(ctx, 10)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if result.PrunedTranscriptLocators != 0 || result.PrunedCompletedJobs != 0 || result.PrunedFailedJobs != 0 || result.PrunedFailedSpool != 0 {
		t.Fatalf("retention result = %#v, want no pruning", result)
	}
	if _, err := os.Stat(failedPath); err != nil {
		t.Fatalf("zero retention removed failed spool file: %v", err)
	}
}
