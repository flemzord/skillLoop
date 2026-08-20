package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

func TestMonitorObserveModeDoesNotMutatePromotionAndManualRollbackStillWorks(t *testing.T) {
	manager, promotion, proposal, skill := newPromotedMonitorFixture(t)
	manager.Config.Mode = domain.ModeObserve
	t.Setenv("SKILLLOOP_MONITOR_REGRESSION", "1")

	result, err := manager.Monitor(context.Background())
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	if result.Checked != 0 || result.Healthy != 0 || result.Regressing != 0 || result.RolledBack != 0 || len(result.Failures) != 0 {
		t.Fatalf("observe monitor result = %#v, want no-op", result)
	}
	storedPromotion, err := manager.Store.Promotion(context.Background(), promotion.ID)
	if err != nil {
		t.Fatalf("read promotion: %v", err)
	}
	if !storedPromotion.Active || storedPromotion.MonitorStatus != domain.MonitorPending {
		t.Fatalf("observe mode mutated promotion = %#v", storedPromotion)
	}
	storedProposal, err := manager.Store.Proposal(context.Background(), proposal.ID)
	if err != nil || storedProposal.Status != domain.ProposalPromoted {
		t.Fatalf("observe mode mutated proposal = %#v err=%v", storedProposal, err)
	}
	current, err := manager.Improver.CurrentRelease(skill)
	if err != nil || current.Commit != promotion.PromotedCommit {
		t.Fatalf("observe mode changed release = %#v err=%v", current, err)
	}

	rollback, err := manager.Rollback(context.Background(), skill.ID, "human", "manual rollback in observe mode")
	if err != nil {
		t.Fatalf("manual rollback: %v", err)
	}
	if rollback.ToCommit != promotion.PreviousCommit {
		t.Fatalf("manual rollback target=%s, want %s", rollback.ToCommit, promotion.PreviousCommit)
	}
}

func TestMonitorDiscardsInFlightResultAfterPolicyChange(t *testing.T) {
	for _, test := range []struct {
		name         string
		mutate       func(*config.Config)
		wantFailures int
	}{
		{name: "observe", mutate: func(settings *config.Config) { settings.Mode = domain.ModeObserve }},
		{
			name: "evaluator-command", wantFailures: 1,
			mutate: func(settings *config.Config) {
				settings.Evaluation.Command = append(settings.Evaluation.Command, "-test.v")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, promotion, proposal, skill := newPromotedMonitorFixture(t)
			settings, err := config.Default()
			if err != nil {
				t.Fatal(err)
			}
			settings.DataDir = fixture.Config.DataDir
			settings.Mode = domain.ModePropose
			settings.Aggregation.MinimumSessions = 3
			settings.Evaluation = fixture.Config.Evaluation
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if _, err := config.WriteInitial(configPath, settings); err != nil {
				t.Fatalf("write config: %v", err)
			}
			manager := New(settings, fixture.Store)
			manager.ConfigLoader = func() (config.Config, error) { return config.Load(configPath) }
			manager.PolicyLocker = func(ctx context.Context) (func() error, error) {
				return config.AcquirePolicyReadLock(ctx, configPath)
			}

			readyPath := filepath.Join(t.TempDir(), "monitor-ready")
			continuePath := filepath.Join(t.TempDir(), "monitor-continue")
			t.Setenv("SKILLLOOP_MONITOR_POLICY_READY", readyPath)
			t.Setenv("SKILLLOOP_MONITOR_POLICY_CONTINUE", continuePath)
			t.Setenv("SKILLLOOP_MONITOR_REGRESSION", "1")
			invocationsPath := filepath.Join(t.TempDir(), "monitor-invocations")
			t.Setenv("SKILLLOOP_MONITOR_INVOCATIONS", invocationsPath)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			type monitorOutcome struct {
				result MonitorResult
				err    error
			}
			finished := make(chan monitorOutcome, 1)
			go func() {
				result, err := manager.Monitor(ctx)
				finished <- monitorOutcome{result: result, err: err}
			}()
			waitForMonitorFile(t, ctx, readyPath)
			test.mutate(&settings)
			saveDone := make(chan error, 1)
			go func() {
				_, saveErr := config.Save(configPath, settings)
				saveDone <- saveErr
			}()
			waitForMonitorPolicyWriter(t, ctx, configPath)

			var nextFinished chan monitorOutcome
			if test.name == "observe" {
				nextFinished = make(chan monitorOutcome, 1)
				go func() {
					result, monitorErr := manager.Monitor(ctx)
					nextFinished <- monitorOutcome{result: result, err: monitorErr}
				}()
			}
			select {
			case saveErr := <-saveDone:
				t.Fatalf("policy change bypassed running monitor evaluator lock: %v", saveErr)
			case <-time.After(100 * time.Millisecond):
			}
			if err := os.WriteFile(continuePath, []byte("continue"), 0o600); err != nil {
				t.Fatalf("release monitor evaluator: %v", err)
			}
			if saveErr := <-saveDone; saveErr != nil {
				t.Fatalf("change policy: %v", saveErr)
			}
			outcome := <-finished
			if outcome.err != nil {
				t.Fatalf("monitor: %v", outcome.err)
			}
			if outcome.result.Checked != 1 || outcome.result.Healthy != 0 || outcome.result.Regressing != 0 || outcome.result.RolledBack != 0 || len(outcome.result.Failures) != test.wantFailures {
				t.Fatalf("discarded monitor result=%#v", outcome.result)
			}
			storedPromotion, err := manager.Store.Promotion(ctx, promotion.ID)
			if err != nil || !storedPromotion.Active || storedPromotion.MonitorStatus != domain.MonitorPending {
				t.Fatalf("promotion mutated=%#v err=%v", storedPromotion, err)
			}
			storedProposal, err := manager.Store.Proposal(ctx, proposal.ID)
			if err != nil || storedProposal.Status != domain.ProposalPromoted {
				t.Fatalf("proposal mutated=%#v err=%v", storedProposal, err)
			}
			current, err := manager.Improver.CurrentRelease(skill)
			if err != nil || current.Commit != promotion.PromotedCommit {
				t.Fatalf("release mutated=%#v err=%v", current, err)
			}
			if test.name == "observe" {
				next := <-nextFinished
				if next.err != nil || next.result.Checked != 0 || next.result.Healthy != 0 ||
					next.result.Regressing != 0 || next.result.RolledBack != 0 || len(next.result.Failures) != 0 {
					t.Fatalf("monitor launched after observe policy was saved: result=%#v err=%v", next.result, next.err)
				}
				if invocations := monitorInvocationCount(t, invocationsPath); invocations != 2 {
					t.Fatalf("monitor evaluator invocations=%d, want only the in-flight baseline/candidate pair", invocations)
				}
			}
		})
	}
}

func TestMonitorDiscardsRegressionForSupersededPromotion(t *testing.T) {
	fixture, firstPromotion, _, skill := newPromotedMonitorFixture(t)
	t.Setenv("SKILLLOOP_MONITOR_EXPECTED_MARKERS", "2")
	secondCluster := seedClusterKind(
		t, fixture.Store, skill, domain.CardValidation,
		"newer monitored release", "go test ./...", 10,
	)
	secondProposal, created, err := fixture.Create(context.Background(), secondCluster.ID, "test")
	if err != nil || !created {
		t.Fatalf("create second proposal: created=%v err=%v", created, err)
	}
	secondProposal, _, err = fixture.Evaluate(context.Background(), secondProposal.ID, "test")
	if err != nil {
		t.Fatalf("evaluate second proposal: %v", err)
	}

	readyPath := filepath.Join(t.TempDir(), "monitor-generation-ready")
	continuePath := filepath.Join(t.TempDir(), "monitor-generation-continue")
	t.Setenv("SKILLLOOP_MONITOR_POLICY_READY", readyPath)
	t.Setenv("SKILLLOOP_MONITOR_POLICY_CONTINUE", continuePath)
	t.Setenv("SKILLLOOP_MONITOR_EXPECTED_MARKERS", "1")
	t.Setenv("SKILLLOOP_MONITOR_REGRESSION", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	type monitorOutcome struct {
		result MonitorResult
		err    error
	}
	finished := make(chan monitorOutcome, 1)
	go func() {
		result, monitorErr := fixture.Monitor(ctx)
		finished <- monitorOutcome{result: result, err: monitorErr}
	}()
	waitForMonitorFile(t, ctx, readyPath)

	secondPromotion, err := fixture.Approve(ctx, secondProposal.ID, "human")
	if err != nil {
		t.Fatalf("approve newer promotion: %v", err)
	}
	if err := os.WriteFile(continuePath, []byte("continue"), 0o600); err != nil {
		t.Fatalf("release monitor evaluator: %v", err)
	}
	outcome := <-finished
	if outcome.err != nil {
		t.Fatalf("monitor: %v", outcome.err)
	}
	if outcome.result.Checked != 1 || outcome.result.Regressing != 0 || outcome.result.RolledBack != 0 || len(outcome.result.Failures) != 1 {
		t.Fatalf("superseded monitor result=%#v", outcome.result)
	}
	if !strings.Contains(outcome.result.Failures[0].Error, "promotion generation changed") {
		t.Fatalf("superseded monitor failure=%q", outcome.result.Failures[0].Error)
	}
	active, err := fixture.Store.ActivePromotion(ctx, skill.ID)
	if err != nil || active.ID != secondPromotion.ID || active.PromotedCommit != secondPromotion.PromotedCommit {
		t.Fatalf("active promotion=%#v err=%v", active, err)
	}
	current, err := fixture.Improver.CurrentRelease(skill)
	if err != nil || current.Commit != secondPromotion.PromotedCommit {
		t.Fatalf("current release=%#v err=%v", current, err)
	}
	stale, err := fixture.Store.Promotion(ctx, firstPromotion.ID)
	if err != nil || stale.Active || stale.MonitorStatus != domain.MonitorPending {
		t.Fatalf("stale promotion mutated=%#v err=%v", stale, err)
	}
}

func TestMonitorExternalEvaluatorHelper(t *testing.T) {
	if os.Getenv("SKILLLOOP_MONITOR_HELPER") != "1" {
		return
	}
	if path := os.Getenv("SKILLLOOP_MONITOR_INVOCATIONS"); path != "" {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(2)
		}
		if _, err := fmt.Fprintln(file, os.Getpid()); err != nil {
			_ = file.Close()
			os.Exit(2)
		}
		if err := file.Close(); err != nil {
			os.Exit(2)
		}
	}
	contents, err := os.ReadFile(filepath.Join("skills", "demo", "SKILL.md"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	expectedMarkers := 1
	if raw := os.Getenv("SKILLLOOP_MONITOR_EXPECTED_MARKERS"); raw != "" {
		expectedMarkers, err = strconv.Atoi(raw)
		if err != nil {
			os.Exit(2)
		}
	}
	if strings.Count(string(contents), "<!-- skillloop:begin ") < expectedMarkers {
		os.Exit(1)
	}
	if readyPath := os.Getenv("SKILLLOOP_MONITOR_POLICY_READY"); readyPath != "" {
		if err := os.WriteFile(readyPath, []byte("ready"), 0o600); err != nil {
			os.Exit(2)
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(os.Getenv("SKILLLOOP_MONITOR_POLICY_CONTINUE")); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if os.Getenv("SKILLLOOP_MONITOR_REGRESSION") == "1" {
		os.Exit(1)
	}
}

func waitForMonitorFile(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for monitor evaluator: %v", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func waitForMonitorPolicyWriter(t *testing.T, ctx context.Context, configPath string) {
	t.Helper()
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
		unlock, err := config.AcquirePolicyReadLock(probeCtx, configPath)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			return
		}
		if err != nil {
			t.Fatalf("probe policy writer: %v", err)
		}
		if err := unlock(); err != nil {
			t.Fatalf("release policy writer probe: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for policy writer: %v", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func monitorInvocationCount(t *testing.T, path string) int {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read monitor invocation log: %v", err)
	}
	return len(strings.Fields(string(contents)))
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
