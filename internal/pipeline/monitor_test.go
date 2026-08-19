package pipeline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flemzord/skillloop/internal/config"
	"github.com/flemzord/skillloop/internal/domain"
	"github.com/flemzord/skillloop/internal/store"
)

func TestMonitorRollsBackExternalEvaluationRegression(t *testing.T) {
	manager, promotion, proposal, skill := newPromotedMonitorFixture(t)
	t.Setenv("SKILLLOOP_MONITOR_REGRESSION", "1")

	result, err := manager.Monitor(context.Background())
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	if result.Checked != 1 || result.Regressing != 1 || result.RolledBack != 1 || result.Healthy != 0 || len(result.Failures) != 0 {
		t.Fatalf("monitor result = %#v", result)
	}

	current, err := manager.Improver.CurrentRelease(skill)
	if err != nil {
		t.Fatalf("current release: %v", err)
	}
	if current.Commit != promotion.PreviousCommit {
		t.Fatalf("current release = %s, want previous %s", current.Commit, promotion.PreviousCommit)
	}
	storedPromotion, err := manager.Store.Promotion(context.Background(), promotion.ID)
	if err != nil {
		t.Fatalf("read promotion: %v", err)
	}
	if storedPromotion.Active || storedPromotion.MonitorStatus != domain.MonitorRolledBack {
		t.Fatalf("promotion after monitoring = %#v", storedPromotion)
	}
	storedProposal, err := manager.Store.Proposal(context.Background(), proposal.ID)
	if err != nil {
		t.Fatalf("read proposal: %v", err)
	}
	if storedProposal.Status != domain.ProposalRolledBack {
		t.Fatalf("proposal status = %s, want %s", storedProposal.Status, domain.ProposalRolledBack)
	}
	rollbacks, err := manager.Store.ListRollbacks(context.Background(), promotion.ID)
	if err != nil {
		t.Fatalf("list rollbacks: %v", err)
	}
	if len(rollbacks) != 1 || rollbacks[0].Actor != "monitor" || rollbacks[0].Reason != monitorRegressionReason {
		t.Fatalf("rollbacks = %#v", rollbacks)
	}
}

func TestMonitorEvaluatorInfrastructureFailureDoesNotRollback(t *testing.T) {
	manager, promotion, _, skill := newPromotedMonitorFixture(t)
	manager.Improver.Runner.Argv = []string{filepath.Join(t.TempDir(), "missing-evaluator")}

	result, err := manager.Monitor(context.Background())
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	if result.Checked != 1 || result.Healthy != 0 || result.Regressing != 0 || result.RolledBack != 0 || len(result.Failures) != 1 {
		t.Fatalf("monitor result = %#v", result)
	}
	if result.Failures[0].PromotionID != promotion.ID || result.Failures[0].SkillID != skill.ID {
		t.Fatalf("monitor failure = %#v", result.Failures[0])
	}
	if !strings.Contains(result.Failures[0].Error, "missing-evaluator") {
		t.Fatalf("monitor failure does not identify evaluator: %q", result.Failures[0].Error)
	}

	active, err := manager.Store.ActivePromotion(context.Background(), skill.ID)
	if err != nil {
		t.Fatalf("active promotion: %v", err)
	}
	if active.ID != promotion.ID || !active.Active || active.MonitorStatus != domain.MonitorPending {
		t.Fatalf("promotion changed after infrastructure failure = %#v", active)
	}
	current, err := manager.Improver.CurrentRelease(skill)
	if err != nil {
		t.Fatalf("current release: %v", err)
	}
	if current.Commit != promotion.PromotedCommit {
		t.Fatalf("current release = %s, want promoted %s", current.Commit, promotion.PromotedCommit)
	}
	rollbacks, err := manager.Store.ListRollbacks(context.Background(), promotion.ID)
	if err != nil {
		t.Fatalf("list rollbacks: %v", err)
	}
	if len(rollbacks) != 0 {
		t.Fatalf("unexpected rollback after infrastructure failure: %#v", rollbacks)
	}
}

func TestMonitorWithoutExternalEvaluatorDoesNotMarkPromotionHealthy(t *testing.T) {
	manager, promotion, _, skill := newPromotedMonitorFixture(t)
	manager.Improver.Runner.Argv = nil

	result, err := manager.Monitor(context.Background())
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	if result.Checked != 1 || result.Healthy != 0 || result.Regressing != 0 || result.RolledBack != 0 || len(result.Failures) != 1 {
		t.Fatalf("monitor result = %#v", result)
	}
	if !strings.Contains(result.Failures[0].Error, "external baseline/candidate evaluation is required") {
		t.Fatalf("monitor failure is not explicit: %q", result.Failures[0].Error)
	}

	active, err := manager.Store.ActivePromotion(context.Background(), skill.ID)
	if err != nil {
		t.Fatalf("active promotion: %v", err)
	}
	if active.ID != promotion.ID || !active.Active || active.MonitorStatus != domain.MonitorPending {
		t.Fatalf("promotion changed without external evaluator = %#v", active)
	}
}

func TestMonitorExternalEvaluatorHelper(t *testing.T) {
	if os.Getenv("SKILLLOOP_MONITOR_HELPER") != "1" {
		return
	}
	contents, err := os.ReadFile(filepath.Join("skills", "demo", "SKILL.md"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if !strings.Contains(string(contents), "<!-- skillloop:begin monitor-regression -->") {
		os.Exit(1)
	}
	if os.Getenv("SKILLLOOP_MONITOR_REGRESSION") == "1" {
		os.Exit(1)
	}
}

func newPromotedMonitorFixture(t *testing.T) (Manager, domain.Promotion, domain.Proposal, domain.Skill) {
	t.Helper()
	ctx := context.Background()
	repository := t.TempDir()
	monitorGit(t, repository, "init", "-b", "main")
	monitorGit(t, repository, "config", "user.name", "Test User")
	monitorGit(t, repository, "config", "user.email", "test@example.invalid")
	instruction := filepath.Join("skills", "demo", "SKILL.md")
	monitorWriteFile(t, filepath.Join(repository, instruction), []byte("---\nname: demo\ndescription: Demonstration skill\n---\n\n# Demo\n\nFollow the documented workflow.\n"))
	monitorGit(t, repository, "add", ".")
	monitorGit(t, repository, "commit", "-m", "feat: add demo skill")

	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "skillloop.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	skill := domain.Skill{
		ID: "demo-skill", Name: "Demo", RepositoryPath: repository,
		InstructionPath: filepath.ToSlash(instruction), Enabled: true,
	}
	created, err := database.RegisterSkill(ctx, skill)
	if err != nil || !created {
		t.Fatalf("register skill: created=%v err=%v", created, err)
	}
	for index := 1; index <= 3; index++ {
		session := domain.Session{
			Reference: fmt.Sprintf("session-%d", index), Source: domain.SourceCodex,
			ExternalID: fmt.Sprintf("external-%d", index), WorkingDir: repository,
			TranscriptPath: filepath.Join(repository, fmt.Sprintf("transcript-%d.jsonl", index)),
		}
		created, err := database.RecordSession(ctx, session)
		if err != nil || !created {
			t.Fatalf("record session: created=%v err=%v", created, err)
		}
		created, err = database.AddLearningCard(ctx, domain.LearningCard{
			ID: fmt.Sprintf("card-%d", index), SessionRef: session.Reference, SkillID: skill.ID,
			Kind: domain.CardCorrection, Fingerprint: "monitor-regression",
			Summary: "Monitoring catches an external regression",
			Lesson:  "Run validation before reporting completion.", Confidence: 1,
		})
		if err != nil || !created {
			t.Fatalf("add learning card: created=%v err=%v", created, err)
		}
	}
	clusters, err := database.RebuildClusters(ctx, 3)
	if err != nil || len(clusters) != 1 {
		t.Fatalf("rebuild clusters: clusters=%#v err=%v", clusters, err)
	}

	t.Setenv("SKILLLOOP_MONITOR_HELPER", "1")
	t.Setenv("SKILLLOOP_MONITOR_REGRESSION", "0")
	stateDir := t.TempDir()
	t.Cleanup(func() { monitorMakeTreeWritable(stateDir) })
	manager := New(config.Config{
		Mode: domain.ModePropose, DataDir: stateDir,
		Aggregation: config.Aggregation{MinimumSessions: 3},
		Evaluation: config.Evaluation{
			Command:            []string{os.Args[0], "-test.run=^TestMonitorExternalEvaluatorHelper$"},
			MinimumImprovement: 0.1,
		},
	}, database)
	proposal, created, err := manager.Create(ctx, clusters[0].ID, "test")
	if err != nil || !created {
		t.Fatalf("create proposal: created=%v err=%v", created, err)
	}
	proposal, evaluation, err := manager.Evaluate(ctx, proposal.ID, "test")
	if err != nil || !evaluation.Passed {
		t.Fatalf("evaluate proposal: passed=%v err=%v", evaluation.Passed, err)
	}
	promotion, err := manager.Approve(ctx, proposal.ID, "test")
	if err != nil {
		t.Fatalf("approve proposal: %v", err)
	}
	return manager, promotion, proposal, skill
}

func monitorGit(t *testing.T, repository string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), output, err)
	}
}

func monitorWriteFile(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatal(err)
	}
}

func monitorMakeTreeWritable(root string) {
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			_ = os.Chmod(path, 0o700)
		} else {
			_ = os.Chmod(path, 0o600)
		}
		return nil
	})
}
