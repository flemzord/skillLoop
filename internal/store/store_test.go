package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flemzord/skillloop/internal/domain"
	"github.com/flemzord/skillloop/internal/learning"
)

func TestOpenConfiguresSQLiteAndConstraints(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	var journalMode string
	if err := store.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("read journal mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal mode = %q, want wal", journalMode)
	}

	var foreignKeys int
	if err := store.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatalf("read foreign key setting: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign keys = %d, want 1", foreignKeys)
	}

	var busyTimeout int
	if err := store.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("read busy timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy timeout = %d, want 5000", busyTimeout)
	}

	_, err := store.db.ExecContext(ctx, `
		INSERT INTO learning_cards (
			id, session_ref, skill_id, kind, fingerprint, summary, lesson, confidence, created_at
		) VALUES ('card', 'missing-session', 'missing-skill', 'failure', 'fp', '', '', 1, 1)`)
	if err == nil {
		t.Fatal("foreign key violation unexpectedly succeeded")
	}
	_, err = store.db.ExecContext(ctx, `
		INSERT INTO sessions (
			reference, source, external_id, outcome, created_at, updated_at
		) VALUES ('invalid-outcome', 'codex', 'invalid-outcome', 'partial', 1, 1)`)
	if err == nil {
		t.Fatal("invalid session outcome unexpectedly satisfied the SQLite constraint")
	}

	var version int
	if err := store.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", version, schemaVersion)
	}
}

func TestOpenMigratesSchemaV1ProposalSafetyMetadataFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
		CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL);
		` + schemaV1 + `
		INSERT INTO skills (
			id, name, repository_path, instruction_path, enabled, created_at
		) VALUES ('legacy-skill', 'Legacy', '/tmp/legacy', 'SKILL.md', 1, 1);
		INSERT INTO clusters (
			id, skill_id, kind, fingerprint, summary, lesson,
			card_count, session_count, status, updated_at
		) VALUES (
			'legacy-cluster', 'legacy-skill', 'validation', 'legacy-fingerprint',
			'old summary', 'old mutable lesson', 3, 3, 'proposed', 1
		);
		INSERT INTO proposals (
			id, cluster_id, skill_id, status, repository_path, worktree_path,
			branch, base_commit, candidate_commit, baseline_score, candidate_score,
			previous_commit, promoted_commit, created_at, updated_at
		) VALUES (
			'legacy-proposal', 'legacy-cluster', 'legacy-skill', 'pending', '/tmp/legacy',
			'/tmp/worktree', 'skillloop/legacy', 'base-commit', 'candidate-commit',
			0, 0, '', '', 1, 1
		);
		INSERT INTO schema_migrations (version, applied_at) VALUES (1, 1);`); err != nil {
		_ = database.Close()
		t.Fatalf("seed schema v1: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open legacy schema: %v", err)
	}
	defer func() { _ = migrated.Close() }()
	var version int
	if err := migrated.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version=%d, want %d", version, schemaVersion)
	}
	var requiresHuman int
	if err := migrated.db.QueryRow(`SELECT dflt_value FROM pragma_table_info('proposals') WHERE name = 'requires_human_approval'`).Scan(&requiresHuman); err != nil {
		t.Fatalf("read migrated safety default: %v", err)
	}
	if requiresHuman != 1 {
		t.Fatalf("legacy proposal safety default=%d, want fail-closed 1", requiresHuman)
	}
	legacy, err := migrated.Proposal(context.Background(), "legacy-proposal")
	if err != nil {
		t.Fatalf("read migrated legacy proposal: %v", err)
	}
	if legacy.Fingerprint != "" || legacy.Lesson != "" || legacy.CardKind != "" ||
		!legacy.RequiresHumanApproval || legacy.BaseCommit != "base-commit" || legacy.CandidateCommit != "candidate-commit" {
		t.Fatalf("migrated legacy proposal lost fail-closed compatibility state: %#v", legacy)
	}
}

func TestSessionAndLearningCardInsertsAreIdempotent(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	skill := testSkill("go")

	created, err := store.RegisterSkill(ctx, skill)
	if err != nil || !created {
		t.Fatalf("register skill: created=%v err=%v", created, err)
	}
	created, err = store.RegisterSkill(ctx, skill)
	if err != nil || created {
		t.Fatalf("register duplicate skill: created=%v err=%v", created, err)
	}

	session := testSession("session-1")
	created, err = store.RecordSession(ctx, session)
	if err != nil || !created {
		t.Fatalf("record session: created=%v err=%v", created, err)
	}
	session.TranscriptPath = "/tmp/session-1-updated.jsonl"
	created, err = store.RecordSession(ctx, session)
	if err != nil || created {
		t.Fatalf("record duplicate session: created=%v err=%v", created, err)
	}

	card := testCard("card-1", session.Reference, skill.ID, "same-friction")
	created, err = store.AddLearningCard(ctx, card)
	if err != nil || !created {
		t.Fatalf("add learning card: created=%v err=%v", created, err)
	}
	card.ID = "card-replayed-with-another-id"
	created, err = store.AddLearningCard(ctx, card)
	if err != nil || created {
		t.Fatalf("replay learning card: created=%v err=%v", created, err)
	}

	skills, err := store.ListSkills(ctx)
	if err != nil {
		t.Fatalf("list skills: %v", err)
	}
	if len(skills) != 1 || skills[0].ID != skill.ID {
		t.Fatalf("skills = %#v, want one registered skill", skills)
	}
	cards, err := store.ListLearningCards(ctx, skill.ID)
	if err != nil {
		t.Fatalf("list cards: %v", err)
	}
	if len(cards) != 1 || cards[0].ID != "card-1" {
		t.Fatalf("cards = %#v, want the first insertion only", cards)
	}

	counts, err := store.Counts(ctx, 3)
	if err != nil {
		t.Fatalf("read counts: %v", err)
	}
	if counts.Skills != 1 || counts.Sessions != 1 || counts.Cards != 1 {
		t.Fatalf("counts = %#v, want one skill/session/card", counts)
	}
}

func TestAddLearningCardRedactsLowerTrustFieldsWithoutChangingQuorumIdentity(t *testing.T) {
	database := openTestStore(t)
	ctx := context.Background()
	skill := testSkill("durable-redaction")
	mustRegisterSkill(t, database, skill)

	const (
		fingerprint = "failure stable-direct-boundary lesson-stable"
		jwt         = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJkaXJlY3QtaW5zZXJ0In0.c2lnbmF0dXJlX3ZhbHVl"
		gitlabToken = "glpat-abcdefghijklmnopqrstuvwx"
		privateKey  = "-----BEGIN PRIVATE KEY-----\nZmFrZS1wcml2YXRlLWtleQ==\n-----END PRIVATE KEY-----"
	)
	for index := range 3 {
		session := testSession(fmt.Sprintf("direct-redaction-%d", index))
		mustRecordSession(t, database, session)
		card := testCard(fmt.Sprintf("direct-card-%d", index), session.Reference, skill.ID, fingerprint)
		card.Summary = "Observed credential " + jwt
		card.Lesson = "Never persist " + privateKey + " or " + gitlabToken
		created, err := database.AddLearningCard(ctx, card)
		if err != nil || !created {
			t.Fatalf("direct add %d: created=%v err=%v", index, created, err)
		}
	}

	cards, err := database.ListLearningCards(ctx, skill.ID)
	if err != nil || len(cards) != 3 {
		t.Fatalf("cards=%#v err=%v", cards, err)
	}
	for _, card := range cards {
		if card.Fingerprint != fingerprint {
			t.Fatalf("durable sanitization changed fingerprint: %q", card.Fingerprint)
		}
		persisted := card.Summary + " " + card.Lesson
		for _, forbidden := range []string{jwt, gitlabToken, "BEGIN PRIVATE KEY", "ZmFrZS1wcml2YXRlLWtleQ"} {
			if strings.Contains(persisted, forbidden) {
				t.Fatalf("secret %q survived direct persistence: %q", forbidden, persisted)
			}
		}
		if !strings.Contains(card.Summary, "[REDACTED_SECRET]") || !strings.Contains(card.Lesson, "[REDACTED_SECRET]") {
			t.Fatalf("card was not redacted at the durable boundary: %#v", card)
		}
	}

	clusters, err := database.RebuildClusters(ctx, 3)
	if err != nil || len(clusters) != 1 || clusters[0].Fingerprint != fingerprint || clusters[0].SessionCount != 3 {
		t.Fatalf("sanitized cards lost quorum identity: clusters=%#v err=%v", clusters, err)
	}
}

func TestAddLearningCardRejectsSecretFingerprint(t *testing.T) {
	database := openTestStore(t)
	ctx := context.Background()
	skill := testSkill("secret-fingerprint")
	mustRegisterSkill(t, database, skill)
	session := testSession("secret-fingerprint")
	mustRecordSession(t, database, session)

	card := testCard("secret-fingerprint", session.Reference, skill.ID,
		"failure github_pat_11AA22bb33CC44dd55EE66ff77GG88hh99II00jj")
	if created, err := database.AddLearningCard(ctx, card); err == nil || created {
		t.Fatalf("secret fingerprint accepted: created=%v err=%v", created, err)
	}
	if cards, err := database.ListLearningCards(ctx, skill.ID); err != nil || len(cards) != 0 {
		t.Fatalf("secret fingerprint reached durable storage: cards=%#v err=%v", cards, err)
	}
}

func TestRecordSessionPersistsAndEnrichesOutcome(t *testing.T) {
	database := openTestStore(t)
	ctx := context.Background()
	session := testSession("outcome")
	session.Outcome = domain.SessionOutcomeUnknown
	if created, err := database.RecordSession(ctx, session); err != nil || !created {
		t.Fatalf("record unknown session: created=%v err=%v", created, err)
	}
	assertOutcome := func(want domain.SessionOutcome) {
		t.Helper()
		var got domain.SessionOutcome
		if err := database.db.QueryRowContext(ctx, `SELECT outcome FROM sessions WHERE reference = ?`, session.Reference).Scan(&got); err != nil {
			t.Fatalf("read outcome: %v", err)
		}
		if got != want {
			t.Fatalf("outcome = %q, want %q", got, want)
		}
	}
	assertOutcome(domain.SessionOutcomeUnknown)

	session.Outcome = domain.SessionOutcomeFailed
	if created, err := database.RecordSession(ctx, session); err != nil || created {
		t.Fatalf("enrich session outcome: created=%v err=%v", created, err)
	}
	assertOutcome(domain.SessionOutcomeFailed)

	session.Outcome = domain.SessionOutcomeUnknown
	if created, err := database.RecordSession(ctx, session); err != nil || created {
		t.Fatalf("replay unknown session outcome: created=%v err=%v", created, err)
	}
	assertOutcome(domain.SessionOutcomeFailed)

	session.Outcome = domain.SessionOutcome("invalid")
	if _, err := database.RecordSession(ctx, session); err == nil {
		t.Fatal("expected invalid outcome error")
	}
}

func TestPruneRetentionKeepsDurableLearningAndRecentOperationalData(t *testing.T) {
	database := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	recent := now.Add(-time.Hour)

	skill := testSkill("retention")
	mustRegisterSkill(t, database, skill)
	database.now = func() time.Time { return old }
	oldSession := testSession("old-retention")
	mustRecordSession(t, database, oldSession)
	mustAddCard(t, database, testCard("durable-card", oldSession.Reference, skill.ID, "durable"))
	completedOld := enqueueTerminalJob(t, database, "completed-old", false)
	failedOld := enqueueTerminalJob(t, database, "failed-old", true)

	database.now = func() time.Time { return recent }
	recentSession := testSession("recent-retention")
	mustRecordSession(t, database, recentSession)
	completedRecent := enqueueTerminalJob(t, database, "completed-recent", false)

	pruned, err := database.PruneRetention(ctx, now, 24*time.Hour, 24*time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatalf("prune retention: %v", err)
	}
	if pruned != (RetentionPruneResult{TranscriptLocators: 1, CompletedJobs: 1, FailedJobs: 1}) {
		t.Fatalf("pruned = %#v", pruned)
	}
	var oldPath, recentPath string
	if err := database.db.QueryRowContext(ctx, `SELECT transcript_path FROM sessions WHERE reference = ?`, oldSession.Reference).Scan(&oldPath); err != nil {
		t.Fatalf("read old locator: %v", err)
	}
	if err := database.db.QueryRowContext(ctx, `SELECT transcript_path FROM sessions WHERE reference = ?`, recentSession.Reference).Scan(&recentPath); err != nil {
		t.Fatalf("read recent locator: %v", err)
	}
	if oldPath != "" || recentPath != recentSession.TranscriptPath {
		t.Fatalf("locators old=%q recent=%q", oldPath, recentPath)
	}
	for _, id := range []string{completedOld, failedOld} {
		var count int
		if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE id = ?`, id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("old job %s count=%d err=%v", id, count, err)
		}
	}
	var recentCount int
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE id = ?`, completedRecent).Scan(&recentCount); err != nil || recentCount != 1 {
		t.Fatalf("recent job count=%d err=%v", recentCount, err)
	}
	cards, err := database.ListLearningCards(ctx, skill.ID)
	if err != nil || len(cards) != 1 || cards[0].ID != "durable-card" {
		t.Fatalf("durable cards=%#v err=%v", cards, err)
	}
}

func TestPruneRetentionZeroKeepsOperationalDataIndefinitely(t *testing.T) {
	database := openTestStore(t)
	ctx := context.Background()
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	database.now = func() time.Time { return old }
	session := testSession("indefinite-retention")
	mustRecordSession(t, database, session)
	jobID := enqueueTerminalJob(t, database, "indefinite-job", false)

	pruned, err := database.PruneRetention(ctx, old.Add(10*365*24*time.Hour), 0, 0, 0)
	if err != nil {
		t.Fatalf("prune indefinite retention: %v", err)
	}
	if pruned != (RetentionPruneResult{}) {
		t.Fatalf("pruned = %#v, want zero", pruned)
	}
	var path string
	if err := database.db.QueryRowContext(ctx, `SELECT transcript_path FROM sessions WHERE reference = ?`, session.Reference).Scan(&path); err != nil || path != session.TranscriptPath {
		t.Fatalf("locator=%q err=%v", path, err)
	}
	var jobCount int
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE id = ?`, jobID).Scan(&jobCount); err != nil || jobCount != 1 {
		t.Fatalf("job count=%d err=%v", jobCount, err)
	}
}

func TestRegisterSkillScopesInstructionPathToRepository(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	first := domain.Skill{
		ID:              "skill-first",
		Name:            "First",
		RepositoryPath:  "/tmp/skills/first",
		InstructionPath: "SKILL.md",
		Enabled:         true,
	}
	second := domain.Skill{
		ID:              "skill-second",
		Name:            "Second",
		RepositoryPath:  "/tmp/skills/second",
		InstructionPath: "SKILL.md",
		Enabled:         true,
	}
	for _, skill := range []domain.Skill{first, second} {
		created, err := store.RegisterSkill(ctx, skill)
		if err != nil || !created {
			t.Fatalf("register %s: created=%v err=%v", skill.ID, created, err)
		}
	}

	duplicateIdentity := first
	duplicateIdentity.ID = "skill-first-duplicate"
	duplicateIdentity.Name = "First duplicate"
	if created, err := store.RegisterSkill(ctx, duplicateIdentity); err == nil || created {
		t.Fatalf("register duplicate repository/instruction identity: created=%v err=%v", created, err)
	}

	skills, err := store.ListSkills(ctx)
	if err != nil {
		t.Fatalf("list skills: %v", err)
	}
	if len(skills) != 2 {
		t.Fatalf("skills = %#v, want the two distinct repositories", skills)
	}
}

func TestRebuildClustersCountsDistinctSessionsAndPreservesStatus(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	skill := testSkill("go")
	mustRegisterSkill(t, store, skill)

	for index := 1; index <= 3; index++ {
		session := testSession(fmt.Sprintf("session-%d", index))
		mustRecordSession(t, store, session)
		card := testCard(fmt.Sprintf("card-%d", index), session.Reference, skill.ID, "linker-resolv")
		card.Summary = fmt.Sprintf("observation %d", index)
		card.CreatedAt = time.Date(2026, 8, 20, 10, index, 0, 0, time.UTC)
		mustAddCard(t, store, card)
	}

	clusters, err := store.RebuildClusters(ctx, 3)
	if err != nil {
		t.Fatalf("rebuild clusters: %v", err)
	}
	if len(clusters) != 1 {
		t.Fatalf("clusters = %#v, want one eligible cluster", clusters)
	}
	cluster := clusters[0]
	if cluster.CardCount != 3 || cluster.SessionCount != 3 {
		t.Fatalf("cluster counts = cards:%d sessions:%d, want 3/3", cluster.CardCount, cluster.SessionCount)
	}
	if cluster.Summary != "observation 3" {
		t.Fatalf("cluster summary = %q, want latest representative", cluster.Summary)
	}
	clusterID := cluster.ID

	if err := store.UpdateClusterStatus(ctx, cluster.ID, domain.ClusterProposed); err != nil {
		t.Fatalf("mark cluster proposed: %v", err)
	}
	clusters, err = store.RebuildClusters(ctx, 3)
	if err != nil {
		t.Fatalf("rebuild clusters again: %v", err)
	}
	if len(clusters) != 1 || clusters[0].ID != clusterID || clusters[0].Status != domain.ClusterProposed {
		t.Fatalf("rebuilt cluster = %#v, want stable id and proposed status", clusters)
	}
}

func TestRebuildClustersRequiresConcordantRecoveryLessons(t *testing.T) {
	database := openTestStore(t)
	ctx := context.Background()
	skill := domain.Skill{
		ID: "skill-quorum", Name: "quorum", RepositoryPath: "/skills/quorum",
		InstructionPath: "SKILL.md", Enabled: true,
	}
	mustRegisterSkill(t, database, skill)
	recoveries := []string{
		`{"command":"nix develop --command go test ./..."}`,
		`{"command":"nix develop --command go test ./..."}`,
		`{"command":"nix develop --command go test ./..."}`,
		`{"command":"go test -run Hostile ./..."}`,
	}

	var concordantFingerprint string
	for index, recovery := range recoveries {
		session := testSession(fmt.Sprintf("quorum-session-%d", index))
		session.Messages = []domain.Message{
			{Role: "tool", ToolName: "exec_command", ToolCallID: "read", Text: `{"cmd":"sed -n '1,240p' /skills/quorum/SKILL.md"}`},
			{Role: "tool", ToolName: "exec_command", ToolCallID: "read", ToolResult: true, Text: "skill instructions"},
			{Role: "tool", ToolName: "exec_command", ToolCallID: "failed", Text: `{"cmd":"go test ./..."}`},
			{Role: "tool", ToolName: "exec_command", ToolCallID: "failed", ToolResult: true, Failed: true, Text: "exit code 1"},
			{Role: "tool", ToolName: "exec_command", ToolCallID: "recovery", Text: strings.Replace(recovery, `"command"`, `"cmd"`, 1)},
			{Role: "tool", ToolName: "exec_command", ToolCallID: "recovery", ToolResult: true, Text: "ok"},
		}
		mustRecordSession(t, database, session)
		cards := learning.NewAnalyzer().Analyze(session, []domain.Skill{skill})
		var failure domain.LearningCard
		for _, card := range cards {
			if card.Kind == domain.CardFailure {
				failure = card
				break
			}
		}
		if failure.ID == "" {
			t.Fatalf("session %d produced no failure card: %#v", index, cards)
		}
		switch {
		case index == 0:
			concordantFingerprint = failure.Fingerprint
		case index < 3 && failure.Fingerprint != concordantFingerprint:
			t.Fatalf("concordant session %d split to %q", index, failure.Fingerprint)
		case index == 3 && failure.Fingerprint == concordantFingerprint:
			t.Fatalf("hostile recovery inherited fingerprint %q", failure.Fingerprint)
		}
		mustAddCard(t, database, failure)
	}

	clusters, err := database.RebuildClusters(ctx, 3)
	if err != nil {
		t.Fatalf("rebuild clusters: %v", err)
	}
	if len(clusters) != 1 || clusters[0].SessionCount != 3 || clusters[0].CardCount != 3 {
		t.Fatalf("eligible clusters = %#v, want only the three concordant sessions", clusters)
	}
	if !strings.Contains(clusters[0].Lesson, "nix develop --command go test") ||
		strings.Contains(clusters[0].Lesson, "Hostile") {
		t.Fatalf("cluster inherited a disagreeing recovery: %#v", clusters[0])
	}
}

func TestJobsAreIdempotentAndLeased(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	job := domain.Job{
		ID:             "job-1",
		Kind:           "analyze-session",
		IdempotencyKey: "session:codex:123",
		Payload:        `{"session":"123"}`,
	}
	created, err := store.EnqueueJob(ctx, job)
	if err != nil || !created {
		t.Fatalf("enqueue job: created=%v err=%v", created, err)
	}
	job.ID = "job-replayed"
	created, err = store.EnqueueJob(ctx, job)
	if err != nil || created {
		t.Fatalf("enqueue duplicate job: created=%v err=%v", created, err)
	}
	persisted, err := store.Job(ctx, "job-1")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if persisted.ID != "job-1" || persisted.Status != domain.JobQueued || persisted.IdempotencyKey != job.IdempotencyKey {
		t.Fatalf("unexpected persisted job: %#v", persisted)
	}
	if _, err := store.Job(ctx, "missing-job"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing job error = %v, want ErrNotFound", err)
	}

	jobs, err := store.ClaimJobs(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claim jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != "job-1" || jobs[0].Attempts != 1 || jobs[0].Status != domain.JobProcessing {
		t.Fatalf("claimed jobs = %#v", jobs)
	}
	if err := store.CompleteJob(ctx, jobs[0].ID); err != nil {
		t.Fatalf("complete job: %v", err)
	}
	jobs, err = store.ClaimJobs(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claim completed jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("claimed completed jobs = %#v, want none", jobs)
	}
}

func TestClaimJobTargetsOneIDAndPreservesStrictTransitions(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	for _, id := range []string{"older-job", "current-job"} {
		created, err := store.EnqueueJob(ctx, domain.Job{
			ID:             id,
			Kind:           "ingest",
			IdempotencyKey: "hook:" + id,
			Payload:        id,
			AvailableAt:    now,
		})
		if err != nil || !created {
			t.Fatalf("enqueue %s: created=%v err=%v", id, created, err)
		}
	}

	claimed, ok, err := store.ClaimJob(ctx, "current-job", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim current job: ok=%v err=%v", ok, err)
	}
	if claimed.ID != "current-job" || claimed.Status != domain.JobProcessing || claimed.Attempts != 1 {
		t.Fatalf("claimed job = %#v, want current processing job", claimed)
	}
	if _, ok, err := store.ClaimJob(ctx, "current-job", time.Minute); err != nil || ok {
		t.Fatalf("reclaim live lease: ok=%v err=%v", ok, err)
	}

	older, ok, err := store.ClaimJob(ctx, "older-job", time.Minute)
	if err != nil || !ok || older.ID != "older-job" {
		t.Fatalf("claim untouched older job: job=%#v ok=%v err=%v", older, ok, err)
	}
	if err := store.CompleteJob(ctx, "current-job"); err != nil {
		t.Fatalf("complete processing job: %v", err)
	}
	if err := store.CompleteJob(ctx, "current-job"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("complete already completed job error = %v, want ErrNotFound", err)
	}
	if err := store.RetryJob(ctx, "current-job", "late failure", now, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retry completed job error = %v, want ErrNotFound", err)
	}
}

func TestProposalEvaluationPromotionRollbackAndAudit(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	skill, cluster := seedEligibleCluster(t, store)

	proposal := domain.Proposal{
		ID:              "proposal-1",
		ClusterID:       cluster.ID,
		SkillID:         skill.ID,
		Status:          domain.ProposalEvaluated,
		RepositoryPath:  skill.RepositoryPath,
		WorktreePath:    "/tmp/worktree",
		Branch:          "skillloop/proposal-1",
		BaseCommit:      "base",
		CandidateCommit: "candidate",
		Fingerprint:     "stored-fingerprint",
		Lesson:          "Validate the candidate before promotion.",
		CardKind:        domain.CardValidation,
		BaselineScore:   0,
		CandidateScore:  1,
	}
	if err := store.SaveProposal(ctx, proposal); err != nil {
		t.Fatalf("save proposal: %v", err)
	}
	gotProposal, err := store.Proposal(ctx, proposal.ID)
	if err != nil {
		t.Fatalf("get proposal: %v", err)
	}
	if gotProposal.CandidateCommit != "candidate" || gotProposal.Status != domain.ProposalEvaluated {
		t.Fatalf("proposal = %#v", gotProposal)
	}

	for _, result := range []domain.EvaluationResult{
		{ID: "eval-baseline", ProposalID: proposal.ID, Variant: domain.EvaluationBaseline, Score: 0, Passed: false},
		{ID: "eval-candidate", ProposalID: proposal.ID, Variant: domain.EvaluationCandidate, Score: 1, Passed: true},
	} {
		created, err := store.RecordEvaluationResult(ctx, result)
		if err != nil || !created {
			t.Fatalf("record evaluation result: created=%v err=%v", created, err)
		}
	}
	created, err := store.RecordEvaluationResult(ctx, domain.EvaluationResult{
		ID: "eval-candidate", ProposalID: proposal.ID, Variant: domain.EvaluationCandidate,
	})
	if err != nil || created {
		t.Fatalf("record duplicate evaluation result: created=%v err=%v", created, err)
	}
	results, err := store.ListEvaluationResults(ctx, proposal.ID)
	if err != nil || len(results) != 2 {
		t.Fatalf("evaluation results = %#v err=%v", results, err)
	}

	promotion := domain.Promotion{
		ID:             "promotion-1",
		ProposalID:     proposal.ID,
		SkillID:        skill.ID,
		PreviousCommit: "base",
		PromotedCommit: "candidate",
	}
	if err := store.RecordPromotion(ctx, promotion); err != nil {
		t.Fatalf("record promotion: %v", err)
	}
	mismatchedPromotion := promotion
	mismatchedPromotion.PromotedCommit = "other-candidate"
	if err := store.RecordPromotion(ctx, mismatchedPromotion); err == nil {
		t.Fatal("mismatched idempotent promotion unexpectedly succeeded")
	}
	active, err := store.ActivePromotion(ctx, skill.ID)
	if err != nil || active.ID != promotion.ID || !active.Active {
		t.Fatalf("active promotion = %#v err=%v", active, err)
	}
	if err := store.UpdatePromotionMonitor(ctx, promotion.ID, domain.MonitorHealthy); err != nil {
		t.Fatalf("update promotion monitor: %v", err)
	}
	active, err = store.Promotion(ctx, promotion.ID)
	if err != nil || active.MonitorStatus != domain.MonitorHealthy || active.LastMonitoredAt.IsZero() {
		t.Fatalf("monitored promotion = %#v err=%v", active, err)
	}
	promotions, err := store.ListPromotions(ctx, true)
	if err != nil || len(promotions) != 1 || promotions[0].ID != promotion.ID {
		t.Fatalf("active promotions = %#v err=%v", promotions, err)
	}
	gotProposal, err = store.Proposal(ctx, proposal.ID)
	if err != nil || gotProposal.Status != domain.ProposalPromoted {
		t.Fatalf("promoted proposal = %#v err=%v", gotProposal, err)
	}

	rollback := domain.Rollback{
		ID:          "rollback-1",
		PromotionID: promotion.ID,
		FromCommit:  "candidate",
		ToCommit:    "base",
		Reason:      "regression",
		Actor:       "tester",
	}
	if err := store.RecordRollback(ctx, rollback); err != nil {
		t.Fatalf("record rollback: %v", err)
	}
	if err := store.RecordRollback(ctx, rollback); err != nil {
		t.Fatalf("record rollback idempotently: %v", err)
	}
	mismatchedRollback := rollback
	mismatchedRollback.Reason = "different reason"
	if err := store.RecordRollback(ctx, mismatchedRollback); err == nil {
		t.Fatal("mismatched idempotent rollback unexpectedly succeeded")
	}
	if _, err := store.ActivePromotion(ctx, skill.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("active promotion after rollback error = %v, want ErrNotFound", err)
	}
	rollbacks, err := store.ListRollbacks(ctx, promotion.ID)
	if err != nil || len(rollbacks) != 1 {
		t.Fatalf("rollbacks = %#v err=%v", rollbacks, err)
	}

	entry, err := store.AppendAudit(ctx, domain.AuditEntry{
		Action: "rollback", EntityType: "promotion", EntityID: promotion.ID,
		Actor: "tester", Details: `{"reason":"regression"}`,
	})
	if err != nil || entry.ID == 0 {
		t.Fatalf("append audit: entry=%#v err=%v", entry, err)
	}
	audit, err := store.ListAudit(ctx, "promotion", promotion.ID)
	if err != nil || len(audit) != 3 || audit[0].ID != entry.ID ||
		audit[1].Action != "promotion.rolled_back" || audit[2].Action != "promotion.created" {
		t.Fatalf("audit = %#v err=%v", audit, err)
	}

	counts, err := store.Counts(ctx, 3)
	if err != nil {
		t.Fatalf("read lifecycle counts: %v", err)
	}
	if counts.ActivePromotions != 0 || counts.Rollbacks != 1 || counts.Proposals[domain.ProposalRolledBack] != 1 {
		t.Fatalf("lifecycle counts = %#v", counts)
	}
}

func TestAbandonProposalIfUnchangedPreservesConcurrentReservationUpdates(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	skill, cluster := seedEligibleCluster(t, store)
	observedTime := time.Date(2026, 8, 20, 10, 0, 0, 123, time.UTC)
	store.now = func() time.Time { return observedTime }
	reserved := domain.Proposal{
		ID: "proposal-cas", ClusterID: cluster.ID, SkillID: skill.ID,
		Status: domain.ProposalPending, RepositoryPath: skill.RepositoryPath,
	}
	created, err := store.ReserveProposal(ctx, reserved)
	if err != nil || !created {
		t.Fatalf("reserve: created=%v err=%v", created, err)
	}
	observed, err := store.Proposal(ctx, reserved.ID)
	if err != nil {
		t.Fatal(err)
	}

	updatedTime := observedTime.Add(time.Minute)
	store.now = func() time.Time { return updatedTime }
	filled := observed
	filled.BaseCommit = "base"
	filled.CandidateCommit = "candidate"
	filled.Fingerprint = "exact-fingerprint"
	filled.Lesson = "Validate the exact candidate output."
	filled.CardKind = domain.CardValidation
	filled.RequiresHumanApproval = false
	filled.UpdatedAt = updatedTime
	if err := store.SaveProposal(ctx, filled); err != nil {
		t.Fatalf("fill reservation: %v", err)
	}

	abandoned, err := store.AbandonProposalIfUnchanged(ctx, observed.ID, observed.UpdatedAt)
	if err != nil || abandoned {
		t.Fatalf("stale CAS abandon: abandoned=%v err=%v", abandoned, err)
	}
	stillFilled, err := store.Proposal(ctx, observed.ID)
	if err != nil || stillFilled.CandidateCommit != filled.CandidateCommit {
		t.Fatalf("concurrent proposal was lost: %#v err=%v", stillFilled, err)
	}
	stillClaimed, err := store.Cluster(ctx, cluster.ID)
	if err != nil || stillClaimed.Status != domain.ClusterProposed {
		t.Fatalf("cluster claim was reopened: %#v err=%v", stillClaimed, err)
	}
}

func TestAbandonProposalIfUnchangedPreservesConcurrentEmptyRefresh(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	skill, cluster := seedEligibleCluster(t, store)
	observedTime := time.Date(2026, 8, 20, 11, 0, 0, 456, time.UTC)
	store.now = func() time.Time { return observedTime }
	reserved := domain.Proposal{
		ID: "proposal-refresh-cas", ClusterID: cluster.ID, SkillID: skill.ID,
		Status: domain.ProposalPending, RepositoryPath: skill.RepositoryPath,
	}
	created, err := store.ReserveProposal(ctx, reserved)
	if err != nil || !created {
		t.Fatalf("reserve: created=%v err=%v", created, err)
	}
	observed, err := store.Proposal(ctx, reserved.ID)
	if err != nil {
		t.Fatal(err)
	}

	refreshedTime := observedTime.Add(time.Minute)
	refreshed := observed
	refreshed.UpdatedAt = refreshedTime
	if err := store.SaveProposal(ctx, refreshed); err != nil {
		t.Fatalf("refresh reservation: %v", err)
	}
	abandoned, err := store.AbandonProposalIfUnchanged(ctx, observed.ID, observed.UpdatedAt)
	if err != nil || abandoned {
		t.Fatalf("stale CAS abandoned refreshed reservation: abandoned=%v err=%v", abandoned, err)
	}
	current, err := store.Proposal(ctx, observed.ID)
	if err != nil || current.UpdatedAt != refreshedTime || current.CandidateCommit != "" {
		t.Fatalf("refreshed empty reservation changed: %#v err=%v", current, err)
	}
	abandoned, err = store.AbandonProposalIfUnchanged(ctx, current.ID, current.UpdatedAt)
	if err != nil || !abandoned {
		t.Fatalf("current CAS did not abandon exact empty reservation: abandoned=%v err=%v", abandoned, err)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "skillloop.db")
	store, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("database mode = %o, want no group/world access", info.Mode().Perm())
	}
	return store
}

func testSkill(name string) domain.Skill {
	return domain.Skill{
		ID:              "skill-" + name,
		Name:            name,
		RepositoryPath:  "/tmp/skills",
		InstructionPath: filepath.Join("/tmp/skills", name, "SKILL.md"),
		Enabled:         true,
	}
}

func testSession(id string) domain.Session {
	return domain.Session{
		Reference:      id,
		Source:         domain.SourceCodex,
		ExternalID:     "external-" + id,
		WorkingDir:     "/tmp/project",
		TranscriptPath: filepath.Join("/tmp/transcripts", id+".jsonl"),
	}
}

func testCard(id, sessionRef, skillID, fingerprint string) domain.LearningCard {
	return domain.LearningCard{
		ID:          id,
		SessionRef:  sessionRef,
		SkillID:     skillID,
		Kind:        domain.CardFailure,
		Fingerprint: fingerprint,
		Summary:     "go test failed",
		Lesson:      "run the command in the Nix development shell",
		Confidence:  1,
	}
}

func mustRegisterSkill(t *testing.T, store *Store, skill domain.Skill) {
	t.Helper()
	created, err := store.RegisterSkill(context.Background(), skill)
	if err != nil || !created {
		t.Fatalf("register skill: created=%v err=%v", created, err)
	}
}

func mustRecordSession(t *testing.T, store *Store, session domain.Session) {
	t.Helper()
	created, err := store.RecordSession(context.Background(), session)
	if err != nil || !created {
		t.Fatalf("record session: created=%v err=%v", created, err)
	}
}

func mustAddCard(t *testing.T, store *Store, card domain.LearningCard) {
	t.Helper()
	created, err := store.AddLearningCard(context.Background(), card)
	if err != nil || !created {
		t.Fatalf("add card: created=%v err=%v", created, err)
	}
}

func enqueueTerminalJob(t *testing.T, database *Store, id string, failed bool) string {
	t.Helper()
	ctx := context.Background()
	job := domain.Job{ID: id, Kind: "test", IdempotencyKey: "test:" + id, Payload: id}
	if created, err := database.EnqueueJob(ctx, job); err != nil || !created {
		t.Fatalf("enqueue job %s: created=%v err=%v", id, created, err)
	}
	if _, claimed, err := database.ClaimJob(ctx, id, time.Minute); err != nil || !claimed {
		t.Fatalf("claim job %s: claimed=%v err=%v", id, claimed, err)
	}
	if failed {
		if err := database.RetryJob(ctx, id, "terminal failure", database.now(), true); err != nil {
			t.Fatalf("fail job %s: %v", id, err)
		}
	} else if err := database.CompleteJob(ctx, id); err != nil {
		t.Fatalf("complete job %s: %v", id, err)
	}
	return id
}

func seedEligibleCluster(t *testing.T, store *Store) (domain.Skill, domain.Cluster) {
	t.Helper()
	skill := testSkill("go")
	mustRegisterSkill(t, store, skill)
	for index := 1; index <= 3; index++ {
		session := testSession(fmt.Sprintf("seed-session-%d", index))
		mustRecordSession(t, store, session)
		mustAddCard(t, store, testCard(
			fmt.Sprintf("seed-card-%d", index), session.Reference, skill.ID, "seed-fingerprint",
		))
	}
	clusters, err := store.RebuildClusters(context.Background(), 3)
	if err != nil || len(clusters) != 1 {
		t.Fatalf("seed cluster: clusters=%#v err=%v", clusters, err)
	}
	return skill, clusters[0]
}
