//go:build unix

package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func acquireInstanceLock(path string) (*InstanceLock, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create lock directory %s: %w", dir, err)
		}
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open instance lock %s: %w", path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, fmt.Errorf("secure instance lock %s: %w", path, err)
	}

	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			ownerPID := readOwnerPID(file)
			file.Close()
			return nil, &AlreadyLockedError{Path: path, OwnerPID: ownerPID}
		}
		file.Close()
		return nil, fmt.Errorf("acquire instance lock %s: %w", path, err)
	}

	if err := writeOwnerPID(file, os.Getpid()); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
		return nil, fmt.Errorf("write instance lock owner %s: %w", path, err)
	}
	return &InstanceLock{path: path, file: file}, nil
}

func releaseInstanceLock(lock *InstanceLock) error {
	unlockErr := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	closeErr := lock.file.Close()
	return errors.Join(unlockErr, closeErr)
}

func readOwnerPID(file *os.File) int {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0
	}
	data, err := io.ReadAll(io.LimitReader(file, 64))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}

func writeOwnerPID(file *os.File, pid int) error {
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(file, "%d\n", pid); err != nil {
		return err
	}
	return file.Sync()
}
