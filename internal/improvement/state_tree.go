package improvement

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func statePathParts(relative string) ([]string, error) {
	if relative == "" || filepath.IsAbs(relative) {
		return nil, ErrUnsafePath
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative)))
	if clean != filepath.ToSlash(relative) {
		return nil, ErrUnsafePath
	}
	parts := strings.Split(clean, "/")
	for _, part := range parts {
		if !safeStateComponent(part) {
			return nil, ErrUnsafePath
		}
	}
	return parts, nil
}

func openOrCreateStateDirectoryPath(root *os.File, relative string, create bool) (*os.File, error) {
	if root == nil {
		return nil, ErrUnsafePath
	}
	parts, err := statePathParts(relative)
	if err != nil {
		return nil, err
	}
	current, err := unix.Openat(int(root.Fd()), ".", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range parts {
		next, openErr := unix.Openat(current, part, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if create && errors.Is(openErr, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(current, part, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = unix.Close(current)
				return nil, mkdirErr
			}
			next, openErr = unix.Openat(current, part, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		}
		_ = unix.Close(current)
		if openErr != nil {
			return nil, openErr
		}
		current = next
	}
	file := os.NewFile(uintptr(current), relative)
	if file == nil {
		_ = unix.Close(current)
		return nil, errors.New("open state tree directory: invalid file descriptor")
	}
	return file, nil
}

func createStateRegularFile(root *os.File, relative string, mode uint32) (*os.File, error) {
	parts, err := statePathParts(relative)
	if err != nil {
		return nil, err
	}
	parent := root
	ownedParent := false
	if len(parts) > 1 {
		parent, err = openOrCreateStateDirectoryPath(root, strings.Join(parts[:len(parts)-1], "/"), true)
		if err != nil {
			return nil, err
		}
		ownedParent = true
	}
	if ownedParent {
		defer func() { _ = parent.Close() }()
	}
	name := parts[len(parts)-1]
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, mode)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), relative)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create state tree file: invalid file descriptor")
	}
	return file, nil
}

type stateTreeVisitor func(relative string, file *os.File, info os.FileInfo) error

const stateTreeBatchSize = 128

func walkStateTree(root *os.File, visit stateTreeVisitor) error {
	if root == nil || visit == nil {
		return ErrUnsafePath
	}
	fd, err := unix.Openat(int(root.Fd()), ".", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(fd), ".")
	if directory == nil {
		_ = unix.Close(fd)
		return errors.New("open state tree root: invalid file descriptor")
	}
	defer func() { _ = directory.Close() }()
	members := 0
	return walkStateTreeDirectory(directory, "", &members, visit)
}

func walkStateTreeDirectory(directory *os.File, prefix string, members *int, visit stateTreeVisitor) error {
	for {
		entries, readErr := directory.ReadDir(stateTreeBatchSize)
		for _, entry := range entries {
			*members++
			if *members > maxReleaseMembers {
				return fmt.Errorf("state tree contains more than %d members: %w", maxReleaseMembers, ErrResourceLimit)
			}
			name := entry.Name()
			if !safeStateComponent(name) {
				return ErrUnsafePath
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("state tree contains link or special file at %s: %w", name, ErrUnsafePath)
			}
			fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
			if errors.Is(err, unix.ENOTDIR) {
				fd, err = unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			}
			if err != nil {
				return err
			}
			child := os.NewFile(uintptr(fd), name)
			if child == nil {
				_ = unix.Close(fd)
				return errors.New("open state tree member: invalid file descriptor")
			}
			actual, statErr := child.Stat()
			if statErr != nil || (!actual.IsDir() && !actual.Mode().IsRegular()) {
				_ = child.Close()
				if statErr != nil {
					return statErr
				}
				return ErrUnsafePath
			}
			relative := name
			if prefix != "" {
				relative = prefix + "/" + name
			}
			if err := visit(relative, child, actual); err != nil {
				_ = child.Close()
				return err
			}
			if actual.IsDir() {
				if err := walkStateTreeDirectory(child, relative, members, visit); err != nil {
					_ = child.Close()
					return err
				}
			}
			if err := child.Close(); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func removeStateChildTree(parent *os.File, name string) error {
	if parent == nil || !safeStateComponent(name) {
		return ErrUnsafePath
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(fd), name)
	if directory == nil {
		_ = unix.Close(fd)
		return errors.New("open state tree for removal: invalid file descriptor")
	}
	if err := unix.Fchmod(fd, 0o700); err != nil {
		_ = directory.Close()
		return err
	}
	for {
		entries, err := readStateDirectoryBatch(directory, stateTreeBatchSize)
		if err != nil {
			_ = directory.Close()
			return err
		}
		if len(entries) == 0 {
			break
		}
		for _, entry := range entries {
			if !safeStateComponent(entry.Name()) {
				_ = directory.Close()
				return ErrUnsafePath
			}
			if entry.Type()&os.ModeSymlink == 0 {
				if err := removeStateChildTree(directory, entry.Name()); err == nil {
					continue
				} else if !errors.Is(err, unix.ENOTDIR) {
					_ = directory.Close()
					return err
				}
			}
			if err := unix.Unlinkat(int(directory.Fd()), entry.Name(), 0); err != nil {
				_ = directory.Close()
				return err
			}
		}
	}
	if err := directory.Close(); err != nil {
		return err
	}
	return unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
}

func readStateDirectoryBatch(directory *os.File, limit int) ([]os.DirEntry, error) {
	if directory == nil || limit <= 0 {
		return nil, ErrUnsafePath
	}
	fd, err := unix.Openat(int(directory.Fd()), ".", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	copy := os.NewFile(uintptr(fd), directory.Name())
	if copy == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open state directory batch: invalid file descriptor")
	}
	entries, readErr := copy.ReadDir(limit)
	closeErr := copy.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return entries, nil
}
