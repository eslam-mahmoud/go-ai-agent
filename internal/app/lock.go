package app

import (
	"errors"
	"fmt"
	"os"
	"sync"
)

var (
	ErrAlreadyLocked   = errors.New("madar instance already holds this lock")
	ErrLockUnsupported = errors.New("single-instance locking is unsupported on this platform")
)

type AlreadyLockedError struct {
	Path     string
	OwnerPID int
}

func (e *AlreadyLockedError) Error() string {
	if e.OwnerPID > 0 {
		return fmt.Sprintf("%s: %s (owner pid %d)", ErrAlreadyLocked, e.Path, e.OwnerPID)
	}
	return fmt.Sprintf("%s: %s", ErrAlreadyLocked, e.Path)
}

func (e *AlreadyLockedError) Unwrap() error {
	return ErrAlreadyLocked
}

type InstanceLock struct {
	path     string
	file     *os.File
	mu       sync.Mutex
	released bool
}

func AcquireInstanceLock(dbPath string) (*InstanceLock, error) {
	return acquireInstanceLock(dbPath + ".lock")
}

func (l *InstanceLock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

func (l *InstanceLock) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	l.released = true
	return releaseInstanceLock(l)
}
