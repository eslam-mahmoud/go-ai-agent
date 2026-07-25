// Package architecturedocs turns a validated Architect proposal into the
// repository documents the project plan calls for. Rendering is deterministic
// and writing is atomic, so re-applying a proposal changes nothing.
package architecturedocs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	AgentsFile        = "AGENTS.md"
	ArchitectureDir   = "docs/architecture"
	DecisionsDir      = "docs/decisions"
	OverviewFile      = "docs/architecture/overview.md"
	ComponentsFile    = "docs/architecture/components.md"
	DataFlowFile      = "docs/architecture/data-flow.md"
	AgentsStartMarker = "<!-- madar:agents:start -->"
	AgentsEndMarker   = "<!-- madar:agents:end -->"
)

var (
	ErrInvalidProposal  = errors.New("invalid architecture proposal")
	ErrUnsafeTarget     = errors.New("unsafe architecture document target")
	ErrInvalidWorkspace = errors.New("invalid architecture workspace")
)

// Proposal is the applicable part of one Architect run.
type Proposal struct {
	Components   []Component  `json:"components"`
	Decisions    []Decision   `json:"decisions"`
	Dependencies []Dependency `json:"dependencies"`
	Risks        []Risk       `json:"risks"`
	Summary      string       `json:"architecture_summary"`
	Status       string       `json:"status"`
}

type Component struct {
	Name           string   `json:"name"`
	Responsibility string   `json:"responsibility"`
	DependsOn      []string `json:"depends_on"`
}

type Decision struct {
	Title        string   `json:"title"`
	Decision     string   `json:"decision"`
	Rationale    string   `json:"rationale"`
	Alternatives []string `json:"alternatives"`
	Consequences string   `json:"consequences"`
}

type Dependency struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

type Risk struct {
	Title      string   `json:"title"`
	Impact     string   `json:"impact"`
	Components []string `json:"components"`
}

// Project is the identity rendered into the documents.
type Project struct {
	Name string
	Goal string
	Repo string
}

type Result struct {
	Written   []string
	Unchanged []string
	// SkippedDecisions are decisions that already have an ADR on disk.
	SkippedDecisions []string
}

// Decode reads a validated Architect output into an applicable proposal.
func Decode(raw json.RawMessage) (*Proposal, error) {
	var proposal Proposal
	if err := json.Unmarshal(raw, &proposal); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidProposal, err)
	}
	if strings.TrimSpace(proposal.Status) != "" && proposal.Status != "completed" {
		return nil, fmt.Errorf(
			"%w: status %q is not applicable",
			ErrInvalidProposal,
			proposal.Status,
		)
	}
	return &proposal, nil
}

// Apply renders and writes every document the proposal implies. Files whose
// content already matches are left untouched.
func Apply(workspace string, project Project, proposal *Proposal) (*Result, error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, fmt.Errorf("%w: workspace is required", ErrInvalidWorkspace)
	}
	root, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidWorkspace, err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrInvalidWorkspace, root)
	}
	if proposal == nil {
		return nil, fmt.Errorf("%w: proposal is nil", ErrInvalidProposal)
	}

	result := &Result{}
	documents := []struct {
		path    string
		content []byte
	}{
		{OverviewFile, renderOverview(project, proposal)},
		{ComponentsFile, renderComponents(proposal)},
		{DataFlowFile, renderDataFlow(proposal)},
	}
	for _, document := range documents {
		if err := applyDocument(root, document.path, document.content, result); err != nil {
			return nil, err
		}
	}

	agents, err := mergeAgentsFile(root, renderAgentsSection(project, proposal))
	if err != nil {
		return nil, err
	}
	if err := applyDocument(root, AgentsFile, agents, result); err != nil {
		return nil, err
	}
	if err := applyDecisions(root, proposal, result); err != nil {
		return nil, err
	}
	return result, nil
}

// applyDecisions files one ADR per new decision, continuing the existing
// numbering rather than renumbering what is already recorded.
func applyDecisions(root string, proposal *Proposal, result *Result) error {
	if len(proposal.Decisions) == 0 {
		return nil
	}
	existing, nextNumber, err := readExistingDecisions(root)
	if err != nil {
		return err
	}
	for _, decision := range proposal.Decisions {
		slug := slugify(decision.Title)
		if slug == "" {
			return fmt.Errorf("%w: a decision has no usable title", ErrInvalidProposal)
		}
		if _, recorded := existing[slug]; recorded {
			result.SkippedDecisions = append(result.SkippedDecisions, decision.Title)
			continue
		}
		path := fmt.Sprintf("%s/ADR-%03d-%s.md", DecisionsDir, nextNumber, slug)
		if err := applyDocument(
			root, path, renderDecision(nextNumber, decision), result,
		); err != nil {
			return err
		}
		existing[slug] = struct{}{}
		nextNumber++
	}
	return nil
}

var decisionFilePattern = regexp.MustCompile(`^ADR-(\d+)-(.+)\.md$`)

// readExistingDecisions returns the decision slugs already on disk and the
// next free number, so numbering is stable across runs.
func readExistingDecisions(root string) (map[string]struct{}, int, error) {
	recorded := make(map[string]struct{})
	next := 1
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(DecisionsDir)))
	if errors.Is(err, os.ErrNotExist) {
		return recorded, next, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("read decision records: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		matches := decisionFilePattern.FindStringSubmatch(entry.Name())
		if matches == nil {
			continue
		}
		recorded[matches[2]] = struct{}{}
		if number, err := strconv.Atoi(matches[1]); err == nil && number >= next {
			next = number + 1
		}
	}
	return recorded, next, nil
}

// applyDocument writes one document unless its bytes already match, recording
// which happened.
func applyDocument(root, relative string, content []byte, result *Result) error {
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := requireSafeTarget(path); err != nil {
		return err
	}
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, content) {
		result.Unchanged = append(result.Unchanged, relative)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create architecture document directory: %w", err)
	}
	if err := atomicWriteFile(path, content); err != nil {
		return err
	}
	result.Written = append(result.Written, relative)
	return nil
}

// requireSafeTarget refuses to write through a symlink or over anything that
// is not a regular file, so generation cannot escape the workspace.
func requireSafeTarget(path string) error {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return requireSafeParents(path)
	case err != nil:
		return fmt.Errorf("inspect architecture document: %w", err)
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%w: %s is a symbolic link", ErrUnsafeTarget, path)
	case !info.Mode().IsRegular():
		return fmt.Errorf("%w: %s is not a regular file", ErrUnsafeTarget, path)
	default:
		return requireSafeParents(path)
	}
}

func requireSafeParents(path string) error {
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) {
			if filepath.Dir(parent) == parent {
				return nil
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect architecture document directory: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s is a symbolic link", ErrUnsafeTarget, parent)
		}
		return nil
	}
}

func atomicWriteFile(path string, content []byte) (err error) {
	file, createErr := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if createErr != nil {
		return fmt.Errorf("create temporary architecture document: %w", createErr)
	}
	temporary := file.Name()
	defer func() {
		if err != nil {
			os.Remove(temporary)
		}
	}()
	if _, err = file.Write(content); err != nil {
		file.Close()
		return fmt.Errorf("write architecture document: %w", err)
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync architecture document: %w", err)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("close architecture document: %w", err)
	}
	if err = os.Chmod(temporary, 0o644); err != nil {
		return fmt.Errorf("set architecture document mode: %w", err)
	}
	if err = os.Rename(temporary, path); err != nil {
		return fmt.Errorf("replace architecture document: %w", err)
	}
	return nil
}

// mergeAgentsFile replaces only Madar's marked section, so human-authored
// guidance in AGENTS.md survives regeneration.
func mergeAgentsFile(root, section string) ([]byte, error) {
	path := filepath.Join(root, AgentsFile)
	existing, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []byte(section + "\n"), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read AGENTS.md: %w", err)
	}
	body := string(existing)
	start := strings.Index(body, AgentsStartMarker)
	end := strings.Index(body, AgentsEndMarker)
	if start < 0 || end < 0 || end < start {
		separator := "\n\n"
		if strings.HasSuffix(body, "\n\n") {
			separator = ""
		} else if strings.HasSuffix(body, "\n") {
			separator = "\n"
		}
		return []byte(body + separator + section + "\n"), nil
	}
	end += len(AgentsEndMarker)
	return []byte(body[:start] + section + body[end:]), nil
}
