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
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

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
	LoadConfig func() (config.Config, error)
	LockPolicy func(context.Context) (func() error, error)
}

type DrainResult struct {
	Captured                   int
	Processed                  int
	Excluded                   int
	Failed                     int
	CardsCreated               int
	EligibleClusters           []domain.Cluster
	ProposalsCreated           int
	ProposalsEvaluated         int
	ProposalsPromoted          int
	ProposalFailures           int
	PromotionsMonitored        int
	PromotionsRolledBack       int
	MonitorFailures            int
	PrunedTranscriptLocators   int
	PrunedCompletedJobs        int
	PrunedFailedJobs           int
	PrunedFailedSpool          int
	ProcessingRecoveryComplete bool
}

func (processor Processor) Drain(ctx context.Context, limit int) (DrainResult, error) {
	if processor.Store == nil {
		return DrainResult{}, errors.New("daemon: store is required")
	}
	if processor.LoadConfig != nil {
		settings, err := processor.LoadConfig()
		if err != nil {
			return DrainResult{}, fmt.Errorf("daemon: reload config before drain: %w", err)
		}
		if err := settings.Validate(); err != nil {
			return DrainResult{}, fmt.Errorf("daemon: validate reloaded config: %w", err)
		}
		processor.Config = settings
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
	defer directories.close()
	now := processor.Now().UTC()
	result := DrainResult{}
	recoveryFailures, recoveryComplete, err := processor.recoverProcessing(ctx, directories, now, limit)
	if err != nil {
		return result, err
	}
	result.Failed += recoveryFailures
	result.ProcessingRecoveryComplete = recoveryComplete
	completedJobRetention := processor.Config.Retention.CompletedJobs
	failedJobRetention := processor.Config.Retention.FailedJobs
	if !recoveryComplete {
		// Terminal job rows are reconciliation tombstones. If the bounded
		// processing scan did not reach EOF, retain every tombstone until a
		// later drain proves that no processing entry still depends on it.
		completedJobRetention = 0
		failedJobRetention = 0
	}
	retentionResult, err := processor.Store.PruneRetention(
		ctx,
		now,
		processor.Config.Retention.TranscriptLocators,
		completedJobRetention,
		failedJobRetention,
	)
	if err != nil {
		return result, fmt.Errorf("daemon: prune retained metadata: %w", err)
	}
	prunedFailedSpool, _, err := pruneFailedSpool(ctx, directories, now, processor.Config.Retention.FailedSpool, limit)
	if err != nil {
		return result, err
	}
	result.PrunedTranscriptLocators = retentionResult.TranscriptLocators
	result.PrunedCompletedJobs = retentionResult.CompletedJobs
	result.PrunedFailedJobs = retentionResult.FailedJobs
	result.PrunedFailedSpool = prunedFailedSpool

	names := make([]string, 0, min(limit, spoolDirectoryBatchSize))
	_, err = walkDirectory(directories.incomingFD, directories.incoming, limit, func(entry os.DirEntry) error {
		names = append(names, entry.Name())
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("daemon: read incoming spool: %w", err)
	}
	for _, name := range names {
		result.Captured++
		if err := ctx.Err(); err != nil {
			return result, err
		}
		outcome, processErr := processor.processFile(ctx, directories, name)
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
	workflow.ConfigLoader = processor.LoadConfig
	workflow.PolicyLocker = processor.LockPolicy
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

func pruneFailedSpool(
	ctx context.Context,
	directories spoolDirectories,
	now time.Time,
	retention time.Duration,
	limit int,
) (int, bool, error) {
	if retention < 0 {
		return 0, false, errors.New("daemon: failed spool retention cannot be negative")
	}
	if retention == 0 {
		return 0, true, nil
	}
	cutoff := now.Add(-retention)
	pruned := 0
	// Every examined expired regular or special entry is unlinked. Entries that
	// are still within retention become eligible as time advances, so repeated
	// bounded drains converge without ever trusting their inode timestamps.
	complete, err := walkDirectory(directories.failedFD, directories.failed, limit, func(entry os.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// DirEntry.Info is lstat-like for symlinks and originates from the
		// already-open directory. Unlinkat removes only the directory entry, so
		// retention never opens or follows quarantined poison objects.
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("daemon: inspect failed spool entry: %w", err)
		}
		quarantinedAt, trustedTimestamp := quarantineTimestamp(entry.Name())
		if !trustedTimestamp {
			// Legacy entries have no daemon-authenticated age. Treat them as
			// expired instead of trusting attacker-controlled inode timestamps.
			quarantinedAt = time.Time{}
		}
		if quarantinedAt.After(now) {
			quarantinedAt = time.Time{}
		}
		if info.IsDir() || !quarantinedAt.Before(cutoff) {
			return nil
		}
		if err := unix.Unlinkat(directories.failedFD, entry.Name(), 0); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return fmt.Errorf("daemon: prune failed spool entry: %w", err)
		}
		pruned++
		return nil
	})
	if err != nil {
		return pruned, false, fmt.Errorf("daemon: inspect failed spool: %w", err)
	}
	if pruned > 0 {
		if err := unix.Fsync(directories.failedFD); err != nil {
			return pruned, false, fmt.Errorf("daemon: sync failed spool retention: %w", err)
		}
	}
	return pruned, complete, nil
}

func (processor Processor) processFile(ctx context.Context, directories spoolDirectories, name string) (DrainResult, error) {
	if err := moveEntryNoReplace(directories.incomingFD, name, nil, directories.processingFD, name); err != nil {
		if errors.Is(err, os.ErrExist) {
			_, quarantineErr := quarantineEntry(directories, directories.incomingFD, name, nil, processor.Now().UTC())
			return DrainResult{Failed: 1}, errors.Join(errors.New("daemon: processing spool collision"), quarantineErr)
		}
		return DrainResult{Failed: 1}, fmt.Errorf("daemon: claim spool event: %w", err)
	}
	file, info, err := openSpoolEntry(directories.processingFD, name)
	if err != nil {
		_, quarantineErr := quarantineEntry(directories, directories.processingFD, name, nil, processor.Now().UTC())
		return DrainResult{Failed: 1}, errors.Join(fmt.Errorf("daemon: reject unsafe spool event: %w", err), quarantineErr)
	}
	defer func() { _ = file.Close() }()
	fencingToken := 0
	fail := func(eventID string, cause error) (DrainResult, error) {
		return processor.failClaimedEntry(ctx, directories, name, info, eventID, fencingToken, cause)
	}
	acknowledge := func() (DrainResult, error) {
		if err := removeEntry(directories.processingFD, name, info); err != nil {
			return fail("", fmt.Errorf("daemon: acknowledge terminal event: %w", err))
		}
		return DrainResult{Captured: 1, Processed: 1}, nil
	}
	deferToLeaseOwner := func() (DrainResult, error) {
		if err := moveEntryNoReplace(directories.processingFD, name, info, directories.incomingFD, name); err != nil {
			return fail("", fmt.Errorf("daemon: defer leased event: %w", err))
		}
		return DrainResult{Captured: 1}, nil
	}

	contents, err := readSpoolEntry(file, directories.processingFD, name, info)
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
	if event.ID+".json" != name {
		return fail("", errors.New("daemon: spool event id does not match its filename"))
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
		existing, err := processor.Store.Job(ctx, event.ID)
		if err != nil {
			return fail("", fmt.Errorf("daemon: inspect duplicate ingest job: %w", err))
		}
		if existing.Status == domain.JobCompleted || existing.Status == domain.JobFailed {
			return acknowledge()
		}
	}
	claimed, ok, err := processor.Store.ClaimJob(ctx, event.ID, time.Minute)
	if err != nil {
		return fail(event.ID, fmt.Errorf("daemon: claim ingest job: %w", err))
	}
	if !ok || claimed.ID != event.ID {
		return deferToLeaseOwner()
	}
	fencingToken = claimed.FencingToken

	session, err := processor.Normalizer.Normalize(ctx, event)
	if err != nil {
		return fail(event.ID, fmt.Errorf("daemon: normalize transcript: %w", err))
	}
	if excluded(session.WorkingDir, processor.Config.ExcludedPaths) {
		if err := processor.Store.CompleteJob(ctx, event.ID, fencingToken); err != nil {
			return fail(event.ID, fmt.Errorf("daemon: complete excluded job: %w", err))
		}
		if err := removeEntry(directories.processingFD, name, info); err != nil {
			return fail(event.ID, fmt.Errorf("daemon: remove excluded event: %w", err))
		}
		return DrainResult{Captured: 1, Excluded: 1}, nil
	}

	skills, err := processor.Store.ListSkills(ctx)
	if err != nil {
		return fail(event.ID, fmt.Errorf("daemon: list skills: %w", err))
	}
	cards := processor.Analyzer.Analyze(session, skills)
	createdCards, err := processor.Store.CommitIngestJob(ctx, event.ID, fencingToken, session, cards)
	if err != nil {
		return fail(event.ID, fmt.Errorf("daemon: commit fenced ingest job: %w", err))
	}
	if err := removeEntry(directories.processingFD, name, info); err != nil {
		return fail(event.ID, fmt.Errorf("daemon: acknowledge spool event: %w", err))
	}
	return DrainResult{Captured: 1, Processed: 1, CardsCreated: createdCards}, nil
}

func (processor Processor) failClaimedEntry(
	ctx context.Context,
	directories spoolDirectories,
	name string,
	info os.FileInfo,
	eventID string,
	fencingToken int,
	cause error,
) (DrainResult, error) {
	if eventID != "" && fencingToken > 0 {
		if retryErr := processor.Store.RetryJob(ctx, eventID, fencingToken, boundedError(cause), processor.Now(), true); retryErr != nil {
			// A failed fencing CAS means another worker owns the inode now. The
			// stale worker must not move or remove that worker's spool entry.
			return DrainResult{Captured: 1, Failed: 1}, errors.Join(cause, fmt.Errorf("daemon: preserve spool after lost lease: %w", retryErr))
		}
	}
	_, quarantineErr := quarantineEntry(directories, directories.processingFD, name, info, processor.Now().UTC())
	return DrainResult{Captured: 1, Failed: 1}, errors.Join(cause, quarantineErr)
}

func (processor Processor) recoverProcessing(
	ctx context.Context,
	directories spoolDirectories,
	now time.Time,
	limit int,
) (int, bool, error) {
	failures := 0
	complete, err := walkDirectory(directories.processingFD, directories.processing, limit, func(entry os.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, info, openErr := openSpoolEntry(directories.processingFD, entry.Name())
		if openErr != nil {
			if _, err := quarantineEntry(directories, directories.processingFD, entry.Name(), nil, now); err != nil {
				return errors.Join(openErr, err)
			}
			failures++
			return nil
		}
		contents, readErr := readSpoolEntry(file, directories.processingFD, entry.Name(), info)
		_ = file.Close()
		event := domain.HookEvent{}
		decodeErr := json.Unmarshal(contents, &event)
		if readErr != nil || decodeErr != nil || event.ID == "" || event.ID+".json" != entry.Name() {
			if _, err := quarantineEntry(directories, directories.processingFD, entry.Name(), info, now); err != nil {
				return errors.Join(readErr, decodeErr, err)
			}
			failures++
			return nil
		}
		job, jobErr := processor.Store.Job(ctx, event.ID)
		switch {
		case jobErr == nil && job.Status == domain.JobProcessing && now.Before(job.LeasedUntil):
			return nil
		case jobErr == nil && job.Status == domain.JobCompleted:
			if err := removeEntry(directories.processingFD, entry.Name(), info); err != nil {
				return fmt.Errorf("daemon: acknowledge recovered terminal event: %w", err)
			}
			return nil
		case jobErr == nil && job.Status == domain.JobFailed:
			if _, err := quarantineEntry(directories, directories.processingFD, entry.Name(), info, now); err != nil {
				return err
			}
			return nil
		case jobErr != nil && !errors.Is(jobErr, store.ErrNotFound):
			return fmt.Errorf("daemon: inspect recovered job: %w", jobErr)
		}
		if err := moveEntryNoReplace(directories.processingFD, entry.Name(), info, directories.incomingFD, entry.Name()); err != nil {
			if errors.Is(err, os.ErrExist) {
				if _, quarantineErr := quarantineEntry(directories, directories.processingFD, entry.Name(), info, now); quarantineErr != nil {
					return errors.Join(err, quarantineErr)
				}
				failures++
				return nil
			}
			return fmt.Errorf("daemon: recover spool event without replacement: %w", err)
		}
		return nil
	})
	if err != nil {
		return failures, false, fmt.Errorf("daemon: inspect processing spool: %w", err)
	}
	return failures, complete, nil
}

func excluded(workingDir string, configured []string) bool {
	if workingDir == "" {
		return false
	}
	canonicalWorkingDir, err := canonicalPath(workingDir)
	if err != nil {
		// Exclusions are a privacy and loop-prevention boundary. If the daemon
		// cannot establish where a captured working directory lives, processing it
		// would fail open across that boundary.
		return true
	}
	for _, path := range configured {
		if strings.TrimSpace(path) == "" {
			continue
		}
		canonicalConfiguredPath, err := canonicalPath(path)
		if err != nil {
			return true
		}
		if pathContains(canonicalConfiguredPath, canonicalWorkingDir) {
			return true
		}
	}
	for current := canonicalWorkingDir; ; current = filepath.Dir(current) {
		if _, err := os.Stat(filepath.Join(current, ".skillloop-ignore")); err == nil {
			return true
		} else if !errors.Is(err, os.ErrNotExist) {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return false
}

func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(absolute)
	missing := make([]string, 0, 4)
	for {
		evaluated, evaluateErr := filepath.EvalSymlinks(current)
		if evaluateErr == nil {
			for index := range slices.Backward(missing) {
				evaluated = filepath.Join(evaluated, missing[index])
			}
			return filepath.Clean(evaluated), nil
		}
		if !errors.Is(evaluateErr, os.ErrNotExist) {
			return "", evaluateErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", evaluateErr
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func pathContains(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func boundedError(err error) string {
	if err == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(err.Error()))
	return "error-" + hex.EncodeToString(sum[:8])
}
