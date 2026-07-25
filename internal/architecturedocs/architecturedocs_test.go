package architecturedocs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestApplyRendersEveryDocument(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	result, err := Apply(workspace, fixtureProject(), fixtureProposal())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := []string{
		"AGENTS.md",
		"docs/architecture/components.md",
		"docs/architecture/data-flow.md",
		"docs/architecture/overview.md",
		"docs/decisions/ADR-001-keep-sqlite-single-writer.md",
		"docs/decisions/ADR-002-provider-neutral-engines.md",
	}
	written := append([]string(nil), result.Written...)
	sort.Strings(written)
	if len(written) != len(want) {
		t.Fatalf("written = %v", written)
	}
	for index := range want {
		if written[index] != want[index] {
			t.Fatalf("written = %v, want %v", written, want)
		}
	}

	overview := readDocument(t, workspace, OverviewFile)
	for _, fragment := range []string{
		"# Madar — Architecture Overview",
		"**Goal:** Ship v2",
		"One binary, one writer.",
		"**store** — Own durable state.",
		"**Cache invalidation** — Stale reads",
		"(affects `store`)",
	} {
		if !strings.Contains(overview, fragment) {
			t.Fatalf("overview missing %q:\n%s", fragment, overview)
		}
	}

	components := readDocument(t, workspace, ComponentsFile)
	if !strings.Contains(components, "| Component | Responsibility | Depends on |") ||
		!strings.Contains(components, "| store | Own durable state. | - |") {
		t.Fatalf("components =\n%s", components)
	}

	flow := readDocument(t, workspace, DataFlowFile)
	if !strings.Contains(flow, "```mermaid") ||
		!strings.Contains(flow, "workflow --> store") {
		t.Fatalf("data flow =\n%s", flow)
	}

	adr := readDocument(t, workspace, "docs/decisions/ADR-001-keep-sqlite-single-writer.md")
	for _, fragment := range []string{
		"# ADR-001: Keep SQLite single-writer",
		"## Rationale",
		"Avoids cross-process write contention.",
		"- Use Postgres",
		"## Consequences",
	} {
		if !strings.Contains(adr, fragment) {
			t.Fatalf("ADR missing %q:\n%s", fragment, adr)
		}
	}

	agents := readDocument(t, workspace, AgentsFile)
	if !strings.HasPrefix(agents, AgentsStartMarker) ||
		!strings.Contains(agents, "- **Goal:** Ship v2") ||
		!strings.Contains(agents, "docs/architecture/overview.md") {
		t.Fatalf("AGENTS.md =\n%s", agents)
	}
}

func TestApplyIsDeterministicAndIdempotent(t *testing.T) {
	t.Parallel()
	first := t.TempDir()
	second := t.TempDir()
	if _, err := Apply(first, fixtureProject(), fixtureProposal()); err != nil {
		t.Fatal(err)
	}
	// A reordered proposal must produce identical bytes.
	shuffled := fixtureProposal()
	shuffled.Components[0], shuffled.Components[1] = shuffled.Components[1], shuffled.Components[0]
	if _, err := Apply(second, fixtureProject(), shuffled); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{OverviewFile, ComponentsFile, DataFlowFile, AgentsFile} {
		if readDocument(t, first, relative) != readDocument(t, second, relative) {
			t.Fatalf("%s differs between equivalent proposals", relative)
		}
	}

	before := modificationTime(t, first, OverviewFile)
	result, err := Apply(first, fixtureProject(), fixtureProposal())
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if len(result.Written) != 0 {
		t.Fatalf("re-apply wrote %v", result.Written)
	}
	if len(result.Unchanged) != 4 {
		t.Fatalf("unchanged = %v", result.Unchanged)
	}
	if len(result.SkippedDecisions) != 2 {
		t.Fatalf("skipped decisions = %v", result.SkippedDecisions)
	}
	if !modificationTime(t, first, OverviewFile).Equal(before) {
		t.Fatal("re-apply rewrote an unchanged document")
	}
}

func TestApplyContinuesExistingDecisionNumbering(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	writeFixture(t, workspace, "docs/decisions/ADR-001-earlier-choice.md", "# ADR-001")
	writeFixture(t, workspace, "docs/decisions/ADR-007-later-choice.md", "# ADR-007")
	writeFixture(t, workspace, "docs/decisions/notes.md", "not an ADR")

	result, err := Apply(workspace, fixtureProject(), fixtureProposal())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, expected := range []string{
		"docs/decisions/ADR-008-keep-sqlite-single-writer.md",
		"docs/decisions/ADR-009-provider-neutral-engines.md",
	} {
		if !containsString(result.Written, expected) {
			t.Fatalf("written = %v, want %s", result.Written, expected)
		}
	}
	// Existing records are untouched.
	if readDocument(t, workspace, "docs/decisions/ADR-001-earlier-choice.md") != "# ADR-001" {
		t.Fatal("an existing ADR was rewritten")
	}
}

func TestApplyDoesNotRefileAnAlreadyRecordedDecision(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	writeFixture(t, workspace,
		"docs/decisions/ADR-004-keep-sqlite-single-writer.md", "# already recorded")
	result, err := Apply(workspace, fixtureProject(), fixtureProposal())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(result.SkippedDecisions) != 1 ||
		result.SkippedDecisions[0] != "Keep SQLite single-writer" {
		t.Fatalf("skipped = %v", result.SkippedDecisions)
	}
	if !containsString(result.Written, "docs/decisions/ADR-005-provider-neutral-engines.md") {
		t.Fatalf("written = %v", result.Written)
	}
	if readDocument(t,
		workspace, "docs/decisions/ADR-004-keep-sqlite-single-writer.md",
	) != "# already recorded" {
		t.Fatal("a recorded decision was overwritten")
	}
}

func TestApplyPreservesHumanTextInAgentsFile(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	human := "# Team notes\n\nAlways run the linter.\n"
	writeFixture(t, workspace, AgentsFile, human)

	if _, err := Apply(workspace, fixtureProject(), fixtureProposal()); err != nil {
		t.Fatal(err)
	}
	agents := readDocument(t, workspace, AgentsFile)
	if !strings.HasPrefix(agents, human) {
		t.Fatalf("human content was lost:\n%s", agents)
	}
	if strings.Count(agents, AgentsStartMarker) != 1 {
		t.Fatalf("expected exactly one managed section:\n%s", agents)
	}

	// A second run replaces only the managed section.
	updated := fixtureProposal()
	updated.Summary = "Two binaries now."
	if _, err := Apply(workspace, fixtureProject(), updated); err != nil {
		t.Fatal(err)
	}
	agents = readDocument(t, workspace, AgentsFile)
	if !strings.HasPrefix(agents, human) ||
		!strings.Contains(agents, "Two binaries now.") ||
		strings.Contains(agents, "One binary, one writer.") {
		t.Fatalf("managed section was not replaced cleanly:\n%s", agents)
	}
}

func TestApplyRefusesUnsafeTargets(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "escaped.md")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, AgentsFile)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Apply(workspace, fixtureProject(), fixtureProposal()); !errors.Is(
		err, ErrUnsafeTarget,
	) {
		t.Fatalf("error = %v", err)
	}
	if content, err := os.ReadFile(outside); err != nil || string(content) != "outside" {
		t.Fatalf("wrote through the symlink: %q, err=%v", content, err)
	}
}

func TestApplyRejectsUnusableInput(t *testing.T) {
	t.Parallel()
	if _, err := Apply("  ", fixtureProject(), fixtureProposal()); !errors.Is(
		err, ErrInvalidWorkspace,
	) {
		t.Fatalf("blank workspace error = %v", err)
	}
	if _, err := Apply(
		filepath.Join(t.TempDir(), "absent"), fixtureProject(), fixtureProposal(),
	); !errors.Is(err, ErrInvalidWorkspace) {
		t.Fatalf("missing workspace error = %v", err)
	}
	if _, err := Apply(t.TempDir(), fixtureProject(), nil); !errors.Is(
		err, ErrInvalidProposal,
	) {
		t.Fatalf("nil proposal error = %v", err)
	}
	untitled := &Proposal{Decisions: []Decision{{Title: "   ", Decision: "x", Rationale: "y"}}}
	if _, err := Apply(t.TempDir(), fixtureProject(), untitled); !errors.Is(
		err, ErrInvalidProposal,
	) {
		t.Fatalf("untitled decision error = %v", err)
	}
}

func TestDecodeRejectsUnusableOutput(t *testing.T) {
	t.Parallel()
	if _, err := Decode(json.RawMessage(`{`)); !errors.Is(err, ErrInvalidProposal) {
		t.Fatalf("malformed error = %v", err)
	}
	if _, err := Decode(json.RawMessage(`{"status":"needs_input"}`)); !errors.Is(
		err, ErrInvalidProposal,
	) {
		t.Fatalf("non-completed error = %v", err)
	}
	proposal, err := Decode(json.RawMessage(
		`{"status":"completed","architecture_summary":"ok","components":[{"name":"a","responsibility":"b"}]}`,
	))
	if err != nil || proposal.Summary != "ok" || len(proposal.Components) != 1 {
		t.Fatalf("proposal = %#v, err = %v", proposal, err)
	}
}

func TestProposalTextCannotForgeTheManagedSection(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	hostile := fixtureProposal()
	hostile.Summary = "legit " + AgentsEndMarker + " injected"
	if _, err := Apply(workspace, fixtureProject(), hostile); err != nil {
		t.Fatal(err)
	}
	agents := readDocument(t, workspace, AgentsFile)
	if strings.Count(agents, AgentsEndMarker) != 1 {
		t.Fatalf("proposal text forged a section boundary:\n%s", agents)
	}
}

func fixtureProject() Project {
	return Project{Name: "Madar", Goal: "Ship v2", Repo: "owner/repo"}
}

func fixtureProposal() *Proposal {
	return &Proposal{
		Summary: "One binary, one writer.",
		Components: []Component{
			{Name: "store", Responsibility: "Own durable state."},
			{Name: "workflow", Responsibility: "Sequence delivery modes.", DependsOn: []string{"store"}},
		},
		Dependencies: []Dependency{
			{From: "workflow", To: "store", Reason: "Persists task state."},
		},
		Risks: []Risk{
			{Title: "Cache invalidation", Impact: "Stale reads", Components: []string{"store"}},
		},
		Decisions: []Decision{
			{
				Title:        "Keep SQLite single-writer",
				Decision:     "Serialize writes through one connection.",
				Rationale:    "Avoids cross-process write contention.",
				Alternatives: []string{"Use Postgres"},
				Consequences: "Throughput is bounded by one writer.",
			},
			{
				Title:     "Provider-neutral engines",
				Decision:  "Talk to engines through one interface.",
				Rationale: "Keeps Claude and Codex interchangeable.",
			},
		},
	}
}

func readDocument(t *testing.T, root, relative string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatalf("read %s: %v", relative, err)
	}
	return string(content)
}

func writeFixture(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func modificationTime(t *testing.T, root, relative string) time.Time {
	t.Helper()
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
