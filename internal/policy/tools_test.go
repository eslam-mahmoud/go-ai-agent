package policy

import (
	"slices"
	"testing"
)

func TestToolRulesAreEmptyForAnEmptyPolicy(t *testing.T) {
	engine, err := New(Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if !engine.ToolRules().Empty() {
		t.Fatalf("rules = %#v, want empty", engine.ToolRules())
	}
}

// A denied path must be denied to every tool that can write it. A rule that
// blocked Write but permitted Edit would be a rule in name only.
func TestDeniedPathsCoverEveryWriteCapableTool(t *testing.T) {
	engine, err := New(Policy{
		Paths: PathRules{Deny: []string{".env"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rules := engine.ToolRules()
	for _, want := range []string{
		"Edit(.env)", "MultiEdit(.env)", "NotebookEdit(.env)", "Write(.env)",
	} {
		if !slices.Contains(rules.Deny, want) {
			t.Fatalf("deny rules %v are missing %q", rules.Deny, want)
		}
	}
}

func TestCommandRulesRenderAsBashPatterns(t *testing.T) {
	engine, err := New(Policy{
		Commands: CommandRules{
			Default: Ask,
			Allow:   []string{"go test ./..."},
			Deny:    []string{"git push --force"},
		},
		RequireApproval: []string{"gh pr merge"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rules := engine.ToolRules()
	if !slices.Contains(rules.Allow, "Bash(go test ./...)") {
		t.Fatalf("allow = %v", rules.Allow)
	}
	if !slices.Contains(rules.Deny, "Bash(git push --force)") {
		t.Fatalf("deny = %v", rules.Deny)
	}
	if !slices.Contains(rules.Ask, "Bash(gh pr merge)") {
		t.Fatalf("ask = %v", rules.Ask)
	}
}

// A policy written in the provider's own vocabulary must survive unchanged,
// so an operator can express something the neutral form cannot.
func TestAPatternThatAlreadyNamesAToolIsPassedThrough(t *testing.T) {
	engine, err := New(Policy{
		Commands: CommandRules{Deny: []string{"WebFetch(https://evil.example)"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(engine.ToolRules().Deny, "WebFetch(https://evil.example)") {
		t.Fatalf("deny = %v", engine.ToolRules().Deny)
	}
}

// Rendered rules are compared across deploys, so their order must not depend
// on map iteration or on the order the config happened to list them.
func TestRenderedRulesAreSortedAndDeduplicated(t *testing.T) {
	engine, err := New(Policy{
		Commands: CommandRules{
			Deny: []string{"rm -rf /", "git push --force", "rm -rf /"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	deny := engine.ToolRules().Deny
	want := []string{"Bash(git push --force)", "Bash(rm -rf /)"}
	if !slices.Equal(deny, want) {
		t.Fatalf("deny = %v, want %v", deny, want)
	}
}

func TestNilEngineRendersNothing(t *testing.T) {
	var engine *Engine
	if !engine.ToolRules().Empty() {
		t.Fatal("a nil engine must render no rules")
	}
}
