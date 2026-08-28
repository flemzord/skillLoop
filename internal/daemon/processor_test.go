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

	"golang.org/x/sys/unix"

	"github.com/flemzord/skillloop/internal/capture"
	"github.com/flemzord/skillloop/internal/config"
	"github.com/flemzord/skillloop/internal/domain"
	"github.com/flemzord/skillloop/internal/store"
)

func TestDrainCreatesOneClusterFromThreeSessions(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	codexSessions := filepath.Join(dataDir, "codex-home", "sessions")
	t.Setenv("CODEX_HOME", filepath.Dir(codexSessions))
	if err := os.MkdirAll(codexSessions, 0o700); err != nil {
		t.Fatalf("create Codex session directory: %v", err)
	}
	database, err := store.Open(ctx, filepath.Join(dataDir, "skillloop.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	repository := filepath.Join(dataDir, "skills", "go-service")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	skillContents := []byte("---\nname: go-service\ndescription: Go service workflow\n---\n\n# Go service\n")
	if err := os.WriteFile(filepath.Join(repository, "SKILL.md"), skillContents, 0o600); err != nil {
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
	home := filepath.Join(dataDir, "home")
	t.Setenv("HOME", home)
	storeSkill := filepath.Join(dataDir, "nix-store", "go-service")
	if err := os.MkdirAll(storeSkill, 0o700); err != nil {
		t.Fatalf("create immutable skill copy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(storeSkill, "SKILL.md"), skillContents, 0o600); err != nil {
		t.Fatalf("write immutable skill copy: %v", err)
	}
	installedSkill := filepath.Join(home, ".agents", "skills", skill.Name)
	if err := os.MkdirAll(filepath.Dir(installedSkill), 0o700); err != nil {
		t.Fatalf("create skill installation root: %v", err)
	}
	if err := os.Symlink(storeSkill, installedSkill); err != nil {
		t.Fatalf("link installed skill: %v", err)
	}
	workingDir := filepath.Join(dataDir, "project")
	if err := os.MkdirAll(workingDir, 0o700); err != nil {
		t.Fatalf("create working dir: %v", err)
	}

	for index := 1; index <= 3; index++ {
		path := filepath.Join(codexSessions, fmt.Sprintf("transcript-%d.jsonl", index))
		sessionID := fmt.Sprintf("session-%d", index)
		readCommand := "cat " + filepath.Join(installedSkill, skill.InstructionPath)
		wrapper := fmt.Sprintf(`const r = await tools.exec_command({cmd:%q,workdir:%q}); text(r.output);`, readCommand, workingDir)
		contents := codexTranscriptContents(sessionID, workingDir, fmt.Sprintf("{\"type\":\"response_item\",\"payload\":{\"type\":\"custom_tool_call\",\"name\":\"exec\",\"call_id\":\"read-skill-%d\",\"input\":%q}}\n{\"type\":\"response_item\",\"payload\":{\"type\":\"custom_tool_call_output\",\"call_id\":\"read-skill-%d\",\"output\":\"skill loaded\"}}\n{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"Non, il faut lancer les tests avec Nix.\"}]}}\n", index, wrapper, index))
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatalf("write transcript: %v", err)
		}
		event := domain.HookEvent{
			ID: fmt.Sprintf("event-%d", index), Source: domain.SourceCodex, SessionID: sessionID,
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
		TranscriptPath: filepath.Join(codexSessions, "transcript-3.jsonl"), WorkingDir: workingDir,
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
	transcriptPath := writeDaemonTranscript(t, dataDir, domain.SourceClaude, "self-session", workingDir, "{}\n")
	if _, err := (capture.Spool{DataDir: dataDir}).Write(domain.HookEvent{
		ID: "self-event", Source: domain.SourceClaude, SessionID: "self-session", WorkingDir: workingDir,
		TranscriptPath: transcriptPath, HookEventName: "stop", CapturedAt: time.Now(),
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
	transcriptPath := writeDaemonTranscript(t, dataDir, domain.SourceCodex, "current-session", workingDir, "{}\n")
	if _, err := (capture.Spool{DataDir: dataDir}).Write(domain.HookEvent{
		ID: "current-event", Source: domain.SourceCodex, SessionID: "current-session",
		WorkingDir: workingDir, TranscriptPath: transcriptPath, HookEventName: "stop", CapturedAt: time.Now(),
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

func TestDrainResumesRecoveredQueuedAndExpiredJobs(t *testing.T) {
	for _, test := range []struct {
		name       string
		processing bool
	}{
		{name: "queued"},
		{name: "expired processing", processing: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			dataDir := t.TempDir()
			database, err := store.Open(ctx, filepath.Join(dataDir, "skillloop.db"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = database.Close() })

			event, settings, directories := recoveredEventFixture(t, dataDir, "recovered-"+strings.ReplaceAll(test.name, " ", "-"))
			job := domain.Job{
				ID: event.ID, Kind: ingestJobKind, IdempotencyKey: "hook:" + event.ID,
				Payload: event.ID, Status: domain.JobQueued, AvailableAt: time.Now().Add(-time.Minute),
			}
			if created, err := database.EnqueueJob(ctx, job); err != nil || !created {
				t.Fatalf("enqueue recovered job: created=%v err=%v", created, err)
			}
			if test.processing {
				claimed, ok, err := database.ClaimJob(ctx, event.ID, time.Nanosecond)
				if err != nil || !ok || claimed.ID != event.ID {
					t.Fatalf("claim recovered job: job=%#v ok=%v err=%v", claimed, ok, err)
				}
				for !time.Now().After(claimed.LeasedUntil) {
					time.Sleep(time.Millisecond)
				}
			}

			result, err := (Processor{Config: settings, Store: database}).Drain(ctx, 10)
			if err != nil {
				t.Fatalf("drain recovered job: %v", err)
			}
			if result.Excluded != 1 || result.Failed != 0 {
				t.Fatalf("recovered result = %#v, want one exclusion", result)
			}
			persisted, err := database.Job(ctx, event.ID)
			if err != nil || persisted.Status != domain.JobCompleted {
				t.Fatalf("recovered job = %#v err=%v, want completed", persisted, err)
			}
			for _, path := range []string{
				filepath.Join(directories.incoming, event.ID+".json"),
				filepath.Join(directories.processing, event.ID+".json"),
			} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("acknowledged spool path still exists %s: %v", path, err)
				}
			}
		})
	}
}

func TestDrainDefersRecoveredEventWithLiveLease(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(dataDir, "skillloop.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	event, settings, directories := recoveredEventFixture(t, dataDir, "live-lease")
	job := domain.Job{
		ID: event.ID, Kind: ingestJobKind, IdempotencyKey: "hook:" + event.ID,
		Payload: event.ID, Status: domain.JobQueued, AvailableAt: time.Now().Add(-time.Minute),
	}
	if created, err := database.EnqueueJob(ctx, job); err != nil || !created {
		t.Fatalf("enqueue live job: created=%v err=%v", created, err)
	}
	claimed, ok, err := database.ClaimJob(ctx, event.ID, time.Hour)
	if err != nil || !ok {
		t.Fatalf("claim live job: ok=%v err=%v", ok, err)
	}

	result, err := (Processor{Config: settings, Store: database}).Drain(ctx, 10)
	if err != nil {
		t.Fatalf("drain live job: %v", err)
	}
	if result.Failed != 0 || result.Processed != 0 || result.Excluded != 0 {
		t.Fatalf("live lease result = %#v, want deferred event", result)
	}
	persisted, err := database.Job(ctx, event.ID)
	if err != nil || persisted.Status != domain.JobProcessing || persisted.LeasedUntil != claimed.LeasedUntil {
		t.Fatalf("live job changed: before=%#v after=%#v err=%v", claimed, persisted, err)
	}
	if _, err := os.Lstat(filepath.Join(directories.incoming, event.ID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live leased event was duplicated into incoming: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directories.processing, event.ID+".json")); err != nil {
		t.Fatalf("live leased event did not remain with its lease owner: %v", err)
	}
}

func recoveredEventFixture(t *testing.T, dataDir, eventID string) (domain.HookEvent, config.Config, spoolDirectories) {
	t.Helper()
	directories, err := ensureSpoolDirectories(dataDir)
	if err != nil {
		t.Fatalf("ensure spool directories: %v", err)
	}
	t.Cleanup(directories.close)
	workingDir := filepath.Join(dataDir, "excluded-"+eventID)
	if err := os.MkdirAll(workingDir, 0o700); err != nil {
		t.Fatalf("create excluded directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workingDir, ".skillloop-ignore"), nil, 0o600); err != nil {
		t.Fatalf("write exclusion marker: %v", err)
	}
	event := domain.HookEvent{
		ID: eventID, Source: domain.SourceCodex, SessionID: "session-" + eventID,
		WorkingDir: workingDir, HookEventName: "stop", CapturedAt: time.Now(),
	}
	event.TranscriptPath = writeDaemonTranscript(t, dataDir, event.Source, event.SessionID, event.WorkingDir, "{}\n")
	incomingPath, err := (capture.Spool{DataDir: dataDir}).Write(event)
	if err != nil {
		t.Fatalf("write recovered event: %v", err)
	}
	if err := os.Rename(incomingPath, filepath.Join(directories.processing, filepath.Base(incomingPath))); err != nil {
		t.Fatalf("stage processing event: %v", err)
	}
	settings, err := config.Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	settings.DataDir = dataDir
	return event, settings, directories
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
	claimedJob, claimed, err := database.ClaimJob(ctx, job.ID, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim job: claimed=%v err=%v", claimed, err)
	}
	if err := database.CompleteJob(ctx, job.ID, claimedJob.FencingToken); err != nil {
		t.Fatalf("complete job: %v", err)
	}

	directories, err := ensureSpoolDirectories(dataDir)
	if err != nil {
		t.Fatalf("create spool: %v", err)
	}
	t.Cleanup(directories.close)
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
	hardlinkedJSON := filepath.Join(directories.failed, "hardlinked.json")
	if err := os.Link(realTarget, hardlinkedJSON); err != nil {
		t.Fatalf("create failed spool hardlink: %v", err)
	}
	fifoJSON := filepath.Join(directories.failed, "fifo.json")
	if err := unix.Mkfifo(fifoJSON, 0o600); err != nil {
		t.Fatalf("create failed spool fifo: %v", err)
	}
	oversizedJSON := filepath.Join(directories.failed, "oversized.json")
	if err := os.WriteFile(oversizedJSON, make([]byte, capture.MaxHookInputBytes+1), 0o600); err != nil {
		t.Fatalf("create failed spool oversized file: %v", err)
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
	if result.PrunedTranscriptLocators != 1 || result.PrunedCompletedJobs != 1 || result.PrunedFailedJobs != 0 || result.PrunedFailedSpool != 7 {
		t.Fatalf("retention result = %#v", result)
	}
	counts, err := database.Counts(ctx, settings.Aggregation.MinimumSessions)
	if err != nil || counts.Sessions != 1 || len(counts.Jobs) != 0 {
		t.Fatalf("durable counts=%#v err=%v", counts, err)
	}
	for _, path := range []string{transcriptPath, realTarget} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("expected retained path %s: %v", path, err)
		}
	}
	for _, path := range []string{oldJSON, recentJSON, oldNonJSON, linkedJSON, hardlinkedJSON, fifoJSON, oversizedJSON} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expired failed spool entry still exists %s: %v", path, err)
		}
	}
	if contents, err := os.ReadFile(realTarget); err != nil || string(contents) != "do not follow" {
		t.Fatalf("retention changed external target: contents=%q err=%v", contents, err)
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
	t.Cleanup(directories.close)
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

func TestDrainQuarantinesPoisonEntriesWithoutStarvingValidWork(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(dataDir, "skillloop.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	directories, err := ensureSpoolDirectories(dataDir)
	if err != nil {
		t.Fatalf("ensure spool: %v", err)
	}
	t.Cleanup(directories.close)

	fifo := filepath.Join(directories.incoming, "000-fifo.json")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("create fifo: %v", err)
	}
	target := filepath.Join(dataDir, "outside-target")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(directories.incoming, "001-symlink.json")); err != nil {
		t.Fatalf("create symlink poison: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(directories.incoming, "002-oversized.json"),
		make([]byte, capture.MaxHookInputBytes+1),
		0o600,
	); err != nil {
		t.Fatalf("write oversized poison: %v", err)
	}
	if err := os.Link(target, filepath.Join(directories.incoming, "003-hardlink.json")); err != nil {
		t.Fatalf("create hardlink poison: %v", err)
	}
	excludedDir := filepath.Join(dataDir, "excluded")
	if err := os.Mkdir(excludedDir, 0o700); err != nil {
		t.Fatalf("mkdir excluded: %v", err)
	}
	if err := os.WriteFile(filepath.Join(excludedDir, ".skillloop-ignore"), nil, 0o600); err != nil {
		t.Fatalf("write exclusion marker: %v", err)
	}
	transcriptPath := writeDaemonTranscript(t, dataDir, domain.SourceCodex, "valid-session", excludedDir, "{}\n")
	if _, err := (capture.Spool{DataDir: dataDir}).Write(domain.HookEvent{
		ID: "zzz-valid", Source: domain.SourceCodex, SessionID: "valid-session",
		WorkingDir: excludedDir, TranscriptPath: transcriptPath, HookEventName: "stop", CapturedAt: time.Now(),
	}); err != nil {
		t.Fatalf("write valid event: %v", err)
	}
	settings, err := config.Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	settings.DataDir = dataDir
	total := DrainResult{}
	for range 5 {
		result, err := (Processor{Config: settings, Store: database}).Drain(ctx, 1)
		if err != nil {
			t.Fatalf("drain poison entries: %v", err)
		}
		if result.Captured > 1 {
			t.Fatalf("drain exceeded capture budget: %#v", result)
		}
		total.Captured += result.Captured
		total.Excluded += result.Excluded
		total.Failed += result.Failed
	}
	if total.Captured != 5 || total.Excluded != 1 || total.Failed != 4 {
		t.Fatalf("poison drain result = %#v", total)
	}
	failed, err := os.ReadDir(directories.failed)
	if err != nil || len(failed) != 4 {
		t.Fatalf("quarantine entries=%v err=%v", failed, err)
	}
	contents, err := os.ReadFile(target)
	if err != nil || string(contents) != "untouched" {
		t.Fatalf("symlink target changed: contents=%q err=%v", contents, err)
	}
}

func TestDrainChargesEveryEntryAgainstTheBatchBudget(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(dataDir, "skillloop.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	directories, err := ensureSpoolDirectories(dataDir)
	if err != nil {
		t.Fatalf("ensure spool: %v", err)
	}
	t.Cleanup(directories.close)
	for index := range spoolDirectoryBatchSize + 17 {
		name := fmt.Sprintf("poison-%03d.txt", index)
		if err := os.WriteFile(filepath.Join(directories.incoming, name), []byte("{"), 0o600); err != nil {
			t.Fatalf("write poison fixture: %v", err)
		}
	}
	settings, err := config.Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	settings.DataDir = dataDir
	result, err := (Processor{Config: settings, Store: database}).Drain(ctx, 7)
	if err != nil {
		t.Fatalf("drain bounded poison batch: %v", err)
	}
	if result.Captured != 7 || result.Failed != 7 {
		t.Fatalf("bounded poison result=%#v, want exactly seven charged entries", result)
	}
	incoming, err := os.ReadDir(directories.incoming)
	if err != nil || len(incoming) != spoolDirectoryBatchSize+10 {
		t.Fatalf("remaining incoming=%d err=%v", len(incoming), err)
	}
}

func TestFailedSpoolPruningIsBoundedAndEventuallyCompletes(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	directories, err := ensureSpoolDirectories(dataDir)
	if err != nil {
		t.Fatalf("ensure spool: %v", err)
	}
	t.Cleanup(directories.close)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	const limit = 3
	const total = limit*2 + 1
	for index := range total {
		name := fmt.Sprintf("expired-%02d.failed-%016x-%024x.txt", index, now.Add(-2*time.Hour).UnixNano(), index+1)
		if err := os.WriteFile(filepath.Join(directories.failed, name), []byte("failed"), 0o600); err != nil {
			t.Fatalf("write failed fixture: %v", err)
		}
	}
	prunedTotal := 0
	complete := false
	for drain := 0; drain < 4 && !complete; drain++ {
		pruned, reachedEOF, err := pruneFailedSpool(ctx, directories, now, time.Hour, limit)
		if err != nil {
			t.Fatalf("bounded failed prune: %v", err)
		}
		if pruned > limit {
			t.Fatalf("pruned %d entries with limit %d", pruned, limit)
		}
		prunedTotal += pruned
		complete = reachedEOF
	}
	if !complete || prunedTotal != total {
		t.Fatalf("failed prune complete=%v total=%d, want %d", complete, prunedTotal, total)
	}
	entries, err := os.ReadDir(directories.failed)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed spool did not converge: entries=%v err=%v", entries, err)
	}
}

func TestFailedSpoolRetentionUsesDaemonQuarantineTime(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(dataDir, "skillloop.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	directories, err := ensureSpoolDirectories(dataDir)
	if err != nil {
		t.Fatalf("ensure spool: %v", err)
	}
	t.Cleanup(directories.close)
	poison := filepath.Join(directories.incoming, "future-mtime.json")
	if err := os.WriteFile(poison, []byte("{"), 0o600); err != nil {
		t.Fatalf("write poison fixture: %v", err)
	}
	future := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(poison, future, future); err != nil {
		t.Fatalf("set attacker-controlled future mtime: %v", err)
	}
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	settings, err := config.Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	settings.DataDir = dataDir
	settings.Retention.FailedSpool = time.Hour
	processor := Processor{Config: settings, Store: database, Now: func() time.Time { return now }}
	result, err := processor.Drain(ctx, 1)
	if err != nil || result.Failed != 1 {
		t.Fatalf("quarantine future-mtime poison: result=%#v err=%v", result, err)
	}
	matches, err := filepath.Glob(filepath.Join(directories.failed, "future-mtime.failed-*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("quarantine matches=%v err=%v", matches, err)
	}
	quarantinedAt, ok := quarantineTimestamp(filepath.Base(matches[0]))
	if !ok || !quarantinedAt.Equal(now) {
		t.Fatalf("trusted quarantine time=%s ok=%v, want %s", quarantinedAt, ok, now)
	}
	processor.Now = func() time.Time { return now.Add(2 * time.Hour) }
	result, err = processor.Drain(ctx, 1)
	if err != nil || result.PrunedFailedSpool != 1 {
		t.Fatalf("prune by daemon time: result=%#v err=%v", result, err)
	}
	if _, err := os.Lstat(matches[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("future mtime retained quarantine: %v", err)
	}
}

func TestDrainRecoversTerminalSpoolBeforePruningJobTombstone(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(dataDir, "skillloop.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	event, settings, directories := recoveredEventFixture(t, dataDir, "terminal-before-prune")
	job := domain.Job{
		ID: event.ID, Kind: ingestJobKind, IdempotencyKey: "hook:" + event.ID,
		Payload: event.ID, Status: domain.JobQueued, AvailableAt: time.Now().Add(-time.Minute),
	}
	if created, err := database.EnqueueJob(ctx, job); err != nil || !created {
		t.Fatalf("enqueue terminal job: created=%v err=%v", created, err)
	}
	claimed, ok, err := database.ClaimJob(ctx, event.ID, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim terminal job: job=%#v ok=%v err=%v", claimed, ok, err)
	}
	if err := database.CompleteJob(ctx, event.ID, claimed.FencingToken); err != nil {
		t.Fatalf("complete terminal job: %v", err)
	}
	settings.Retention.CompletedJobs = time.Hour
	processorNow := time.Now().UTC().Add(48 * time.Hour)
	result, err := (Processor{Config: settings, Store: database, Now: func() time.Time { return processorNow }}).Drain(ctx, 2)
	if err != nil {
		t.Fatalf("drain terminal recovery: %v", err)
	}
	if result.Captured != 0 || result.Excluded != 0 || result.Failed != 0 || result.PrunedCompletedJobs != 1 {
		t.Fatalf("terminal recovery result=%#v", result)
	}
	if _, err := database.Job(ctx, event.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("terminal tombstone not pruned after reconciliation: %v", err)
	}
	for _, directory := range []string{directories.incoming, directories.processing, directories.failed} {
		entries, err := os.ReadDir(directory)
		if err != nil || len(entries) != 0 {
			t.Fatalf("replayed terminal spool in %s: entries=%v err=%v", directory, entries, err)
		}
	}
}

func TestDrainDefersTerminalPruningUntilBoundedRecoveryReachesEOF(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(dataDir, "skillloop.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	directories, err := ensureSpoolDirectories(dataDir)
	if err != nil {
		t.Fatalf("ensure spool: %v", err)
	}
	t.Cleanup(directories.close)

	const limit = 2
	jobIDs := make([]string, 0, limit+1)
	for index := range limit + 1 {
		eventID := fmt.Sprintf("terminal-budget-%d", index)
		event := domain.HookEvent{
			ID: eventID, Source: domain.SourceCodex, SessionID: "session-" + eventID,
			WorkingDir: dataDir, HookEventName: "stop", CapturedAt: time.Now(),
		}
		incomingPath, err := (capture.Spool{DataDir: dataDir}).Write(event)
		if err != nil {
			t.Fatalf("write terminal event: %v", err)
		}
		if err := os.Rename(incomingPath, filepath.Join(directories.processing, eventID+".json")); err != nil {
			t.Fatalf("stage terminal event: %v", err)
		}
		job := domain.Job{
			ID: eventID, Kind: ingestJobKind, IdempotencyKey: "hook:" + eventID,
			Payload: eventID, Status: domain.JobQueued, AvailableAt: time.Now(),
		}
		if created, err := database.EnqueueJob(ctx, job); err != nil || !created {
			t.Fatalf("enqueue terminal job: created=%v err=%v", created, err)
		}
		claimed, ok, err := database.ClaimJob(ctx, eventID, time.Minute)
		if err != nil || !ok {
			t.Fatalf("claim terminal job: job=%#v ok=%v err=%v", claimed, ok, err)
		}
		if err := database.CompleteJob(ctx, eventID, claimed.FencingToken); err != nil {
			t.Fatalf("complete terminal job: %v", err)
		}
		jobIDs = append(jobIDs, eventID)
	}
	settings, err := config.Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	settings.DataDir = dataDir
	settings.Retention.CompletedJobs = time.Hour
	processorNow := time.Now().UTC().Add(48 * time.Hour)
	processor := Processor{Config: settings, Store: database, Now: func() time.Time { return processorNow }}
	first, err := processor.Drain(ctx, limit)
	if err != nil {
		t.Fatalf("first bounded recovery: %v", err)
	}
	if first.ProcessingRecoveryComplete || first.PrunedCompletedJobs != 0 {
		t.Fatalf("incomplete recovery pruned tombstones: %#v", first)
	}
	for _, jobID := range jobIDs {
		if _, err := database.Job(ctx, jobID); err != nil {
			t.Fatalf("tombstone %s pruned before recovery EOF: %v", jobID, err)
		}
	}
	second, err := processor.Drain(ctx, limit)
	if err != nil {
		t.Fatalf("second bounded recovery: %v", err)
	}
	if !second.ProcessingRecoveryComplete || second.PrunedCompletedJobs != len(jobIDs) {
		t.Fatalf("completed recovery result=%#v", second)
	}
	for _, jobID := range jobIDs {
		if _, err := database.Job(ctx, jobID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("terminal job %s retained after recovery EOF: %v", jobID, err)
		}
	}
}

func TestDrainRejectsHookIdentityAndCWDThatDisagreeWithTranscript(t *testing.T) {
	for _, test := range []struct {
		name      string
		hookID    string
		nativeID  string
		hookCWD   string
		nativeCWD string
	}{
		{name: "session replay", hookID: "replayed", nativeID: "native", hookCWD: "project", nativeCWD: "project"},
		{name: "masked loop cwd", hookID: "native", nativeID: "native", hookCWD: "project", nativeCWD: "self"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			dataDir := t.TempDir()
			database, err := store.Open(ctx, filepath.Join(dataDir, "skillloop.db"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = database.Close() })
			hookCWD := filepath.Join(dataDir, test.hookCWD)
			nativeCWD := filepath.Join(dataDir, test.nativeCWD)
			for _, path := range []string{hookCWD, nativeCWD} {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatalf("create cwd: %v", err)
				}
			}
			if test.nativeCWD == "self" {
				if err := os.WriteFile(filepath.Join(nativeCWD, ".skillloop-ignore"), nil, 0o600); err != nil {
					t.Fatalf("write loop marker: %v", err)
				}
			}
			transcriptPath := writeDaemonTranscript(t, dataDir, domain.SourceCodex, test.nativeID, nativeCWD, "{}\n")
			if _, err := (capture.Spool{DataDir: dataDir}).Write(domain.HookEvent{
				ID: "identity-event", Source: domain.SourceCodex, SessionID: test.hookID,
				WorkingDir: hookCWD, TranscriptPath: transcriptPath, HookEventName: "stop", CapturedAt: time.Now(),
			}); err != nil {
				t.Fatalf("write replay event: %v", err)
			}
			settings, err := config.Default()
			if err != nil {
				t.Fatalf("default config: %v", err)
			}
			settings.DataDir = dataDir
			result, err := (Processor{Config: settings, Store: database}).Drain(ctx, 1)
			if err != nil {
				t.Fatalf("drain replay event: %v", err)
			}
			if result.Captured != 1 || result.Failed != 1 || result.Excluded != 0 || result.Processed != 0 {
				t.Fatalf("identity replay result=%#v", result)
			}
			counts, err := database.Counts(ctx, settings.Aggregation.MinimumSessions)
			if err != nil || counts.Sessions != 0 {
				t.Fatalf("identity replay reached sessions: counts=%#v err=%v", counts, err)
			}
		})
	}
}

func TestDrainFailureCollisionPublishesUniqueQuarantine(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(dataDir, "skillloop.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	directories, err := ensureSpoolDirectories(dataDir)
	if err != nil {
		t.Fatalf("ensure spool: %v", err)
	}
	t.Cleanup(directories.close)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	existing := filepath.Join(directories.failed, fmt.Sprintf("bad.failed-%016x-%024x.json", now.UnixNano(), 0))
	if err := os.WriteFile(existing, []byte("existing quarantine"), 0o600); err != nil {
		t.Fatalf("write collision fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directories.incoming, "bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("write poison: %v", err)
	}
	settings, err := config.Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	settings.DataDir = dataDir
	result, err := (Processor{Config: settings, Store: database, Now: func() time.Time { return now }}).Drain(ctx, 1)
	if err != nil {
		t.Fatalf("drain collision: %v", err)
	}
	if result.Failed != 1 {
		t.Fatalf("collision result = %#v", result)
	}
	contents, err := os.ReadFile(existing)
	if err != nil || string(contents) != "existing quarantine" {
		t.Fatalf("existing quarantine overwritten: contents=%q err=%v", contents, err)
	}
	matches, err := filepath.Glob(filepath.Join(directories.failed, "bad.failed-*.json"))
	if err != nil || len(matches) != 2 {
		t.Fatalf("unique quarantine matches=%v err=%v", matches, err)
	}
}

func TestRecoveryCollisionDoesNotReplaceIncomingEvent(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(dataDir, "skillloop.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	excludedDir := filepath.Join(dataDir, "excluded")
	if err := os.Mkdir(excludedDir, 0o700); err != nil {
		t.Fatalf("mkdir excluded: %v", err)
	}
	if err := os.WriteFile(filepath.Join(excludedDir, ".skillloop-ignore"), nil, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	transcriptPath := writeDaemonTranscript(t, dataDir, domain.SourceCodex, "incoming", excludedDir, "{}\n")
	incomingPath, err := (capture.Spool{DataDir: dataDir}).Write(domain.HookEvent{
		ID: "duplicate", Source: domain.SourceCodex, SessionID: "incoming",
		WorkingDir: excludedDir, TranscriptPath: transcriptPath, HookEventName: "stop", Reason: "incoming", CapturedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("write incoming: %v", err)
	}
	incomingContents, err := os.ReadFile(incomingPath)
	if err != nil {
		t.Fatalf("read incoming: %v", err)
	}
	directories, err := ensureSpoolDirectories(dataDir)
	if err != nil {
		t.Fatalf("ensure spool: %v", err)
	}
	t.Cleanup(directories.close)
	processingContents := strings.Replace(string(incomingContents), `"incoming"`, `"recovered"`, 1)
	if err := os.WriteFile(filepath.Join(directories.processing, "duplicate.json"), []byte(processingContents), 0o600); err != nil {
		t.Fatalf("write processing collision: %v", err)
	}
	settings, err := config.Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	settings.DataDir = dataDir
	result, err := (Processor{Config: settings, Store: database}).Drain(ctx, 1)
	if err != nil {
		t.Fatalf("drain recovery collision: %v", err)
	}
	if result.Failed != 1 || result.Excluded != 1 {
		t.Fatalf("recovery collision result = %#v", result)
	}
	quarantinedPaths, err := filepath.Glob(filepath.Join(directories.failed, "duplicate.failed-*.json"))
	if err != nil || len(quarantinedPaths) != 1 {
		t.Fatalf("recovered quarantine paths=%v err=%v", quarantinedPaths, err)
	}
	quarantined, err := os.ReadFile(quarantinedPaths[0])
	if err != nil || !strings.Contains(string(quarantined), "recovered") {
		t.Fatalf("recovered copy not quarantined: contents=%q err=%v", quarantined, err)
	}
}

func TestDaemonRefusesSymlinkedSpoolDirectory(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dataDir, "spool")); err != nil {
		t.Fatalf("symlink spool: %v", err)
	}
	database, err := store.Open(ctx, filepath.Join(root, "skillloop.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	settings, err := config.Default()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	settings.DataDir = dataDir
	if _, err := (Processor{Config: settings, Store: database}).Drain(ctx, 1); err == nil {
		t.Fatal("daemon accepted symlinked spool directory")
	}
	info, err := os.Stat(outside)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("outside permissions changed: info=%v err=%v", info, err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("daemon wrote through symlink: entries=%v err=%v", entries, err)
	}
}

func TestSpoolIdentityCheckRejectsReplacement(t *testing.T) {
	dataDir := t.TempDir()
	directories, err := ensureSpoolDirectories(dataDir)
	if err != nil {
		t.Fatalf("ensure spool: %v", err)
	}
	t.Cleanup(directories.close)
	name := "race.json"
	path := filepath.Join(directories.incoming, name)
	if err := os.WriteFile(path, []byte(`{"event_id":"race"}`), 0o600); err != nil {
		t.Fatalf("write original: %v", err)
	}
	file, info, err := openSpoolEntry(directories.incomingFD, name)
	if err != nil {
		t.Fatalf("open original: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if err := os.Rename(path, filepath.Join(directories.incoming, "old.json")); err != nil {
		t.Fatalf("rename original: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"event_id":"replacement"}`), 0o600); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	if _, err := readSpoolEntry(file, directories.incomingFD, name, info); err == nil {
		t.Fatal("replacement passed stable identity check")
	}
	if err := removeEntry(directories.incomingFD, name, info); err == nil {
		t.Fatal("stale identity removed replacement")
	}
	if contents, err := os.ReadFile(path); err != nil || !strings.Contains(string(contents), "replacement") {
		t.Fatalf("replacement was removed: contents=%q err=%v", contents, err)
	}
}

func TestStaleWorkerCannotQuarantineReclaimedSpoolEntry(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(dataDir, "skillloop.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	directories, err := ensureSpoolDirectories(dataDir)
	if err != nil {
		t.Fatalf("ensure spool: %v", err)
	}
	t.Cleanup(directories.close)
	job := domain.Job{ID: "reclaimed", Kind: ingestJobKind, IdempotencyKey: "hook:reclaimed", Payload: "reclaimed"}
	if created, err := database.EnqueueJob(ctx, job); err != nil || !created {
		t.Fatalf("enqueue job: created=%v err=%v", created, err)
	}
	stale, ok, err := database.ClaimJob(ctx, job.ID, time.Nanosecond)
	if err != nil || !ok {
		t.Fatalf("claim stale lease: job=%#v ok=%v err=%v", stale, ok, err)
	}
	for !time.Now().After(stale.LeasedUntil) {
		time.Sleep(time.Millisecond)
	}
	current, ok, err := database.ClaimJob(ctx, job.ID, time.Hour)
	if err != nil || !ok {
		t.Fatalf("reclaim job: job=%#v ok=%v err=%v", current, ok, err)
	}
	name := job.ID + ".json"
	processingPath := filepath.Join(directories.processing, name)
	if err := os.WriteFile(processingPath, []byte(`{"event_id":"reclaimed"}`), 0o600); err != nil {
		t.Fatalf("write processing entry: %v", err)
	}
	file, info, err := openSpoolEntry(directories.processingFD, name)
	if err != nil {
		t.Fatalf("open processing entry: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })
	processor := Processor{Store: database, Now: time.Now}
	if _, err := processor.failClaimedEntry(ctx, directories, name, info, job.ID, stale.FencingToken, errors.New("stale failure")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale failure error = %v, want lost lease", err)
	}
	currentInfo, err := os.Stat(processingPath)
	if err != nil || !os.SameFile(info, currentInfo) {
		t.Fatalf("stale worker moved current inode: same=%v err=%v", err == nil && os.SameFile(info, currentInfo), err)
	}
	staleMatches, err := filepath.Glob(filepath.Join(directories.failed, "reclaimed.failed-*.json"))
	if err != nil || len(staleMatches) != 0 {
		t.Fatalf("stale worker published quarantine: matches=%v err=%v", staleMatches, err)
	}
	persisted, err := database.Job(ctx, job.ID)
	if err != nil || persisted.Status != domain.JobProcessing || persisted.FencingToken != current.FencingToken {
		t.Fatalf("stale worker changed current job: job=%#v err=%v", persisted, err)
	}
	if _, err := processor.failClaimedEntry(ctx, directories, name, info, job.ID, current.FencingToken, errors.New("current failure")); err == nil {
		t.Fatal("current failure should remain explicit")
	}
	matches, err := filepath.Glob(filepath.Join(directories.failed, "reclaimed.failed-*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("current owner quarantine matches=%v err=%v", matches, err)
	}
}

func writeDaemonTranscript(
	t *testing.T,
	dataDir string,
	source domain.Source,
	sessionID string,
	workingDir string,
	body string,
) string {
	t.Helper()
	var transcriptRoot string
	switch source {
	case domain.SourceCodex:
		providerHome := filepath.Join(dataDir, "codex-home")
		t.Setenv("CODEX_HOME", providerHome)
		transcriptRoot = filepath.Join(providerHome, "sessions")
	case domain.SourceClaude:
		providerHome := filepath.Join(dataDir, "claude-home")
		t.Setenv("CLAUDE_CONFIG_DIR", providerHome)
		transcriptRoot = filepath.Join(providerHome, "projects")
	default:
		t.Fatalf("unsupported transcript source %q", source)
	}
	if err := os.MkdirAll(transcriptRoot, 0o700); err != nil {
		t.Fatalf("create transcript root: %v", err)
	}
	path := filepath.Join(transcriptRoot, sessionID+".jsonl")
	var contents string
	if source == domain.SourceCodex {
		contents = codexTranscriptContents(sessionID, workingDir, body)
	} else {
		contents = fmt.Sprintf("{\"type\":\"system\",\"sessionId\":%q,\"cwd\":%q}\n%s", sessionID, workingDir, body)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

func codexTranscriptContents(sessionID, workingDir, body string) string {
	return fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cwd\":%q}}\n%s", sessionID, workingDir, body)
}

func TestExcludedPathContainmentHandlesRootsAndMissingSymlinkChildren(t *testing.T) {
	root := t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	realWorkspace := filepath.Join(canonicalRoot, "workspace")
	if err := os.Mkdir(realWorkspace, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(canonicalRoot, "workspace-alias")
	if err := os.Symlink(realWorkspace, alias); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name       string
		workingDir string
		configured []string
		want       bool
	}{
		{
			name:       "filesystem root contains descendants",
			workingDir: realWorkspace,
			configured: []string{string(filepath.Separator)},
			want:       true,
		},
		{
			name:       "missing child below symlink alias",
			workingDir: filepath.Join(alias, "removed", "child"),
			configured: []string{realWorkspace},
			want:       true,
		},
		{
			name:       "existing symlink alias",
			workingDir: alias,
			configured: []string{realWorkspace},
			want:       true,
		},
		{
			name:       "sibling remains processable",
			workingDir: filepath.Join(canonicalRoot, "workspace-other"),
			configured: []string{realWorkspace},
			want:       false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := excluded(test.workingDir, test.configured); got != test.want {
				t.Fatalf("excluded(%q, %#v)=%v, want %v", test.workingDir, test.configured, got, test.want)
			}
		})
	}
}

func TestExcludedPathResolutionFailsClosed(t *testing.T) {
	root := t.TempDir()
	loop := filepath.Join(root, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}

	if !excluded(loop, nil) {
		t.Fatal("unresolvable working directory failed open")
	}
	if !excluded(t.TempDir(), []string{loop}) {
		t.Fatal("unresolvable configured exclusion failed open")
	}
}
