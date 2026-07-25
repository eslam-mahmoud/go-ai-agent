package policy

import (
	"fmt"
	"sort"
	"strings"
)

// ToolRules are provider-neutral tool-permission patterns. They exist so the
// rules can be handed to the process that actually makes the tool calls: a
// check we run ourselves happens after the write, which is too late.
type ToolRules struct {
	Allow []string
	Ask   []string
	Deny  []string
}

// Empty reports that these rules constrain nothing, so a caller can leave the
// provider's own defaults alone rather than passing an empty ruleset.
func (rules ToolRules) Empty() bool {
	return len(rules.Allow) == 0 && len(rules.Ask) == 0 && len(rules.Deny) == 0
}

// ToolRules renders the policy as tool patterns.
//
// Deny patterns are emitted for both Bash and the file-writing tools, because
// a rule that stops `rm -rf /etc` at the shell but permits Write("/etc/passwd")
// is not a rule. The same reasoning drives the path rules: a denied path is
// denied to every tool that can write, not just to the one we thought of.
func (engine *Engine) ToolRules() ToolRules {
	if engine == nil {
		return ToolRules{}
	}
	rules := ToolRules{}
	for _, pattern := range engine.policy.Commands.Deny {
		rules.Deny = append(rules.Deny, bashPattern(pattern))
	}
	for _, pattern := range engine.policy.Commands.Allow {
		rules.Allow = append(rules.Allow, bashPattern(pattern))
	}
	// A denied path outranks a writable root, matching Evaluate's
	// deny-precedence, so it is emitted for every write-capable tool.
	for _, pattern := range engine.policy.Paths.Deny {
		for _, tool := range writeCapableTools {
			rules.Deny = append(rules.Deny, fmt.Sprintf("%s(%s)", tool, pattern))
		}
	}
	for _, pattern := range engine.policy.Paths.Writable {
		for _, tool := range writeCapableTools {
			rules.Allow = append(rules.Allow, fmt.Sprintf("%s(%s)", tool, pattern))
		}
	}
	// Commands that require approval cannot be answered by a headless daemon,
	// so they are surfaced as ask rules rather than quietly allowed.
	for _, action := range engine.policy.RequireApproval {
		rules.Ask = append(rules.Ask, bashPattern(action))
	}
	rules.Allow = normalizePatterns(rules.Allow)
	rules.Ask = normalizePatterns(rules.Ask)
	rules.Deny = normalizePatterns(rules.Deny)
	return rules
}

// writeCapableTools are the tools that can create or modify a file.
var writeCapableTools = []string{"Edit", "MultiEdit", "NotebookEdit", "Write"}

// bashPattern wraps a command glob in the tool-pattern form. A pattern that
// already names a tool is passed through, so a policy can be written in the
// provider's own vocabulary when it needs to be.
func bashPattern(pattern string) string {
	trimmed := strings.TrimSpace(pattern)
	if strings.Contains(trimmed, "(") && strings.HasSuffix(trimmed, ")") {
		return trimmed
	}
	return fmt.Sprintf("Bash(%s)", trimmed)
}

// normalizePatterns sorts and deduplicates, so the rendered rules are stable
// across runs and a diff of them means something.
func normalizePatterns(patterns []string) []string {
	if len(patterns) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(patterns))
	unique := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		trimmed := strings.TrimSpace(pattern)
		if trimmed == "" {
			continue
		}
		if _, duplicate := seen[trimmed]; duplicate {
			continue
		}
		seen[trimmed] = struct{}{}
		unique = append(unique, trimmed)
	}
	sort.Strings(unique)
	return unique
}
