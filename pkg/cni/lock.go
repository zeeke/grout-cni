package cni

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const (
	defaultLockPath    = "/var/run/grout-k-cni.lock"
	lockAcquireTimeout = 60 * time.Second
)

type FileLock struct {
	fd int
}

func NewFileLock(path string) (*FileLock, error) {
	if path == "" {
		path = defaultLockPath
	}

	// The CNI binary is invoked fresh by kubelet/Multus on each call, so the
	// data directory may not exist yet on the node. Create it before opening
	// the lock file.
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating lock directory %s: %w", dir, err)
		}
	}

	// O_CLOEXEC ensures the kernel closes this FD on process exit,
	// which automatically releases the flock. This prevents lock
	// leaks when the CNI binary terminates unexpectedly.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CREAT|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening lock file %s: %w", path, err)
	}

	return &FileLock{fd: fd}, nil
}

func (l *FileLock) Lock() error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- unix.Flock(l.fd, unix.LOCK_EX)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("acquiring exclusive lock: %w", err)
		}
		return nil
	case <-time.After(lockAcquireTimeout):
		return fmt.Errorf("timed out waiting for lock after %v", lockAcquireTimeout)
	}
}

func (l *FileLock) Unlock() error {
	return unix.Flock(l.fd, unix.LOCK_UN)
}

func (l *FileLock) Close() error {
	return unix.Close(l.fd)
}
