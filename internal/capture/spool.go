package capture

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/flemzord/skillloop/internal/domain"
)

const MaxHookInputBytes int64 = 1 << 20

var ErrInputTooLarge = errors.New("hook input exceeds 1 MiB")

type Spool struct {
	DataDir string
	Now     func() time.Time
	NewID   func(time.Time) (string, error)
	// afterPublish is a deterministic concurrency seam for package tests. It
	// runs after the single atomic no-replace publication syscall.
	afterPublish func()
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
	if !safeEntryName(event.ID) {
		return "", errors.New("event id is not a safe spool filename")
	}
	var encoded bytes.Buffer
	if err := json.NewEncoder(&encoded).Encode(event); err != nil {
		return "", fmt.Errorf("encode spool event: %w", err)
	}
	if int64(encoded.Len()) > MaxHookInputBytes {
		return "", ErrInputTooLarge
	}

	absoluteDataDir, dataFD, err := openPrivateDataDir(spool.DataDir)
	if err != nil {
		return "", err
	}
	defer func() { _ = unix.Close(dataFD) }()
	spoolFD, err := openPrivateChild(dataFD, "spool")
	if err != nil {
		return "", fmt.Errorf("open spool directory: %w", err)
	}
	defer func() { _ = unix.Close(spoolFD) }()
	incomingFD, err := openPrivateChild(spoolFD, "incoming")
	if err != nil {
		return "", fmt.Errorf("open incoming spool directory: %w", err)
	}
	defer func() { _ = unix.Close(incomingFD) }()

	temporaryName, temporary, err := createTemporary(incomingFD)
	if err != nil {
		return "", fmt.Errorf("create spool temporary file: %w", err)
	}
	cleanup := func() {
		_ = temporary.Close()
		_ = unix.Unlinkat(incomingFD, temporaryName, 0)
	}
	if _, err := temporary.Write(encoded.Bytes()); err != nil {
		cleanup()
		return "", fmt.Errorf("write spool event: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return "", fmt.Errorf("sync spool event: %w", err)
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return "", fmt.Errorf("close spool event: %w", err)
	}

	finalName := event.ID + ".json"
	if err := renameNoReplace(incomingFD, temporaryName, incomingFD, finalName); err != nil {
		cleanup()
		return "", fmt.Errorf("publish spool event without replacement: %w", err)
	}
	if spool.afterPublish != nil {
		spool.afterPublish()
	}
	if err := unix.Fsync(incomingFD); err != nil {
		return "", fmt.Errorf("sync incoming spool directory: %w", err)
	}
	return filepath.Join(absoluteDataDir, "spool", "incoming", finalName), nil
}

func openPrivateDataDir(path string) (string, int, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", -1, fmt.Errorf("resolve data directory: %w", err)
	}
	absolute = filepath.Clean(absolute)
	if info, statErr := os.Lstat(absolute); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", -1, errors.New("data directory cannot be a symlink")
	}
	absolute, err = canonicalizeExistingAncestor(absolute)
	if err != nil {
		return "", -1, fmt.Errorf("anchor data directory: %w", err)
	}
	if absolute == string(filepath.Separator) {
		return "", -1, errors.New("data directory cannot be the filesystem root")
	}
	current, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", -1, fmt.Errorf("open filesystem root: %w", err)
	}
	components := strings.Split(strings.TrimPrefix(absolute, string(filepath.Separator)), string(filepath.Separator))
	for index, component := range components {
		if component == "" || component == "." || component == ".." {
			_ = unix.Close(current)
			return "", -1, errors.New("data directory contains an unsafe path component")
		}
		next, openErr := unix.Openat(current, component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if errors.Is(openErr, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(current, component, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = unix.Close(current)
				return "", -1, fmt.Errorf("create data directory component %q: %w", component, mkdirErr)
			}
			next, openErr = unix.Openat(current, component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		}
		_ = unix.Close(current)
		if openErr != nil {
			return "", -1, fmt.Errorf("open data directory component %q without symlinks: %w", component, openErr)
		}
		current = next
		if index == len(components)-1 {
			if err := unix.Fchmod(current, 0o700); err != nil {
				_ = unix.Close(current)
				return "", -1, fmt.Errorf("secure data directory: %w", err)
			}
		}
	}
	return absolute, current, nil
}

func canonicalizeExistingAncestor(path string) (string, error) {
	var missing []string
	current := path
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, resolveErr := filepath.EvalSymlinks(current)
			if resolveErr != nil {
				return "", resolveErr
			}
			for index := range slices.Backward(missing) {
				resolved = filepath.Join(resolved, missing[index])
			}
			return resolved, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("no existing data directory ancestor")
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func openPrivateChild(parent int, name string) (int, error) {
	if !safeEntryName(name) {
		return -1, errors.New("unsafe private directory name")
	}
	if err := unix.Mkdirat(parent, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, err
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	if err := unix.Fchmod(fd, 0o700); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func createTemporary(directoryFD int) (string, *os.File, error) {
	for range 100 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, err
		}
		name := ".skillloop-" + hex.EncodeToString(random[:]) + ".tmp"
		fd, err := unix.Openat(directoryFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		file := os.NewFile(uintptr(fd), name)
		if file == nil {
			_ = unix.Close(fd)
			return "", nil, errors.New("create spool temporary file: invalid file descriptor")
		}
		return name, file, nil
	}
	return "", nil, errors.New("create unique spool temporary file: exhausted attempts")
}

func safeEntryName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 200 &&
		!strings.ContainsRune(name, filepath.Separator) && filepath.Base(name) == name
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
