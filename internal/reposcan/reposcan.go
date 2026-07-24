// Package reposcan inspects a cloned workspace and reports what a project
// already contains. It is read-only, bounded, and deterministic: the same tree
// always produces the same report.
package reposcan

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

var ErrInvalidWorkspace = errors.New("invalid scan workspace")

const (
	// MaxVisitedFiles bounds the walk so a huge or hostile repository cannot
	// stall initialization.
	MaxVisitedFiles = 20000
	// MaxExcerptBytes bounds how much of the README travels with the report.
	MaxExcerptBytes = 2000
	// maxDocumentBytes bounds any single file read.
	maxDocumentBytes = 64 * 1024
)

// Report is the deterministic result of one scan. Every slice is sorted.
type Report struct {
	Documents         []Document `json:"documents"`
	ArchitectureDocs  []Document `json:"architecture_docs"`
	DecisionRecords   []Document `json:"decision_records"`
	Manifests         []Manifest `json:"manifests"`
	Ecosystems        []string   `json:"ecosystems"`
	CIWorkflows       []string   `json:"ci_workflows"`
	Languages         []Language `json:"languages"`
	HasMadarProject   bool       `json:"has_madar_project"`
	HasArchitecture   bool       `json:"has_architecture"`
	ReadmeExcerpt     string     `json:"readme_excerpt"`
	FilesVisited      int        `json:"files_visited"`
	TruncatedFileWalk bool       `json:"truncated_file_walk"`
}

type Document struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

type Manifest struct {
	Path      string `json:"path"`
	Ecosystem string `json:"ecosystem"`
}

type Language struct {
	Name  string `json:"name"`
	Files int    `json:"files"`
}

// skippedDirectories are never descended into: they hold version control,
// dependencies, or build output, none of which describe the project.
var skippedDirectories = map[string]struct{}{
	".git":         {},
	".hg":          {},
	".svn":         {},
	"node_modules": {},
	"vendor":       {},
	"target":       {},
	"dist":         {},
	"build":        {},
	".venv":        {},
	"venv":         {},
	"__pycache__":  {},
	".idea":        {},
	".gradle":      {},
	".terraform":   {},
}

var manifestEcosystems = map[string]string{
	"go.mod":           "go",
	"package.json":     "javascript",
	"pnpm-lock.yaml":   "javascript",
	"yarn.lock":        "javascript",
	"pyproject.toml":   "python",
	"requirements.txt": "python",
	"setup.py":         "python",
	"Cargo.toml":       "rust",
	"pom.xml":          "java",
	"build.gradle":     "java",
	"Gemfile":          "ruby",
	"composer.json":    "php",
	"Package.swift":    "swift",
	"pubspec.yaml":     "dart",
	"mix.exs":          "elixir",
}

var languageExtensions = map[string]string{
	".go":    "Go",
	".ts":    "TypeScript",
	".tsx":   "TypeScript",
	".js":    "JavaScript",
	".jsx":   "JavaScript",
	".py":    "Python",
	".rs":    "Rust",
	".java":  "Java",
	".kt":    "Kotlin",
	".rb":    "Ruby",
	".php":   "PHP",
	".swift": "Swift",
	".c":     "C",
	".h":     "C",
	".cc":    "C++",
	".cpp":   "C++",
	".cs":    "C#",
	".sh":    "Shell",
	".sql":   "SQL",
	".ex":    "Elixir",
	".dart":  "Dart",
}

var documentNames = map[string]struct{}{
	"readme.md":          {},
	"readme":             {},
	"readme.rst":         {},
	"readme.txt":         {},
	"agents.md":          {},
	"claude.md":          {},
	"contributing.md":    {},
	"architecture.md":    {},
	"code_of_conduct.md": {},
}

// Scan walks the workspace and reports what it contains. A missing or
// unreadable entry is skipped rather than failing the scan.
func Scan(workspace string) (*Report, error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, fmt.Errorf("%w: workspace path is required", ErrInvalidWorkspace)
	}
	root, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidWorkspace, err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidWorkspace, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrInvalidWorkspace, root)
	}

	report := &Report{
		Documents:        []Document{},
		ArchitectureDocs: []Document{},
		DecisionRecords:  []Document{},
		Manifests:        []Manifest{},
		Ecosystems:       []string{},
		CIWorkflows:      []string{},
		Languages:        []Language{},
	}
	languageCounts := map[string]int{}
	ecosystems := map[string]struct{}{}
	var readmePath string

	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable entry is reported by omission, not by failure.
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if path == root {
			return nil
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if _, skip := skippedDirectories[entry.Name()]; skip {
				return fs.SkipDir
			}
			return nil
		}
		// Symbolic links are never followed: a link could point anywhere.
		if entry.Type()&fs.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return nil
		}
		if report.FilesVisited >= MaxVisitedFiles {
			report.TruncatedFileWalk = true
			return filepath.SkipAll
		}
		report.FilesVisited++

		name := strings.ToLower(entry.Name())
		size := fileSize(entry)
		switch {
		case isArchitectureDecision(relative):
			report.DecisionRecords = append(report.DecisionRecords, Document{relative, size})
		case isArchitectureDoc(relative):
			report.ArchitectureDocs = append(report.ArchitectureDocs, Document{relative, size})
			report.HasArchitecture = true
		}
		if _, known := documentNames[name]; known {
			report.Documents = append(report.Documents, Document{relative, size})
			if readmePath == "" && strings.HasPrefix(name, "readme") &&
				!strings.Contains(relative, "/") {
				readmePath = path
			}
		}
		if ecosystem, known := manifestEcosystems[entry.Name()]; known {
			report.Manifests = append(report.Manifests, Manifest{relative, ecosystem})
			ecosystems[ecosystem] = struct{}{}
		}
		if isCIWorkflow(relative) {
			report.CIWorkflows = append(report.CIWorkflows, relative)
		}
		if strings.HasPrefix(relative, ".madar/") {
			report.HasMadarProject = true
		}
		if language, known := languageExtensions[strings.ToLower(filepath.Ext(entry.Name()))]; known {
			languageCounts[language]++
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, filepath.SkipAll) {
		return nil, fmt.Errorf("scan workspace: %w", walkErr)
	}

	for ecosystem := range ecosystems {
		report.Ecosystems = append(report.Ecosystems, ecosystem)
	}
	for language, count := range languageCounts {
		report.Languages = append(report.Languages, Language{Name: language, Files: count})
	}
	if readmePath != "" {
		report.ReadmeExcerpt = readExcerpt(readmePath)
	}
	sortReport(report)
	return report, nil
}

// sortReport gives every list a stable order, so two scans of the same tree
// produce byte-identical reports regardless of filesystem iteration order.
func sortReport(report *Report) {
	sortDocuments := func(documents []Document) {
		sort.Slice(documents, func(i, j int) bool {
			return documents[i].Path < documents[j].Path
		})
	}
	sortDocuments(report.Documents)
	sortDocuments(report.ArchitectureDocs)
	sortDocuments(report.DecisionRecords)
	sort.Slice(report.Manifests, func(i, j int) bool {
		return report.Manifests[i].Path < report.Manifests[j].Path
	})
	sort.Strings(report.Ecosystems)
	sort.Strings(report.CIWorkflows)
	// Most files first, then by name so ties are stable.
	sort.Slice(report.Languages, func(i, j int) bool {
		if report.Languages[i].Files != report.Languages[j].Files {
			return report.Languages[i].Files > report.Languages[j].Files
		}
		return report.Languages[i].Name < report.Languages[j].Name
	})
}

func isArchitectureDoc(relative string) bool {
	lower := strings.ToLower(relative)
	return strings.HasPrefix(lower, "docs/architecture/") ||
		strings.HasPrefix(lower, "architecture/") ||
		lower == "architecture.md"
}

func isArchitectureDecision(relative string) bool {
	lower := strings.ToLower(relative)
	if strings.HasPrefix(lower, "docs/decisions/") || strings.HasPrefix(lower, "adr/") {
		return true
	}
	return strings.HasPrefix(strings.ToLower(filepath.Base(relative)), "adr-")
}

func isCIWorkflow(relative string) bool {
	lower := strings.ToLower(relative)
	if strings.HasPrefix(lower, ".github/workflows/") &&
		(strings.HasSuffix(lower, ".yml") || strings.HasSuffix(lower, ".yaml")) {
		return true
	}
	switch lower {
	case ".gitlab-ci.yml", ".circleci/config.yml", "azure-pipelines.yml", "jenkinsfile":
		return true
	default:
		return false
	}
}

func fileSize(entry fs.DirEntry) int64 {
	info, err := entry.Info()
	if err != nil {
		return 0
	}
	return info.Size()
}

// readExcerpt returns the opening of a document, truncated on a rune boundary
// so the excerpt is always valid UTF-8.
func readExcerpt(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	buffer := make([]byte, maxDocumentBytes)
	read, err := file.Read(buffer)
	if err != nil && read == 0 {
		return ""
	}
	content := strings.TrimSpace(string(buffer[:read]))
	if len(content) <= MaxExcerptBytes {
		return content
	}
	truncated := content[:MaxExcerptBytes]
	for !utf8.ValidString(truncated) && len(truncated) > 0 {
		truncated = truncated[:len(truncated)-1]
	}
	return strings.TrimSpace(truncated) + "\n…"
}
