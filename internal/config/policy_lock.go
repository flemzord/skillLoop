package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	policyLockSuffix = ".policy.lock"
	policyGateSuffix = ".policy.gate.lock"
)

// AcquirePolicyReadLock serializes an automatic policy decision with config
// writers. Callers must hold the returned lock through the complete mutation
// authorized by the configuration they subsequently load.
func AcquirePolicyReadLock(ctx context.Context, path string) (func() error, error) {
	path, err := policyPath(path)
	if err != nil {
		return nil, err
	}
	gateUnlock, err := acquirePolicyLock(ctx, path, policyGateSuffix, unix.LOCK_SH)
	if err != nil {
		return nil, err
	}
	policyUnlock, err := acquirePolicyLock(ctx, path, policyLockSuffix, unix.LOCK_SH)
	if err != nil {
		_ = gateUnlock()
		return nil, err
	}
	if err := gateUnlock(); err != nil {
		_ = policyUnlock()
		return nil, fmt.Errorf("release config policy read gate: %w", err)
	}
	return policyUnlock, nil
}

func acquirePolicyWriteLock(ctx context.Context, path string) (func() error, error) {
	path, err := policyPath(path)
	if err != nil {
		return nil, err
	}
	// The exclusive gate stays held while the writer waits for existing policy
	// readers. New readers cannot starve a queued mode or policy change.
	gateUnlock, err := acquirePolicyLock(ctx, path, policyGateSuffix, unix.LOCK_EX)
	if err != nil {
		return nil, err
	}
	policyUnlock, err := acquirePolicyLock(ctx, path, policyLockSuffix, unix.LOCK_EX)
	if err != nil {
		_ = gateUnlock()
		return nil, err
	}
	var once sync.Once
	var unlockErr error
	return func() error {
		once.Do(func() {
			unlockErr = errors.Join(policyUnlock(), gateUnlock())
		})
		return unlockErr
	}, nil
}

func policyPath(path string) (string, error) {
	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return "", err
		}
	}
	return filepath.Clean(path), nil
}

func acquirePolicyLock(ctx context.Context, path, suffix string, operation int) (func() error, error) {
	lockPath := path + suffix
	fd, err := unix.Open(lockPath, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open config policy lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), lockPath)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open config policy lock: invalid file descriptor")
	}
	fail := func(cause error) (func() error, error) {
		_ = file.Close()
		return nil, cause
	}
	info, err := file.Stat()
	if err != nil {
		return fail(fmt.Errorf("inspect config policy lock: %w", err))
	}
	if !info.Mode().IsRegular() {
		return fail(errors.New("config policy lock is not a regular file"))
	}
	if err := file.Chmod(0o600); err != nil {
		return fail(fmt.Errorf("secure config policy lock: %w", err))
	}

	for {
		err = unix.Flock(fd, operation|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			return fail(fmt.Errorf("acquire config policy lock: %w", err))
		}
		select {
		case <-ctx.Done():
			return fail(fmt.Errorf("acquire config policy lock: %w", ctx.Err()))
		case <-time.After(10 * time.Millisecond):
		}
	}

	var once sync.Once
	var unlockErr error
	return func() error {
		once.Do(func() {
			unlockErr = errors.Join(unix.Flock(fd, unix.LOCK_UN), file.Close())
		})
		return unlockErr
	}, nil
}
