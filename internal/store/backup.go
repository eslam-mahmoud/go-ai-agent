package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrBackupExists      = errors.New("restore target already exists")
	ErrBackupUnreadable  = errors.New("backup is not a readable Madar database")
	ErrBackupTooNew      = errors.New("backup is newer than this binary supports")
	ErrInvalidBackupPath = errors.New("invalid backup path")
)

// BackupInfo describes a backup so an operator can tell backups apart without
// opening them by hand.
type BackupInfo struct {
	Path          string
	SchemaVersion int
	Projects      int
	Tasks         int
	Executions    int
	Bytes         int64
}

// CurrentSchemaVersion is the newest migration this binary knows.
func CurrentSchemaVersion() int {
	newest := 0
	for _, migration := range migrations {
		if migration.version > newest {
			newest = migration.version
		}
	}
	return newest
}

// Backup writes a consistent snapshot to path. It uses SQLite's own VACUUM
// INTO rather than copying the file, because a live database has a
// write-ahead log and a naive copy can capture a torn state.
func (s *Store) Backup(path string) (*BackupInfo, error) {
	target, err := prepareBackupPath(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(target); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrBackupExists, target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect backup target: %w", err)
	}
	if _, err := s.db.Exec(`VACUUM INTO ?`, target); err != nil {
		return nil, fmt.Errorf("write backup: %w", err)
	}
	return InspectBackup(target)
}

// InspectBackup verifies a backup and reports what it contains. A file that
// is not a readable Madar database is rejected here rather than at restore.
func InspectBackup(path string) (*BackupInfo, error) {
	target, err := prepareBackupPath(path)
	if err != nil {
		return nil, err
	}
	stat, err := os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBackupUnreadable, err)
	}
	backup, err := OpenReadOnly(target)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBackupUnreadable, err)
	}
	defer backup.Close()

	info := &BackupInfo{Path: target, Bytes: stat.Size()}
	if err := backup.db.QueryRow(
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`,
	).Scan(&info.SchemaVersion); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBackupUnreadable, err)
	}
	if info.SchemaVersion == 0 {
		return nil, fmt.Errorf("%w: no schema is recorded", ErrBackupUnreadable)
	}
	counts := []struct {
		table string
		into  *int
	}{
		{"projects", &info.Projects},
		{"project_tasks", &info.Tasks},
		{"executions", &info.Executions},
	}
	for _, count := range counts {
		if err := backup.db.QueryRow(
			fmt.Sprintf(`SELECT COUNT(*) FROM %s`, count.table),
		).Scan(count.into); err != nil {
			// A missing v2 table means an old backup, not an unreadable one.
			if isMissingTable(err) {
				continue
			}
			return nil, fmt.Errorf("%w: %v", ErrBackupUnreadable, err)
		}
	}
	return info, nil
}

// RestoreOptions controls how a backup replaces a database.
type RestoreOptions struct {
	// Overwrite permits replacing an existing database. Without it a restore
	// refuses rather than silently destroying current state.
	Overwrite bool
}

// Restore copies a verified backup into place. The restored database is
// opened afterwards, which migrates it forward to the current schema.
func Restore(backupPath, targetPath string, options RestoreOptions) (*BackupInfo, error) {
	info, err := InspectBackup(backupPath)
	if err != nil {
		return nil, err
	}
	if info.SchemaVersion > CurrentSchemaVersion() {
		// A forward migration cannot be undone, so refuse rather than corrupt.
		return nil, fmt.Errorf(
			"%w: backup is schema v%d, this binary supports v%d",
			ErrBackupTooNew,
			info.SchemaVersion,
			CurrentSchemaVersion(),
		)
	}
	target, err := prepareBackupPath(targetPath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(target); err == nil {
		if !options.Overwrite {
			return nil, fmt.Errorf("%w: %s", ErrBackupExists, target)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect restore target: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, fmt.Errorf("create restore directory: %w", err)
	}
	content, err := os.ReadFile(info.Path)
	if err != nil {
		return nil, fmt.Errorf("read backup: %w", err)
	}
	// Write beside the target and rename, so an interrupted restore cannot
	// leave a half-written database in place.
	temporary := target + ".restoring"
	if err := os.WriteFile(temporary, content, 0o600); err != nil {
		return nil, fmt.Errorf("write restored database: %w", err)
	}
	if err := os.Rename(temporary, target); err != nil {
		os.Remove(temporary)
		return nil, fmt.Errorf("replace database: %w", err)
	}
	// Opening migrates an older backup forward to the current schema.
	restored, err := Open(target)
	if err != nil {
		return nil, fmt.Errorf("open restored database: %w", err)
	}
	defer restored.Close()
	version, err := restored.SchemaVersion()
	if err != nil {
		return nil, err
	}
	info.Path = target
	info.SchemaVersion = version
	return info, nil
}

func prepareBackupPath(path string) (string, error) {
	if filepath.Clean(path) == "." || path == "" {
		return "", fmt.Errorf("%w: a path is required", ErrInvalidBackupPath)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidBackupPath, err)
	}
	return absolute, nil
}

// isMissingTable recognizes a table a newer schema added, which makes an old
// backup incomplete rather than unreadable.
func isMissingTable(err error) bool {
	return err != nil && (errors.Is(err, sql.ErrNoRows) ||
		strings.Contains(err.Error(), "no such table"))
}
