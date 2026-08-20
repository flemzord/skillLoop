package config

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestPolicyLockGivesQueuedWriterPriority(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	firstReader, err := AcquirePolicyReadLock(ctx, path)
	if err != nil {
		t.Fatalf("acquire first reader: %v", err)
	}

	type writerOutcome struct {
		unlock func() error
		err    error
	}
	writerAcquired := make(chan writerOutcome, 1)
	go func() {
		unlock, lockErr := acquirePolicyWriteLock(ctx, path)
		writerAcquired <- writerOutcome{unlock: unlock, err: lockErr}
	}()
	waitForPolicyWriteGate(t, ctx, path)

	if err := firstReader(); err != nil {
		t.Fatalf("release first reader: %v", err)
	}
	writer := <-writerAcquired
	if writer.err != nil {
		t.Fatalf("acquire queued writer: %v", writer.err)
	}
	probeCtx, probeCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	_, err = AcquirePolicyReadLock(probeCtx, path)
	probeCancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("new reader bypassed active writer: %v", err)
	}
	if err := writer.unlock(); err != nil {
		t.Fatalf("release writer: %v", err)
	}

	reader, err := AcquirePolicyReadLock(ctx, path)
	if err != nil {
		t.Fatalf("acquire reader after writer: %v", err)
	}
	if err := reader(); err != nil {
		t.Fatalf("release final reader: %v", err)
	}
}

func waitForPolicyWriteGate(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
		unlock, err := AcquirePolicyReadLock(probeCtx, path)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			return
		}
		if err != nil {
			t.Fatalf("probe policy write gate: %v", err)
		}
		if err := unlock(); err != nil {
			t.Fatalf("release policy write gate probe: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for policy write gate: %v", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
