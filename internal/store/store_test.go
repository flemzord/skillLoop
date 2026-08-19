package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flemzord/skillloop/internal/domain"
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

	var version int
	if err := store.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", version, schemaVersion)
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
