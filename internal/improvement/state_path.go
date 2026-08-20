package improvement

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// stateDirectory is an opened, identity-stable directory below StateDir. Its
// ancestor descriptors let callers prove that its public path still names the
// same inode before returning or persisting that path.
type stateDirectory struct {
	path    string
	file    *os.File
	anchors []statePathAnchor
}

type statePathAnchor struct {
	parentFD int
	name     string
	device   uint64
	inode    uint64
}

func (directory *stateDirectory) Close() error {
	if directory == nil {
		return nil
	}
	var closeErr error
	if directory.file != nil {
		closeErr = errors.Join(closeErr, directory.file.Close())
		directory.file = nil
	}
	for _, anchor := range directory.anchors {
		closeErr = errors.Join(closeErr, unix.Close(anchor.parentFD))
	}
	directory.anchors = nil
	return closeErr
}

func (directory *stateDirectory) verifyIdentity() error {
	if directory == nil || directory.file == nil || len(directory.anchors) == 0 {
		return ErrUnsafePath
	}
	for _, anchor := range directory.anchors {
		var stat unix.Stat_t
		if err := unix.Fstatat(anchor.parentFD, anchor.name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("verify state path identity: %w", errors.Join(err, ErrUnsafePath))
		}
		if uint64(stat.Dev) != anchor.device || stat.Ino != anchor.inode || stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			return fmt.Errorf("state path identity changed at %q: %w", anchor.name, ErrDrift)
		}
	}
	return nil
}

func verifyStateChildIdentity(parent *stateDirectory, name string, child *os.File) error {
	if parent == nil || parent.file == nil || child == nil || !safeStateComponent(name) {
		return ErrUnsafePath
	}
	if len(parent.anchors) > 0 {
		if err := parent.verifyIdentity(); err != nil {
			return err
		}
	}
	var named, opened unix.Stat_t
	if err := unix.Fstatat(int(parent.file.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("verify state child name: %w", errors.Join(err, ErrUnsafePath))
	}
	if err := unix.Fstat(int(child.Fd()), &opened); err != nil {
		return fmt.Errorf("verify opened state child: %w", err)
	}
	if uint64(named.Dev) != uint64(opened.Dev) || named.Ino != opened.Ino || named.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("state child identity changed at %q: %w", name, ErrDrift)
	}
	return nil
}

func openStateDirectory(stateDir string, components ...string) (*stateDirectory, error) {
	if strings.TrimSpace(stateDir) == "" {
		return nil, errors.New("state directory is required")
	}
	absolute, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, fmt.Errorf("resolve state directory: %w", err)
	}
	absolute = filepath.Clean(absolute)
	if absolute == string(filepath.Separator) {
		return nil, fmt.Errorf("state directory cannot be the filesystem root: %w", ErrUnsafePath)
	}
	absolute, err = canonicalizeStateParent(absolute)
	if err != nil {
		return nil, fmt.Errorf("anchor state directory parent: %w", err)
	}
	logicalRootComponents := strings.Split(strings.TrimPrefix(absolute, string(filepath.Separator)), string(filepath.Separator))
	logicalRootIndex := len(logicalRootComponents) - 1
	for _, component := range components {
		if !safeStateComponent(component) {
			return nil, fmt.Errorf("unsafe state directory component %q: %w", component, ErrUnsafePath)
		}
		absolute = filepath.Join(absolute, component)
	}

	current, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open filesystem root: %w", err)
	}
	var anchors []statePathAnchor
	closeTraversal := func() {
		_ = unix.Close(current)
		for _, anchor := range anchors {
			_ = unix.Close(anchor.parentFD)
		}
	}
	pathComponents := strings.Split(strings.TrimPrefix(absolute, string(filepath.Separator)), string(filepath.Separator))
	for index, component := range pathComponents {
		if !safeStateComponent(component) {
			closeTraversal()
			return nil, fmt.Errorf("unsafe state path component %q: %w", component, ErrUnsafePath)
		}
		next, openErr := unix.Openat(current, component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if errors.Is(openErr, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(current, component, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				closeTraversal()
				return nil, fmt.Errorf("create state directory component %q: %w", component, mkdirErr)
			}
			next, openErr = unix.Openat(current, component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			closeTraversal()
			return nil, fmt.Errorf("open state directory component %q without symlinks: %w", component, errors.Join(openErr, ErrUnsafePath))
		}
		var stat unix.Stat_t
		if statErr := unix.Fstat(next, &stat); statErr != nil {
			_ = unix.Close(next)
			closeTraversal()
			return nil, fmt.Errorf("inspect state directory component %q: %w", component, statErr)
		}
		anchors = append(anchors, statePathAnchor{parentFD: current, name: component, device: uint64(stat.Dev), inode: stat.Ino})
		current = next
		if index >= logicalRootIndex {
			if chmodErr := unix.Fchmod(current, 0o700); chmodErr != nil {
				closeTraversal()
				return nil, fmt.Errorf("secure state directory: %w", chmodErr)
			}
		}
	}
	file := os.NewFile(uintptr(current), absolute)
	if file == nil {
		closeTraversal()
		return nil, errors.New("open state directory: invalid file descriptor")
	}
	directory := &stateDirectory{path: absolute, file: file, anchors: anchors}
	if err := directory.verifyIdentity(); err != nil {
		_ = directory.Close()
		return nil, err
	}
	return directory, nil
}

func canonicalizeStateParent(path string) (string, error) {
	finalName := filepath.Base(path)
	current := filepath.Dir(path)
	var missing []string
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, resolveErr := filepath.EvalSymlinks(current)
			if resolveErr != nil {
				return "", resolveErr
			}
			for index := range slices.Backward(missing) {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Join(resolved, finalName), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("no existing state directory parent")
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func safeStateComponent(value string) bool {
	return value != "" && value != "." && value != ".." && len(value) <= 255 &&
		filepath.Base(value) == value && !strings.ContainsRune(value, filepath.Separator)
}

func createStateChild(directory *stateDirectory, prefix string) (string, string, error) {
	if directory == nil || directory.file == nil || !safeStateComponent(prefix) {
		return "", "", ErrUnsafePath
	}
	for range 100 {
		suffix, err := randomSuffix()
		if err != nil {
			return "", "", err
		}
		name := prefix + suffix
		if err := unix.Mkdirat(int(directory.file.Fd()), name, 0o700); errors.Is(err, unix.EEXIST) {
			continue
		} else if err != nil {
			return "", "", fmt.Errorf("create state child: %w", err)
		}
		return name, filepath.Join(directory.path, name), nil
	}
	return "", "", errors.New("create unique state child: exhausted attempts")
}

func openStateChild(directory *stateDirectory, name string) (*os.File, error) {
	if directory == nil || directory.file == nil || !safeStateComponent(name) {
		return nil, ErrUnsafePath
	}
	fd, err := unix.Openat(int(directory.file.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, err
		}
		return nil, fmt.Errorf("open state child without symlinks: %w", errors.Join(err, ErrUnsafePath))
	}
	file := os.NewFile(uintptr(fd), filepath.Join(directory.path, name))
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open state child: invalid file descriptor")
	}
	return file, nil
}

func openStateFile(directory *stateDirectory, relative string, flags int) (*os.File, error) {
	if directory == nil || directory.file == nil || relative == "" || filepath.IsAbs(relative) {
		return nil, ErrUnsafePath
	}
	parts := strings.Split(filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative))), "/")
	if strings.Join(parts, "/") != filepath.ToSlash(relative) {
		return nil, ErrUnsafePath
	}
	current, err := unix.Dup(int(directory.file.Fd()))
	if err != nil {
		return nil, err
	}
	for _, component := range parts[:len(parts)-1] {
		if !safeStateComponent(component) {
			_ = unix.Close(current)
			return nil, ErrUnsafePath
		}
		next, openErr := unix.Openat(current, component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		_ = unix.Close(current)
		if openErr != nil {
			return nil, fmt.Errorf("open state file parent without symlinks: %w", openErr)
		}
		current = next
	}
	name := parts[len(parts)-1]
	if !safeStateComponent(name) {
		_ = unix.Close(current)
		return nil, ErrUnsafePath
	}
	fd, openErr := unix.Openat(current, name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	_ = unix.Close(current)
	if openErr != nil {
		return nil, openErr
	}
	file := os.NewFile(uintptr(fd), filepath.Join(directory.path, filepath.FromSlash(relative)))
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open state file: invalid file descriptor")
	}
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if statErr != nil {
			return nil, statErr
		}
		return nil, ErrUnsafePath
	}
	return file, nil
}

func readOpenFileLimit(file *os.File, limit int64) ([]byte, os.FileInfo, error) {
	if file == nil || limit <= 0 {
		return nil, nil, ErrResourceLimit
	}
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > limit {
		return nil, nil, ErrResourceLimit
	}
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(contents)) > limit {
		return nil, nil, ErrResourceLimit
	}
	return contents, info, nil
}
