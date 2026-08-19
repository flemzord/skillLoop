// Package daemon drains captured lifecycle events and turns them into durable
// learning cards. Capture remains a separate, fail-open filesystem operation.
package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/flemzord/skillloop/internal/config"
	"github.com/flemzord/skillloop/internal/domain"
	"github.com/flemzord/skillloop/internal/learning"
	"github.com/flemzord/skillloop/internal/pipeline"
	"github.com/flemzord/skillloop/internal/store"
	"github.com/flemzord/skillloop/internal/transcript"
)

const ingestJobKind = "ingest"

type Processor struct {
	Config     config.Config
	Store      *store.Store
	Normalizer transcript.Normalizer
	Analyzer   learning.Analyzer
	Now        func() time.Time
}

type DrainResult struct {
	Captured             int
	Processed            int
	Excluded             int
	Failed               int
	CardsCreated         int
	EligibleClusters     []domain.Cluster
	ProposalsCreated     int
	ProposalsEvaluated   int
	ProposalsPromoted    int
	ProposalFailures     int
	PromotionsMonitored  int
	PromotionsRolledBack int
	MonitorFailures      int
}

func (processor Processor) Drain(ctx context.Context, limit int) (DrainResult, error) {
	if processor.Store == nil {
		return DrainResult{}, errors.New("daemon: store is required")
	}
	if limit <= 0 {
		limit = 100
	}
	if processor.Now == nil {
		processor.Now = time.Now
	}

	directories, err := ensureSpoolDirectories(processor.Config.DataDir)
	if err != nil {
		return DrainResult{}, err
	}
	if err := recoverProcessing(directories); err != nil {
		return DrainResult{}, err
	}
	entries, err := os.ReadDir(directories.incoming)
	if err != nil {
		return DrainResult{}, fmt.Errorf("daemon: read incoming spool: %w", err)
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })

	result := DrainResult{}
	for _, entry := range entries {
		if result.Captured >= limit {
			break
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		result.Captured++
		if err := ctx.Err(); err != nil {
			return result, err
		}
		outcome, processErr := processor.processFile(ctx, directories, entry.Name())
		result.Processed += outcome.Processed
		result.Excluded += outcome.Excluded
		result.Failed += outcome.Failed
		result.CardsCreated += outcome.CardsCreated
		if processErr != nil {
			// A bad capture must not prevent independent events from being drained.
			continue
		}
	}
	clusters, err := processor.Store.RebuildClusters(ctx, processor.Config.Aggregation.MinimumSessions)
	if err != nil {
		return result, fmt.Errorf("daemon: rebuild clusters: %w", err)
	}
	result.EligibleClusters = clusters
	workflow := pipeline.New(processor.Config, processor.Store)
	proposalResult, err := workflow.ProcessEligible(ctx, clusters)
	if err != nil {
		return result, fmt.Errorf("daemon: process proposals: %w", err)
	}
	result.ProposalsCreated = proposalResult.Created
	result.ProposalsEvaluated = proposalResult.Evaluated
	result.ProposalsPromoted = proposalResult.Promoted
	result.ProposalFailures = len(proposalResult.Failures)
	if processor.Config.Mode != domain.ModeObserve {
		monitorResult, monitorErr := workflow.Monitor(ctx)
		if monitorErr != nil {
			return result, fmt.Errorf("daemon: monitor promotions: %w", monitorErr)
		}
		result.PromotionsMonitored = monitorResult.Checked
		result.PromotionsRolledBack = monitorResult.RolledBack
		result.MonitorFailures = len(monitorResult.Failures)
	}
	return result, nil
}

func (processor Processor) processFile(ctx context.Context, directories spoolDirectories, name string) (DrainResult, error) {
	incomingPath := filepath.Join(directories.incoming, name)
	processingPath := filepath.Join(directories.processing, name)
	if err := os.Rename(incomingPath, processingPath); err != nil {
		return DrainResult{Failed: 1}, fmt.Errorf("daemon: claim spool event: %w", err)
	}
	fail := func(eventID string, cause error) (DrainResult, error) {
		failedPath := filepath.Join(directories.failed, name)
		_ = os.Rename(processingPath, failedPath)
		if eventID != "" {
			_ = processor.Store.RetryJob(ctx, eventID, boundedError(cause), processor.Now(), true)
		}
		return DrainResult{Failed: 1}, cause
	}

	contents, err := os.ReadFile(processingPath)
	if err != nil {
		return fail("", fmt.Errorf("daemon: read spool event: %w", err))
	}
	event := domain.HookEvent{}
	if err := json.Unmarshal(contents, &event); err != nil {
		return fail("", fmt.Errorf("daemon: decode spool event: %w", err))
	}
	if event.ID == "" {
		return fail("", errors.New("daemon: spool event has no id"))
	}
	job := domain.Job{
		ID:             event.ID,
		Kind:           ingestJobKind,
		IdempotencyKey: "hook:" + event.ID,
		Payload:        event.ID,
		Status:         domain.JobQueued,
		AvailableAt:    processor.Now().UTC(),
	}
	created, err := processor.Store.EnqueueJob(ctx, job)
	if err != nil {
		return fail(event.ID, fmt.Errorf("daemon: enqueue ingest job: %w", err))
	}
	if !created {
		if err := os.Remove(processingPath); err != nil {
			return fail("", fmt.Errorf("daemon: acknowledge duplicate event: %w", err))
		}
		return DrainResult{Processed: 1}, nil
	}
	claimed, ok, err := processor.Store.ClaimJob(ctx, event.ID, time.Minute)
	if err != nil {
		return fail(event.ID, fmt.Errorf("daemon: claim ingest job: %w", err))
	}
	if !ok || claimed.ID != event.ID {
		return fail(event.ID, errors.New("daemon: ingest job is not claimable"))
	}

	if excluded(event.WorkingDir, processor.Config.ExcludedPaths) {
		if err := processor.Store.CompleteJob(ctx, event.ID); err != nil {
			return fail(event.ID, fmt.Errorf("daemon: complete excluded job: %w", err))
		}
		if err := os.Remove(processingPath); err != nil {
			return fail(event.ID, fmt.Errorf("daemon: remove excluded event: %w", err))
		}
		return DrainResult{Excluded: 1}, nil
	}

	session, err := processor.Normalizer.Normalize(ctx, event)
	if err != nil {
		return fail(event.ID, fmt.Errorf("daemon: normalize transcript: %w", err))
	}
	if _, err := processor.Store.RecordSession(ctx, session); err != nil {
		return fail(event.ID, fmt.Errorf("daemon: record session: %w", err))
	}
	skills, err := processor.Store.ListSkills(ctx)
	if err != nil {
		return fail(event.ID, fmt.Errorf("daemon: list skills: %w", err))
	}
	createdCards := 0
	for _, card := range processor.Analyzer.Analyze(session, skills) {
		created, err := processor.Store.AddLearningCard(ctx, card)
		if err != nil {
			return fail(event.ID, fmt.Errorf("daemon: add learning card: %w", err))
		}
		if created {
			createdCards++
		}
	}
	if err := processor.Store.CompleteJob(ctx, event.ID); err != nil {
		return fail(event.ID, fmt.Errorf("daemon: complete ingest job: %w", err))
	}
	if err := os.Remove(processingPath); err != nil {
		return fail(event.ID, fmt.Errorf("daemon: acknowledge spool event: %w", err))
	}
	return DrainResult{Processed: 1, CardsCreated: createdCards}, nil
}

type spoolDirectories struct {
	incoming   string
	processing string
	failed     string
}

func ensureSpoolDirectories(dataDir string) (spoolDirectories, error) {
	if dataDir == "" {
		return spoolDirectories{}, errors.New("daemon: data directory is required")
	}
	root := filepath.Join(dataDir, "spool")
	directories := spoolDirectories{
		incoming: filepath.Join(root, "incoming"), processing: filepath.Join(root, "processing"), failed: filepath.Join(root, "failed"),
	}
	for _, path := range []string{dataDir, root, directories.incoming, directories.processing, directories.failed} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return spoolDirectories{}, fmt.Errorf("daemon: create spool directory: %w", err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return spoolDirectories{}, fmt.Errorf("daemon: secure spool directory: %w", err)
		}
	}
	return directories, nil
}

func recoverProcessing(directories spoolDirectories) error {
	entries, err := os.ReadDir(directories.processing)
	if err != nil {
		return fmt.Errorf("daemon: inspect processing spool: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		from := filepath.Join(directories.processing, entry.Name())
		to := filepath.Join(directories.incoming, entry.Name())
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("daemon: recover spool event: %w", err)
		}
	}
	return nil
}

func excluded(workingDir string, configured []string) bool {
	if workingDir == "" {
		return false
	}
	workingDir = canonical(workingDir)
	for _, path := range configured {
		path = canonical(path)
		if path != "" && (workingDir == path || strings.HasPrefix(workingDir, path+string(filepath.Separator))) {
			return true
		}
	}
	for current := workingDir; ; current = filepath.Dir(current) {
		if _, err := os.Stat(filepath.Join(current, ".skillloop-ignore")); err == nil {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return false
}

func canonical(path string) string {
	path = filepath.Clean(path)
	if evaluated, err := filepath.EvalSymlinks(path); err == nil {
		return evaluated
	}
	return path
}

func boundedError(err error) string {
	if err == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(err.Error()))
	return "error-" + hex.EncodeToString(sum[:8])
}
