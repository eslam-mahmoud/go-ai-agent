package reposcan

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanReportsWhatAProjectContains(t *testing.T) {
	t.Parallel()
	workspace := buildFixtureTree(t)
	report, err := Scan(workspace)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	assertPaths(t, "documents", documentPaths(report.Documents), []string{
		"AGENTS.md", "CLAUDE.md", "README.md",
	})
	assertPaths(t, "architecture docs", documentPaths(report.ArchitectureDocs), []string{
		"docs/architecture/components.md", "docs/architecture/overview.md",
	})
	assertPaths(t, "decision records", documentPaths(report.DecisionRecords), []string{
		"docs/decisions/ADR-001-engine.md",
	})
	if !report.HasArchitecture || !report.HasMadarProject {
		t.Fatalf("flags: architecture=%v madar=%v",
			report.HasArchitecture, report.HasMadarProject)
	}

	assertPaths(t, "manifests", manifestPaths(report.Manifests), []string{
		"go.mod", "web/package.json",
	})
	assertPaths(t, "ecosystems", report.Ecosystems, []string{"go", "javascript"})
	assertPaths(t, "CI workflows", report.CIWorkflows, []string{
		".github/workflows/ci.yml", ".github/workflows/release.yaml",
	})

	// Languages are ordered by file count, most first.
	if len(report.Languages) < 2 ||
		report.Languages[0].Name != "Go" ||
		report.Languages[0].Files != 3 {
		t.Fatalf("languages = %#v", report.Languages)
	}
	if !strings.HasPrefix(report.ReadmeExcerpt, "# Fixture") {
		t.Fatalf("readme excerpt = %q", report.ReadmeExcerpt)
	}
	if report.TruncatedFileWalk {
		t.Fatal("small fixture reported a truncated walk")
	}
}

func TestScanIsDeterministic(t *testing.T) {
	t.Parallel()
	workspace := buildFixtureTree(t)
	first, err := Scan(workspace)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		next, err := Scan(workspace)
		if err != nil {
			t.Fatal(err)
		}
		firstJSON, _ := json.Marshal(first)
		nextJSON, _ := json.Marshal(next)
		if string(firstJSON) != string(nextJSON) {
			t.Fatalf("scan differed between runs:\n%s\n%s", firstJSON, nextJSON)
		}
	}
}

func TestScanSkipsDependencyAndVersionControlDirectories(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	writeFile(t, workspace, "README.md", "# Root")
	for _, excluded := range []string{
		".git/config",
		"node_modules/left-pad/package.json",
		"vendor/example.com/dep/go.mod",
		"target/debug/build.rs",
		"__pycache__/module.py",
	} {
		writeFile(t, workspace, excluded, "excluded")
	}
	report, err := Scan(workspace)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(report.Manifests) != 0 {
		t.Fatalf("manifests = %#v", report.Manifests)
	}
	if len(report.Languages) != 0 {
		t.Fatalf("languages = %#v", report.Languages)
	}
	if report.FilesVisited != 1 {
		t.Fatalf("visited %d files, want 1", report.FilesVisited)
	}
}

func TestScanNeverFollowsSymbolicLinks(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	writeFile(t, outside, "secret.go", "package secret")
	workspace := t.TempDir()
	writeFile(t, workspace, "README.md", "# Root")
	if err := os.Symlink(outside, filepath.Join(workspace, "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(
		filepath.Join(outside, "secret.go"),
		filepath.Join(workspace, "linked.go"),
	); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	report, err := Scan(workspace)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, language := range report.Languages {
		if language.Name == "Go" {
			t.Fatalf("scan followed a symlink out of the workspace: %#v", report.Languages)
		}
	}
	if report.FilesVisited != 1 {
		t.Fatalf("visited %d files, want 1", report.FilesVisited)
	}
}

func TestScanCapsTheWalkAndReportsTruncation(t *testing.T) {
	workspace := t.TempDir()
	for index := range MaxVisitedFiles + 10 {
		writeFile(t, workspace, fmt.Sprintf("src/file%05d.go", index), "package main")
	}
	report, err := Scan(workspace)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !report.TruncatedFileWalk {
		t.Fatal("oversized tree did not report truncation")
	}
	if report.FilesVisited != MaxVisitedFiles {
		t.Fatalf("visited %d files, want %d", report.FilesVisited, MaxVisitedFiles)
	}
}

func TestScanTruncatesTheReadmeExcerpt(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	writeFile(t, workspace, "README.md", "# Long\n\n"+strings.Repeat("détail ", 2000))
	report, err := Scan(workspace)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(report.ReadmeExcerpt) > MaxExcerptBytes+8 {
		t.Fatalf("excerpt is %d bytes", len(report.ReadmeExcerpt))
	}
	if !strings.HasSuffix(report.ReadmeExcerpt, "…") {
		t.Fatalf("excerpt was not marked as truncated: %q",
			report.ReadmeExcerpt[max(0, len(report.ReadmeExcerpt)-20):])
	}
	// Truncation must not split a multi-byte rune.
	if !json.Valid(mustMarshal(t, report.ReadmeExcerpt)) {
		t.Fatal("excerpt is not valid UTF-8")
	}
}

func TestScanHandlesEmptyAndMissingWorkspaces(t *testing.T) {
	t.Parallel()
	empty, err := Scan(t.TempDir())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if empty.FilesVisited != 0 ||
		len(empty.Documents) != 0 ||
		empty.HasArchitecture ||
		empty.ReadmeExcerpt != "" {
		t.Fatalf("empty report = %#v", empty)
	}
	// Empty slices, not nil, so the encoded snapshot is stable.
	encoded := string(mustMarshal(t, empty))
	for _, field := range []string{
		`"documents":[]`, `"manifests":[]`, `"languages":[]`, `"ci_workflows":[]`,
	} {
		if !strings.Contains(encoded, field) {
			t.Fatalf("encoded report missing %s: %s", field, encoded)
		}
	}

	for _, workspace := range []string{"", "   "} {
		if _, err := Scan(workspace); !errors.Is(err, ErrInvalidWorkspace) {
			t.Fatalf("Scan(%q) error = %v", workspace, err)
		}
	}
	if _, err := Scan(filepath.Join(t.TempDir(), "absent")); !errors.Is(
		err, ErrInvalidWorkspace,
	) {
		t.Fatalf("missing workspace error = %v", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	writeFile(t, filepath.Dir(file), "file", "content")
	if _, err := Scan(file); !errors.Is(err, ErrInvalidWorkspace) {
		t.Fatalf("file workspace error = %v", err)
	}
}

func buildFixtureTree(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	files := map[string]string{
		"README.md":                        "# Fixture\n\nA project for scanning.",
		"AGENTS.md":                        "# Agents",
		"CLAUDE.md":                        "# Claude",
		"go.mod":                           "module example.com/fixture",
		"web/package.json":                 `{"name":"fixture"}`,
		".github/workflows/ci.yml":         "name: CI",
		".github/workflows/release.yaml":   "name: Release",
		"docs/architecture/overview.md":    "# Overview",
		"docs/architecture/components.md":  "# Components",
		"docs/decisions/ADR-001-engine.md": "# ADR 1",
		".madar/project.yaml":              "version: 1",
		"main.go":                          "package main",
		"internal/app/app.go":              "package app",
		"internal/app/app_test.go":         "package app",
		"web/index.ts":                     "export {}",
		".git/config":                      "[core]",
	}
	for path, content := range files {
		writeFile(t, workspace, path, content)
	}
	return workspace
}

func writeFile(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func documentPaths(documents []Document) []string {
	paths := make([]string, 0, len(documents))
	for _, document := range documents {
		paths = append(paths, document.Path)
	}
	return paths
}

func manifestPaths(manifests []Manifest) []string {
	paths := make([]string, 0, len(manifests))
	for _, manifest := range manifests {
		paths = append(paths, manifest.Path)
	}
	return paths
}

func assertPaths(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("%s = %v, want %v", label, got, want)
		}
	}
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
