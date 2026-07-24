//go:build unix

package app

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestInstanceLockRejectsConcurrentOwner(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "madar.db")
	first, err := AcquireInstanceLock(dbPath)
	if err != nil {
		t.Fatalf("first AcquireInstanceLock: %v", err)
	}
	defer first.Release()

	second, err := AcquireInstanceLock(dbPath)
	if err == nil {
		second.Release()
		t.Fatal("second AcquireInstanceLock succeeded while first lock is held")
	}
	if !errors.Is(err, ErrAlreadyLocked) {
		t.Errorf("error = %v, want ErrAlreadyLocked", err)
	}
	var lockedErr *AlreadyLockedError
	if !errors.As(err, &lockedErr) {
		t.Fatalf("error type = %T, want *AlreadyLockedError", err)
	}
	if lockedErr.Path != dbPath+".lock" {
		t.Errorf("lock path = %q, want %q", lockedErr.Path, dbPath+".lock")
	}
	if lockedErr.OwnerPID != os.Getpid() {
		t.Errorf("owner PID = %d, want %d", lockedErr.OwnerPID, os.Getpid())
	}
	if !containsPID(err.Error(), os.Getpid()) {
		t.Errorf("error %q does not report owner PID", err)
	}
}

func TestInstanceLockReleaseAllowsReacquire(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "madar.db")
	first, err := AcquireInstanceLock(dbPath)
	if err != nil {
		t.Fatalf("first AcquireInstanceLock: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	second, err := AcquireInstanceLock(dbPath)
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}

	if _, err := os.Stat(dbPath + ".lock"); err != nil {
		t.Errorf("stable lock file was removed on release: %v", err)
	}
}

func TestInstanceLocksAreIndependentPerDatabase(t *testing.T) {
	dir := t.TempDir()
	first, err := AcquireInstanceLock(filepath.Join(dir, "first.db"))
	if err != nil {
		t.Fatalf("lock first DB: %v", err)
	}
	defer first.Release()

	second, err := AcquireInstanceLock(filepath.Join(dir, "second.db"))
	if err != nil {
		t.Fatalf("lock second DB: %v", err)
	}
	defer second.Release()
}

func TestInstanceLockCreatesParentDirectory(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nested", "state", "madar.db")
	lock, err := AcquireInstanceLock(dbPath)
	if err != nil {
		t.Fatalf("AcquireInstanceLock: %v", err)
	}
	defer lock.Release()

	info, err := os.Stat(dbPath + ".lock")
	if err != nil {
		t.Fatalf("stat lock file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("lock mode = %o, want 600", info.Mode().Perm())
	}
}

func containsPID(message string, pid int) bool {
	return strings.Contains(message, strconv.Itoa(pid))
}
