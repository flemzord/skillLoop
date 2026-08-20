package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flemzord/skillloop/internal/config"
	"github.com/flemzord/skillloop/internal/domain"
	"github.com/flemzord/skillloop/internal/store"
)

func TestDrainRefusesInFlightAutopilotAfterPolicyChange(t *testing.T) {
	for _, test := range []struct {
		name          string
		mutate        func(*config.Config)
		wantEvaluated int
		wantStatus    domain.ProposalStatus
	}{
		{
			name: "mode-propose", mutate: func(settings *config.Config) { settings.Mode = domain.ModePropose },
			wantEvaluated: 1, wantStatus: domain.ProposalEvaluated,
		},
		{
			name: "mode-observe", mutate: func(settings *config.Config) { settings.Mode = domain.ModeObserve },
			wantEvaluated: 1, wantStatus: domain.ProposalEvaluated,
		},
		{
			name: "quorum-increase", mutate: func(settings *config.Config) { settings.Aggregation.MinimumSessions = 4 },
			wantEvaluated: 1, wantStatus: domain.ProposalEvaluated,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			dataDir := t.TempDir()
			t.Cleanup(func() { daemonPolicyMakeTreeWritable(dataDir) })
			database, err := store.Open(ctx, filepath.Join(dataDir, "skillloop.db"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = database.Close() })

			repository := filepath.Join(t.TempDir(), "repository")
			instructionPath := filepath.Join("skills", "demo", "SKILL.md")
			if err := os.MkdirAll(filepath.Join(repository, filepath.Dir(instructionPath)), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repository, instructionPath), []byte("---\nname: demo\ndescription: Demo skill\n---\n\n# Demo\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			daemonPolicyGit(t, repository, "init", "-b", "main")
			daemonPolicyGit(t, repository, "config", "user.name", "Test User")
			daemonPolicyGit(t, repository, "config", "user.email", "test@example.invalid")
			daemonPolicyGit(t, repository, "add", ".")
			daemonPolicyGit(t, repository, "commit", "-m", "feat: add skill")
			skill := domain.Skill{
				ID: "demo-skill", Name: "Demo", RepositoryPath: repository,
				InstructionPath: filepath.ToSlash(instructionPath), Enabled: true, CreatedAt: time.Now().UTC(),
			}
			if created, err := database.RegisterSkill(ctx, skill); err != nil || !created {
				t.Fatalf("register skill: created=%v err=%v", created, err)
			}
			for index := range 3 {
				sessionID := fmt.Sprintf("policy-session-%d", index)
				if created, err := database.RecordSession(ctx, domain.Session{
					Reference: sessionID, Source: domain.SourceCodex, ExternalID: sessionID,
				}); err != nil || !created {
					t.Fatalf("record session: created=%v err=%v", created, err)
				}
				if created, err := database.AddLearningCard(ctx, domain.LearningCard{
					ID: fmt.Sprintf("policy-card-%d", index), SessionRef: sessionID, SkillID: skill.ID,
					Kind: domain.CardValidation, Fingerprint: "in-flight-policy",
					Summary: "Current policy is required", Lesson: "go test ./...", Confidence: 1,
				}); err != nil || !created {
					t.Fatalf("add card: created=%v err=%v", created, err)
				}
			}

			readyPath := filepath.Join(t.TempDir(), "candidate-ready")
			continuePath := filepath.Join(t.TempDir(), "continue")
			t.Setenv("SKILLLOOP_POLICY_EVALUATOR", "1")
			t.Setenv("SKILLLOOP_POLICY_READY", readyPath)
			t.Setenv("SKILLLOOP_POLICY_CONTINUE", continuePath)
			settings, err := config.Default()
			if err != nil {
				t.Fatal(err)
			}
			settings.DataDir = dataDir
			settings.Mode = domain.ModeAutopilot
			settings.Aggregation.MinimumSessions = 3
			settings.Evaluation.AllowAutopilot = true
			settings.Evaluation.Command = []string{os.Args[0], "-test.run=^TestDaemonPolicyEvaluatorHelper$"}
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if _, err := config.WriteInitial(configPath, settings); err != nil {
				t.Fatalf("write config: %v", err)
			}

			var loads atomic.Int32
			processor := Processor{
				Config: settings,
				Store:  database,
				LoadConfig: func() (config.Config, error) {
					loads.Add(1)
					return config.Load(configPath)
				},
				LockPolicy: func(ctx context.Context) (func() error, error) {
					return config.AcquirePolicyReadLock(ctx, configPath)
				},
			}
			type drainOutcome struct {
				result DrainResult
				err    error
			}
			finished := make(chan drainOutcome, 1)
			go func() {
				result, err := processor.Drain(ctx, 10)
				finished <- drainOutcome{result: result, err: err}
			}()

			waitForPolicyFile(t, ctx, readyPath)
			test.mutate(&settings)
			saveDone := make(chan error, 1)
			go func() {
				_, saveErr := config.Save(configPath, settings)
				saveDone <- saveErr
			}()
			select {
			case saveErr := <-saveDone:
				t.Fatalf("policy change bypassed running evaluator lock: %v", saveErr)
			case <-time.After(100 * time.Millisecond):
			}
			waitForPolicyWriter(t, ctx, configPath)
			if err := os.WriteFile(continuePath, []byte("continue"), 0o600); err != nil {
				t.Fatalf("release evaluator: %v", err)
			}
			if saveErr := <-saveDone; saveErr != nil {
				t.Fatalf("change policy after evaluator: %v", saveErr)
			}
			outcome := <-finished
			if outcome.err != nil {
				t.Fatalf("drain: %v", outcome.err)
			}
			if outcome.result.ProposalsCreated != 1 || outcome.result.ProposalsEvaluated != test.wantEvaluated || outcome.result.ProposalsPromoted != 0 || outcome.result.ProposalFailures != 1 {
				t.Fatalf("downgraded drain result=%#v", outcome.result)
			}
			if loads.Load() < 2 {
				t.Fatalf("config loads=%d, want start-of-drain and pre-promotion reloads", loads.Load())
			}
			if _, err := database.ActivePromotion(ctx, skill.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("promotion exists after downgrade: %v", err)
			}
			proposals, err := database.ListProposals(ctx, test.wantStatus)
			if err != nil || len(proposals) != 1 {
				t.Fatalf("%s proposals=%#v err=%v", test.wantStatus, proposals, err)
			}
		})
	}
}

func TestDaemonPolicyEvaluatorHelper(t *testing.T) {
	if os.Getenv("SKILLLOOP_POLICY_EVALUATOR") != "1" {
		return
	}
	contents, err := os.ReadFile(filepath.Join("skills", "demo", "SKILL.md"))
	if err != nil {
		os.Exit(2)
	}
	if !strings.Contains(string(contents), "<!-- skillloop:begin ") {
		os.Exit(1)
	}
	if err := os.WriteFile(os.Getenv("SKILLLOOP_POLICY_READY"), []byte("ready"), 0o600); err != nil {
		os.Exit(2)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(os.Getenv("SKILLLOOP_POLICY_CONTINUE")); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	os.Exit(2)
}

func waitForPolicyFile(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for evaluator: %v", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func waitForPolicyWriter(t *testing.T, ctx context.Context, configPath string) {
	t.Helper()
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
		unlock, err := config.AcquirePolicyReadLock(probeCtx, configPath)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			return
		}
		if err != nil {
			t.Fatalf("probe queued policy writer: %v", err)
		}
		if unlockErr := unlock(); unlockErr != nil {
			t.Fatalf("release policy writer probe: %v", unlockErr)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for queued policy writer: %v", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func daemonPolicyGit(t *testing.T, repository string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), output, err)
	}
}

func daemonPolicyMakeTreeWritable(root string) {
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
