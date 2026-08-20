package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/flemzord/skillloop/internal/capture"
)

var errUnsafeSpoolEntry = errors.New("daemon: unsafe spool entry")

type spoolDirectories struct {
	incoming, processing, failed       string
	incomingFD, processingFD, failedFD int
	dataFD, spoolFD                    int
}

func (directories *spoolDirectories) close() {
	for _, fd := range []int{
		directories.incomingFD, directories.processingFD, directories.failedFD,
		directories.spoolFD, directories.dataFD,
	} {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
	}
}

func ensureSpoolDirectories(dataDir string) (spoolDirectories, error) {
	if dataDir == "" {
		return spoolDirectories{}, errors.New("daemon: data directory is required")
	}
	absolute, dataFD, err := openPrivateDataDir(dataDir)
	if err != nil {
		return spoolDirectories{}, fmt.Errorf("daemon: secure data directory: %w", err)
	}
	directories := spoolDirectories{
		dataFD: dataFD, spoolFD: -1, incomingFD: -1, processingFD: -1, failedFD: -1,
	}
	fail := func(cause error) (spoolDirectories, error) {
		directories.close()
		return spoolDirectories{}, cause
	}
	directories.spoolFD, err = openPrivateChild(dataFD, "spool")
	if err != nil {
		return fail(fmt.Errorf("daemon: open spool directory: %w", err))
	}
	directories.incomingFD, err = openPrivateChild(directories.spoolFD, "incoming")
	if err != nil {
		return fail(fmt.Errorf("daemon: open incoming spool: %w", err))
	}
	directories.processingFD, err = openPrivateChild(directories.spoolFD, "processing")
	if err != nil {
		return fail(fmt.Errorf("daemon: open processing spool: %w", err))
	}
	directories.failedFD, err = openPrivateChild(directories.spoolFD, "failed")
	if err != nil {
		return fail(fmt.Errorf("daemon: open failed spool: %w", err))
	}
	root := filepath.Join(absolute, "spool")
	directories.incoming = filepath.Join(root, "incoming")
	directories.processing = filepath.Join(root, "processing")
	directories.failed = filepath.Join(root, "failed")
	return directories, nil
}

// openPrivateDataDir walks the absolute path through directory descriptors.
// O_NOFOLLOW on every component prevents an attacker-controlled ancestor from
// redirecting chmod or spool I/O outside the configured data directory.
func openPrivateDataDir(path string) (string, int, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", -1, err
	}
	absolute = filepath.Clean(absolute)
	if info, statErr := os.Lstat(absolute); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", -1, errors.New("data directory cannot be a symlink")
	}
	absolute, err = canonicalizeExistingAncestor(absolute)
	if err != nil {
		return "", -1, err
	}
	if absolute == string(filepath.Separator) {
		return "", -1, errors.New("data directory cannot be the filesystem root")
	}
	current, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", -1, err
	}
	components := strings.Split(strings.TrimPrefix(absolute, string(filepath.Separator)), string(filepath.Separator))
	for index, component := range components {
		if !safeSpoolName(component) {
			_ = unix.Close(current)
			return "", -1, errors.New("unsafe data directory component")
		}
		next, openErr := unix.Openat(current, component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if errors.Is(openErr, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(current, component, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = unix.Close(current)
				return "", -1, mkdirErr
			}
			next, openErr = unix.Openat(current, component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		}
		_ = unix.Close(current)
		if openErr != nil {
			return "", -1, fmt.Errorf("open component %q without symlinks: %w", component, openErr)
		}
		current = next
		if index == len(components)-1 {
			if err := unix.Fchmod(current, 0o700); err != nil {
				_ = unix.Close(current)
				return "", -1, err
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
	if !safeSpoolName(name) {
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

const spoolDirectoryBatchSize = 128

func walkDirectory(directoryFD int, name string, limit int, visit func(os.DirEntry) error) (bool, error) {
	if limit <= 0 {
		return false, errors.New("directory walk limit must be positive")
	}
	duplicate, err := unix.Openat(directoryFD, ".", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false, err
	}
	file := os.NewFile(uintptr(duplicate), name)
	if file == nil {
		_ = unix.Close(duplicate)
		return false, errors.New("invalid directory file descriptor")
	}
	defer func() { _ = file.Close() }()
	examined := 0
	for examined < limit {
		entries, readErr := file.ReadDir(min(spoolDirectoryBatchSize, limit-examined))
		sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })
		for _, entry := range entries {
			examined++
			if visitErr := visit(entry); visitErr != nil {
				return false, visitErr
			}
		}
		if errors.Is(readErr, io.EOF) {
			return true, nil
		}
		if readErr != nil {
			return false, readErr
		}
		if len(entries) == 0 {
			return false, io.ErrNoProgress
		}
	}
	return false, nil
}

func openSpoolEntry(directoryFD int, name string) (*os.File, os.FileInfo, error) {
	if !safeSpoolName(name) || filepath.Ext(name) != ".json" {
		return nil, nil, fmt.Errorf("%w: invalid name", errUnsafeSpoolEntry)
	}
	fd, err := unix.Openat(directoryFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: open without following links: %w", errUnsafeSpoolEntry, err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, nil, fmt.Errorf("%w: inspect descriptor: %w", errUnsafeSpoolEntry, err)
	}
	if stat.Nlink != 1 {
		_ = unix.Close(fd)
		return nil, nil, fmt.Errorf("%w: regular spool entry must have exactly one link", errUnsafeSpoolEntry)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, nil, fmt.Errorf("%w: invalid file descriptor", errUnsafeSpoolEntry)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%w: inspect: %w", errUnsafeSpoolEntry, err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%w: type %s", errUnsafeSpoolEntry, info.Mode().Type())
	}
	if info.Size() < 0 || info.Size() > capture.MaxHookInputBytes {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%w: size %d exceeds %d", errUnsafeSpoolEntry, info.Size(), capture.MaxHookInputBytes)
	}
	return file, info, nil
}

func readSpoolEntry(file *os.File, directoryFD int, name string, expected os.FileInfo) ([]byte, error) {
	contents, err := io.ReadAll(io.LimitReader(file, capture.MaxHookInputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read bounded spool entry: %w", err)
	}
	if int64(len(contents)) > capture.MaxHookInputBytes {
		return nil, fmt.Errorf("%w: content exceeds capture limit", errUnsafeSpoolEntry)
	}
	if err := verifyEntryIdentity(directoryFD, name, expected); err != nil {
		return nil, err
	}
	return contents, nil
}

func verifyEntryIdentity(directoryFD int, name string, expected os.FileInfo) error {
	fd, err := unix.Openat(directoryFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("spool entry changed while open: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return errors.New("spool entry changed while open: invalid file descriptor")
	}
	current, statErr := file.Stat()
	_ = file.Close()
	if statErr != nil {
		return fmt.Errorf("spool entry changed while open: %w", statErr)
	}
	if !os.SameFile(expected, current) {
		return errors.New("spool entry changed while open")
	}
	return nil
}

func removeEntry(directoryFD int, name string, expected os.FileInfo) error {
	if expected != nil {
		if err := verifyEntryIdentity(directoryFD, name, expected); err != nil {
			return err
		}
	}
	if err := unix.Unlinkat(directoryFD, name, 0); err != nil {
		return err
	}
	return unix.Fsync(directoryFD)
}

func moveEntryNoReplace(sourceFD int, sourceName string, expected os.FileInfo, destinationFD int, destinationName string) error {
	if !safeSpoolName(sourceName) || !safeSpoolName(destinationName) {
		return errors.New("unsafe spool move name")
	}
	if expected != nil {
		if err := verifyEntryIdentity(sourceFD, sourceName, expected); err != nil {
			return err
		}
	}
	if err := renameNoReplace(sourceFD, sourceName, destinationFD, destinationName); err != nil {
		return err
	}
	return errors.Join(unix.Fsync(sourceFD), unix.Fsync(destinationFD))
}

func quarantineEntry(
	directories spoolDirectories,
	sourceFD int,
	sourceName string,
	expected os.FileInfo,
	quarantinedAt time.Time,
) (string, error) {
	for range 32 {
		var suffix [12]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", fmt.Errorf("generate quarantine name: %w", err)
		}
		extension := filepath.Ext(sourceName)
		stem := strings.TrimSuffix(sourceName, extension)
		uniqueSuffix := fmt.Sprintf(".failed-%016x-%s%s", quarantinedAt.UnixNano(), hex.EncodeToString(suffix[:]), extension)
		if maximumStem := 255 - len(uniqueSuffix); len(stem) > maximumStem {
			stem = stem[:maximumStem]
		}
		candidate := stem + uniqueSuffix
		err := moveEntryNoReplace(sourceFD, sourceName, expected, directories.failedFD, candidate)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("quarantine spool entry without replacement: %w", err)
		}
		return candidate, nil
	}
	return "", errors.New("quarantine spool entry: unique name attempts exhausted")
}

func quarantineTimestamp(name string) (time.Time, bool) {
	extension := filepath.Ext(name)
	stem := strings.TrimSuffix(name, extension)
	marker := strings.LastIndex(stem, ".failed-")
	if marker < 0 {
		return time.Time{}, false
	}
	metadata := stem[marker+len(".failed-"):]
	separator := strings.LastIndexByte(metadata, '-')
	if separator < 1 {
		return time.Time{}, false
	}
	timestamp, err := strconv.ParseInt(metadata[:separator], 16, 64)
	if err != nil || timestamp <= 0 {
		return time.Time{}, false
	}
	randomSuffix := metadata[separator+1:]
	decoded, err := hex.DecodeString(randomSuffix)
	if err != nil || len(decoded) != 12 {
		return time.Time{}, false
	}
	return time.Unix(0, timestamp).UTC(), true
}

func safeSpoolName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 255 &&
		!strings.ContainsRune(name, filepath.Separator) && filepath.Base(name) == name
}
