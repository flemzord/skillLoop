package capture

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/flemzord/skillloop/internal/domain"
)

const MaxHookInputBytes int64 = 1 << 20

var ErrInputTooLarge = errors.New("hook input exceeds 1 MiB")

type Spool struct {
	DataDir string
	Now     func() time.Time
	NewID   func(time.Time) (string, error)
}

func ReadHookInput(reader io.Reader) ([]byte, error) {
	contents, err := io.ReadAll(io.LimitReader(reader, MaxHookInputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read hook input: %w", err)
	}
	if int64(len(contents)) > MaxHookInputBytes {
		return nil, ErrInputTooLarge
	}
	return contents, nil
}

func (spool Spool) Write(event domain.HookEvent) (string, error) {
	if spool.DataDir == "" {
		return "", errors.New("data directory is required")
	}
	now := time.Now
	if spool.Now != nil {
		now = spool.Now
	}
	capturedAt := now().UTC()
	if event.CapturedAt.IsZero() {
		event.CapturedAt = capturedAt
	}
	if event.SchemaVersion == 0 {
		event.SchemaVersion = 1
	}
	if event.ID == "" {
		newID := newUUIDv7
		if spool.NewID != nil {
			newID = spool.NewID
		}
		id, err := newID(capturedAt)
		if err != nil {
			return "", fmt.Errorf("generate event id: %w", err)
		}
		event.ID = id
	}

	incoming := filepath.Join(spool.DataDir, "spool", "incoming")
	if err := makePrivateDir(spool.DataDir); err != nil {
		return "", err
	}
	if err := makePrivateDir(filepath.Join(spool.DataDir, "spool")); err != nil {
		return "", err
	}
	if err := makePrivateDir(incoming); err != nil {
		return "", err
	}

	temporary, err := os.CreateTemp(incoming, ".skillloop-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create spool temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return "", fmt.Errorf("set spool file permissions: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	if err := encoder.Encode(event); err != nil {
		cleanup()
		return "", fmt.Errorf("encode spool event: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return "", fmt.Errorf("sync spool event: %w", err)
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return "", fmt.Errorf("close spool event: %w", err)
	}

	finalPath := filepath.Join(incoming, event.ID+".json")
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		cleanup()
		return "", fmt.Errorf("publish spool event: %w", err)
	}
	return finalPath, nil
}

func makePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create private directory %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("set private directory permissions %s: %w", path, err)
	}
	return nil
}

func newUUIDv7(now time.Time) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	milliseconds := uint64(now.UnixMilli())
	var timestamp [8]byte
	binary.BigEndian.PutUint64(timestamp[:], milliseconds)
	copy(value[0:6], timestamp[2:8])
	value[6] = (value[6] & 0x0f) | 0x70
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}
