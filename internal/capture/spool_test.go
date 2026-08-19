package capture

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/flemzord/skillloop/internal/domain"
)

func TestReadHookInputLimit(t *testing.T) {
	withinLimit := bytes.Repeat([]byte("x"), int(MaxHookInputBytes))
	contents, err := ReadHookInput(bytes.NewReader(withinLimit))
	if err != nil {
		t.Fatalf("read input at limit: %v", err)
	}
	if len(contents) != len(withinLimit) {
		t.Fatalf("unexpected input length: %d", len(contents))
	}

	overLimit := bytes.Repeat([]byte("x"), int(MaxHookInputBytes)+1)
	if _, err := ReadHookInput(bytes.NewReader(overLimit)); !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("expected ErrInputTooLarge, got %v", err)
	}
}

func TestSpoolWriteIsAtomicAndPrivate(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	capturedAt := time.Date(2026, time.August, 20, 12, 34, 56, 0, time.UTC)
	spool := Spool{
		DataDir: dataDir,
		Now:     func() time.Time { return capturedAt },
		NewID:   func(time.Time) (string, error) { return "event-1", nil },
	}
	path, err := spool.Write(domain.HookEvent{
		Source:        domain.SourceCodex,
		SessionID:     "session-1",
		WorkingDir:    "/workspace",
		HookEventName: "stop",
	})
	if err != nil {
		t.Fatalf("write event: %v", err)
	}
	if filepath.Base(path) != "event-1.json" {
		t.Fatalf("unexpected spool path: %s", path)
	}

	for _, directory := range []string{
		dataDir,
		filepath.Join(dataDir, "spool"),
		filepath.Join(dataDir, "spool", "incoming"),
	} {
		assertMode(t, directory, 0o700)
	}
	assertMode(t, path, 0o600)

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read incoming spool: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "event-1.json" {
		t.Fatalf("temporary file leaked: %#v", entries)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	event := domain.HookEvent{}
	if err := json.Unmarshal(contents, &event); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if event.SchemaVersion != 1 || event.ID != "event-1" || !event.CapturedAt.Equal(capturedAt) {
		t.Fatalf("unexpected event metadata: %#v", event)
	}
}

func TestSpoolConcurrentWritersDoNotCorruptEvents(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	spool := Spool{DataDir: dataDir}
	const writers = 50
	errorsChannel := make(chan error, writers)
	wait := sync.WaitGroup{}
	for index := range writers {
		wait.Go(func() {
			_, err := spool.Write(domain.HookEvent{
				Source:        domain.SourceCodex,
				SessionID:     "session",
				TurnID:        string(rune(index)),
				WorkingDir:    "/workspace",
				HookEventName: "stop",
			})
			errorsChannel <- err
		})
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatalf("concurrent write: %v", err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(dataDir, "spool", "incoming"))
	if err != nil {
		t.Fatalf("read incoming spool: %v", err)
	}
	if len(entries) != writers {
		t.Fatalf("expected %d events, got %d", writers, len(entries))
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			t.Fatalf("unexpected temporary file: %s", entry.Name())
		}
		contents, readErr := os.ReadFile(filepath.Join(dataDir, "spool", "incoming", entry.Name()))
		if readErr != nil {
			t.Fatalf("read event %s: %v", entry.Name(), readErr)
		}
		if !json.Valid(contents) {
			t.Fatalf("invalid event %s: %s", entry.Name(), contents)
		}
	}
}

func assertMode(t *testing.T, path string, expected os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if actual := info.Mode().Perm(); actual != expected {
		t.Fatalf("unexpected permissions for %s: got %o, want %o", path, actual, expected)
	}
}
