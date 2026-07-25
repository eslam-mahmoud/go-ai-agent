// Package policy enforces Madar's safety rules outside the model, as the plan
// requires. A model may propose anything; policy decides what is allowed.
package policy

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

var ErrInvalidPolicy = errors.New("invalid policy")

// Decision is the outcome of evaluating an action against policy.
type Decision string

const (
	// Allow permits the action without asking.
	Allow Decision = "allow"
	// Ask requires human approval before proceeding.
	Ask Decision = "ask"
	// Deny refuses the action outright.
	Deny Decision = "deny"
)

func (decision Decision) Valid() bool {
	switch decision {
	case Allow, Ask, Deny:
		return true
	default:
		return false
	}
}

// Result explains a decision, so a refusal can be reported with its cause.
type Result struct {
	Decision Decision
	// Rule is the pattern or setting that produced the decision.
	Rule string
	// Reason is a short human-readable explanation.
	Reason string
}

// CommandRules govern shell commands a mode may run.
type CommandRules struct {
	// Default applies when no pattern matches. An unset default asks.
	Default Decision
	Allow   []string
	Deny    []string
}

// PathRules govern where a mode may write.
type PathRules struct {
	// Writable lists the roots that may be written, as glob patterns.
	Writable []string
	// Deny lists patterns that may never be written, even inside a writable
	// root. Secrets live here.
	Deny []string
}

// Policy is the loaded safety policy.
type Policy struct {
	Commands        CommandRules
	Paths           PathRules
	RequireApproval []string
}

// Engine evaluates actions against a policy.
type Engine struct {
	policy   Policy
	approval map[string]struct{}
}

// New builds an engine. An empty policy denies nothing but asks for
// everything, so a missing configuration is never silently permissive.
func New(policy Policy) (*Engine, error) {
	if policy.Commands.Default == "" {
		policy.Commands.Default = Ask
	}
	if !policy.Commands.Default.Valid() {
		return nil, fmt.Errorf(
			"%w: unknown command default %q",
			ErrInvalidPolicy,
			policy.Commands.Default,
		)
	}
	for _, group := range [][]string{
		policy.Commands.Allow,
		policy.Commands.Deny,
		policy.Paths.Writable,
		policy.Paths.Deny,
	} {
		for _, pattern := range group {
			if err := validatePattern(pattern); err != nil {
				return nil, err
			}
		}
	}
	approval := make(map[string]struct{}, len(policy.RequireApproval))
	for _, action := range policy.RequireApproval {
		trimmed := strings.TrimSpace(action)
		if trimmed != "" {
			approval[trimmed] = struct{}{}
		}
	}
	return &Engine{policy: policy, approval: approval}, nil
}

// EvaluateCommand decides whether a command may run.
//
// Deny always wins: a permissive pattern must never be able to re-enable
// something the operator explicitly forbade, regardless of ordering or how
// specific the allow pattern is.
func (engine *Engine) EvaluateCommand(command string) Result {
	normalized := strings.TrimSpace(command)
	if normalized == "" {
		return Result{
			Decision: Deny,
			Reason:   "an empty command cannot be evaluated",
		}
	}
	if rule, matched := matchAny(engine.policy.Commands.Deny, normalized); matched {
		return Result{Decision: Deny, Rule: rule, Reason: "command matches a deny rule"}
	}
	if rule, matched := matchAny(engine.policy.Commands.Allow, normalized); matched {
		return Result{Decision: Allow, Rule: rule, Reason: "command matches an allow rule"}
	}
	return Result{
		Decision: engine.policy.Commands.Default,
		Rule:     "commands.default",
		Reason:   "no command rule matched",
	}
}

// EvaluateWrite decides whether a path may be written. The path is resolved
// first, so traversal and redundant separators cannot escape a writable root.
func (engine *Engine) EvaluateWrite(target string) Result {
	cleaned := normalizePath(target)
	if cleaned == "" {
		return Result{Decision: Deny, Reason: "an empty path cannot be evaluated"}
	}
	// Denied patterns beat writable roots: a secret inside a writable
	// directory is still a secret. Every equivalent spelling of the path is
	// checked, so "./.env" cannot slip past a rule written as ".env".
	for _, form := range pathForms(cleaned) {
		if rule, matched := matchAny(engine.policy.Paths.Deny, form); matched {
			return Result{Decision: Deny, Rule: rule, Reason: "path matches a deny rule"}
		}
	}
	if len(engine.policy.Paths.Writable) == 0 {
		return Result{
			Decision: Ask,
			Rule:     "paths.writable",
			Reason:   "no writable roots are configured",
		}
	}
	for _, form := range pathForms(cleaned) {
		if rule, matched := matchAny(engine.policy.Paths.Writable, form); matched {
			return Result{
				Decision: Allow,
				Rule:     rule,
				Reason:   "path is inside a writable root",
			}
		}
	}
	return Result{
		Decision: Deny,
		Rule:     "paths.writable",
		Reason:   "path is outside every writable root",
	}
}

// RequiresApproval reports whether an action needs human approval.
func (engine *Engine) RequiresApproval(action string) bool {
	_, required := engine.approval[strings.TrimSpace(action)]
	return required
}

// Approvals returns the configured approval-gated actions.
func (engine *Engine) Approvals() []string {
	actions := make([]string, 0, len(engine.approval))
	for action := range engine.approval {
		actions = append(actions, action)
	}
	return actions
}

// matchAny reports the first pattern that matches, and which one it was.
func matchAny(patterns []string, value string) (string, bool) {
	for _, pattern := range patterns {
		if matchPattern(pattern, value) {
			return pattern, true
		}
	}
	return "", false
}

// matchPattern implements the plan's glob shapes: a trailing * as a prefix
// match, ** spanning separators, and otherwise path-style globbing.
func matchPattern(pattern, value string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	if pattern == "**" || pattern == "*" {
		return true
	}
	if strings.Contains(pattern, "**") {
		return matchDoubleStar(pattern, value)
	}
	if matched, err := path.Match(pattern, value); err == nil && matched {
		return true
	}
	// "git status*" should match "git status --short".
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(value, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == value
}

// matchDoubleStar handles patterns like "**/*.pem" and "./**" by splitting on
// the wildcard and requiring each literal segment in order.
func matchDoubleStar(pattern, value string) bool {
	parts := strings.Split(pattern, "**")
	position := 0
	for index, part := range parts {
		if part == "" {
			continue
		}
		// A leading segment must anchor at the start.
		if index == 0 {
			if !strings.HasPrefix(value, part) {
				return false
			}
			position = len(part)
			continue
		}
		// A trailing segment may be a suffix or a glob over the remainder.
		if index == len(parts)-1 {
			remainder := value[position:]
			if strings.HasSuffix(remainder, strings.TrimPrefix(part, "/")) {
				return true
			}
			trimmed := strings.TrimPrefix(part, "/")
			for _, candidate := range suffixes(remainder) {
				if matched, err := path.Match(trimmed, candidate); err == nil && matched {
					return true
				}
			}
			return false
		}
		next := strings.Index(value[position:], part)
		if next < 0 {
			return false
		}
		position += next + len(part)
	}
	return true
}

// suffixes yields the path segments a trailing pattern may match against.
func suffixes(value string) []string {
	trimmed := strings.TrimPrefix(value, "/")
	segments := strings.Split(trimmed, "/")
	candidates := make([]string, 0, len(segments))
	for index := range segments {
		candidates = append(candidates, strings.Join(segments[index:], "/"))
	}
	return candidates
}

// normalizePath resolves a path so traversal cannot escape a writable root.
// Relative paths keep a leading "./" so patterns like "./**" still match.
func normalizePath(target string) string {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return ""
	}
	cleaned := filepath.ToSlash(path.Clean(filepath.ToSlash(trimmed)))
	if strings.HasPrefix(trimmed, "./") && !strings.HasPrefix(cleaned, "/") &&
		!strings.HasPrefix(cleaned, "../") {
		return "./" + strings.TrimPrefix(cleaned, "./")
	}
	return cleaned
}

func validatePattern(pattern string) error {
	if strings.TrimSpace(pattern) == "" {
		return fmt.Errorf("%w: a rule pattern cannot be empty", ErrInvalidPolicy)
	}
	probe := strings.ReplaceAll(pattern, "**", "*")
	if _, err := path.Match(probe, "probe"); err != nil {
		return fmt.Errorf("%w: pattern %q: %v", ErrInvalidPolicy, pattern, err)
	}
	return nil
}

// pathForms returns the equivalent spellings of a relative path. A deny rule
// must apply however the path was written, so matching considers both the
// "./"-prefixed and bare forms.
func pathForms(cleaned string) []string {
	if strings.HasPrefix(cleaned, "./") {
		return []string{cleaned, strings.TrimPrefix(cleaned, "./")}
	}
	if strings.HasPrefix(cleaned, "/") || strings.HasPrefix(cleaned, "../") {
		return []string{cleaned}
	}
	return []string{cleaned, "./" + cleaned}
}
