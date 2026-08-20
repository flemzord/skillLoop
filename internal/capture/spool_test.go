package capture

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

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

func TestSpoolRefusesSymlinkedDataAndSpoolDirectories(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(t *testing.T, root, outside string) string
	}{
		{
			name: "data directory",
			setup: func(t *testing.T, root, outside string) string {
				t.Helper()
				path := filepath.Join(root, "data-link")
				if err := os.Symlink(outside, path); err != nil {
					t.Fatalf("symlink data directory: %v", err)
				}
				return path
			},
		},
		{
			name: "spool directory",
			setup: func(t *testing.T, root, outside string) string {
				t.Helper()
				path := filepath.Join(root, "data")
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("mkdir data directory: %v", err)
				}
				if err := os.Symlink(outside, filepath.Join(path, "spool")); err != nil {
					t.Fatalf("symlink spool directory: %v", err)
				}
				return path
			},
		},
		{
			name: "incoming directory",
			setup: func(t *testing.T, root, outside string) string {
				t.Helper()
				path := filepath.Join(root, "data")
				if err := os.MkdirAll(filepath.Join(path, "spool"), 0o700); err != nil {
					t.Fatalf("mkdir spool directory: %v", err)
				}
				if err := os.Symlink(outside, filepath.Join(path, "spool", "incoming")); err != nil {
					t.Fatalf("symlink incoming directory: %v", err)
				}
				return path
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			outside := filepath.Join(root, "outside")
			if err := os.Mkdir(outside, 0o755); err != nil {
				t.Fatalf("mkdir outside: %v", err)
			}
			dataDir := test.setup(t, root, outside)
			_, err := (Spool{DataDir: dataDir}).Write(domain.HookEvent{ID: "event", Source: domain.SourceCodex})
			if err == nil {
				t.Fatal("symlinked spool path was accepted")
			}
			info, statErr := os.Stat(outside)
			if statErr != nil {
				t.Fatalf("stat outside directory: %v", statErr)
			}
			if info.Mode().Perm() != 0o755 {
				t.Fatalf("outside directory permissions changed: mode=%v", info.Mode().Perm())
			}
			entries, readErr := os.ReadDir(outside)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("wrote through symlink: entries=%v err=%v", entries, readErr)
			}
		})
	}
}

func TestSpoolPublicationNeverReplacesAnExistingEvent(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	spool := Spool{DataDir: dataDir}
	first := domain.HookEvent{ID: "same-event", Source: domain.SourceCodex, Reason: "first"}
	path, err := spool.Write(first)
	if err != nil {
		t.Fatalf("write first event: %v", err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read first event: %v", err)
	}
	if _, err := spool.Write(domain.HookEvent{ID: first.ID, Source: domain.SourceClaude, Reason: "second"}); err == nil {
		t.Fatal("duplicate publication replaced an existing event")
	}
	contents, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(contents, original) {
		t.Fatalf("existing event changed: equal=%v err=%v", bytes.Equal(contents, original), err)
	}
}

func TestSpoolPublicationIsAtomicForConcurrentConsumer(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	published := make(chan struct{})
	releaseWriter := make(chan struct{})
	spool := Spool{
		DataDir: dataDir,
		afterPublish: func() {
			close(published)
			<-releaseWriter
		},
	}
	result := make(chan error, 1)
	go func() {
		_, err := spool.Write(domain.HookEvent{ID: "atomic-event", Source: domain.SourceCodex})
		result <- err
	}()
	<-published
	finalPath := filepath.Join(dataDir, "spool", "incoming", "atomic-event.json")
	fd, err := unix.Open(finalPath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		close(releaseWriter)
		t.Fatalf("concurrent consumer could not open published event: %v", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		close(releaseWriter)
		t.Fatalf("inspect concurrent publication: %v", err)
	}
	_ = unix.Close(fd)
	if stat.Nlink != 1 {
		close(releaseWriter)
		t.Fatalf("published event exposed with %d links, want exactly one", stat.Nlink)
	}
	entries, err := os.ReadDir(filepath.Dir(finalPath))
	if err != nil {
		close(releaseWriter)
		t.Fatalf("read concurrent spool: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(finalPath) {
		close(releaseWriter)
		t.Fatalf("atomic publication exposed source and destination: %#v", entries)
	}
	close(releaseWriter)
	if err := <-result; err != nil {
		t.Fatalf("complete atomic publication: %v", err)
	}
}

func TestSpoolRejectsUnsafeNamesAndOversizedEvents(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	for _, event := range []domain.HookEvent{
		{ID: "../escape", Source: domain.SourceCodex},
		{ID: "oversized", Source: domain.SourceCodex, Reason: strings.Repeat("x", int(MaxHookInputBytes))},
	} {
		if _, err := (Spool{DataDir: dataDir}).Write(event); err == nil {
			t.Fatalf("unsafe event %#v was accepted", event.ID)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dataDir), "escape.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe id escaped spool: %v", err)
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
