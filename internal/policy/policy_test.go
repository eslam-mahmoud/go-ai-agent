package policy

import (
	"errors"
	"testing"
)

func planPolicy() Policy {
	// The example policy from the plan.
	return Policy{
		Commands: CommandRules{
			Default: Ask,
			Allow: []string{
				"git status*", "git diff*", "go test*", "go vet*", "go fmt*",
			},
			Deny: []string{
				"rm -rf /*", "git push --force*",
				"terraform destroy*", "kubectl delete namespace*",
			},
		},
		Paths: PathRules{
			Writable: []string{"./**"},
			Deny:     []string{".env", "**/*.pem", "**/*.key", "**/secrets/**"},
		},
		RequireApproval: []string{
			"database-destructive-migration",
			"dependency-major-upgrade",
			"force-push",
			"production-deployment",
		},
	}
}

func TestEvaluateCommandFollowsThePlanPolicy(t *testing.T) {
	t.Parallel()
	engine := newTestEngine(t, planPolicy())
	tests := []struct {
		command string
		want    Decision
	}{
		{"git status --short", Allow},
		{"go test ./...", Allow},
		{"go vet ./...", Allow},
		{"rm -rf /", Deny},
		{"git push --force origin main", Deny},
		{"terraform destroy -auto-approve", Deny},
		{"kubectl delete namespace prod", Deny},
		// Nothing matched, so the default applies.
		{"curl https://example.com", Ask},
		{"", Deny},
	}
	for _, test := range tests {
		test := test
		t.Run(test.command, func(t *testing.T) {
			t.Parallel()
			result := engine.EvaluateCommand(test.command)
			if result.Decision != test.want {
				t.Fatalf("EvaluateCommand(%q) = %q (%s), want %q",
					test.command, result.Decision, result.Reason, test.want)
			}
			if result.Reason == "" {
				t.Fatal("decision has no reason")
			}
		})
	}
}

// A permissive pattern must never re-enable something explicitly forbidden.
func TestDenyAlwaysBeatsAllow(t *testing.T) {
	t.Parallel()
	engine := newTestEngine(t, Policy{
		Commands: CommandRules{
			Default: Ask,
			// Deliberately broad allow, listed first.
			Allow: []string{"*", "git push*"},
			Deny:  []string{"git push --force*"},
		},
	})
	result := engine.EvaluateCommand("git push --force origin main")
	if result.Decision != Deny {
		t.Fatalf("a broad allow overrode a deny: %#v", result)
	}
	if result.Rule != "git push --force*" {
		t.Fatalf("rule = %q", result.Rule)
	}
	// The allow still applies to what is not denied.
	if engine.EvaluateCommand("git push origin main").Decision != Allow {
		t.Fatal("deny leaked onto an allowed command")
	}
}

func TestEvaluateWriteHonoursWritableRootsAndSecrets(t *testing.T) {
	t.Parallel()
	engine := newTestEngine(t, planPolicy())
	tests := []struct {
		path string
		want Decision
	}{
		{"./internal/store/store.go", Allow},
		{"./README.md", Allow},
		// Secrets are denied even inside a writable root.
		{"./.env", Deny},
		{".env", Deny},
		{"./deploy/tls/server.pem", Deny},
		{"./config/private.key", Deny},
		{"./app/secrets/token.txt", Deny},
		// Outside every writable root.
		{"/etc/passwd", Deny},
	}
	for _, test := range tests {
		test := test
		t.Run(test.path, func(t *testing.T) {
			t.Parallel()
			result := engine.EvaluateWrite(test.path)
			if result.Decision != test.want {
				t.Fatalf("EvaluateWrite(%q) = %q (%s), want %q",
					test.path, result.Decision, result.Reason, test.want)
			}
		})
	}
}

// Traversal must not be able to escape a writable root.
func TestEvaluateWriteResolvesTraversal(t *testing.T) {
	t.Parallel()
	engine := newTestEngine(t, planPolicy())
	for _, target := range []string{
		"./internal/../../etc/passwd",
		"./../../../root/.ssh/id_rsa",
		"./a/b/../../../outside.txt",
	} {
		result := engine.EvaluateWrite(target)
		if result.Decision != Deny {
			t.Fatalf("traversal %q was not denied: %#v", target, result)
		}
	}
	// Redundant separators and segments still resolve inside the root.
	for _, target := range []string{
		"./internal//store/./store.go",
		"./internal/store/../store/store.go",
	} {
		if engine.EvaluateWrite(target).Decision != Allow {
			t.Fatalf("%q was not recognized as inside the root", target)
		}
	}
	// A secret reached through traversal is still a secret.
	if engine.EvaluateWrite("./internal/../.env").Decision != Deny {
		t.Fatal("traversal reached a denied path")
	}
	if engine.EvaluateWrite("   ").Decision != Deny {
		t.Fatal("an empty path was not denied")
	}
}

// A missing configuration must not be silently permissive.
func TestEmptyPolicyAsksRatherThanAllows(t *testing.T) {
	t.Parallel()
	engine := newTestEngine(t, Policy{})
	if result := engine.EvaluateCommand("rm -rf /"); result.Decision != Ask {
		t.Fatalf("empty policy command decision = %q", result.Decision)
	}
	if result := engine.EvaluateWrite("./anything"); result.Decision != Ask {
		t.Fatalf("empty policy write decision = %q", result.Decision)
	}
	if engine.RequiresApproval("force-push") {
		t.Fatal("empty policy required approval")
	}
}

func TestRequiresApprovalUsesTheConfiguredList(t *testing.T) {
	t.Parallel()
	engine := newTestEngine(t, planPolicy())
	for _, action := range []string{
		"database-destructive-migration", "force-push", " production-deployment ",
	} {
		if !engine.RequiresApproval(action) {
			t.Errorf("%q does not require approval", action)
		}
	}
	for _, action := range []string{"", "run-tests", "force push"} {
		if engine.RequiresApproval(action) {
			t.Errorf("%q unexpectedly requires approval", action)
		}
	}
	if len(engine.Approvals()) != 4 {
		t.Fatalf("approvals = %v", engine.Approvals())
	}
}

func TestNewRejectsUnusablePolicies(t *testing.T) {
	t.Parallel()
	if _, err := New(Policy{
		Commands: CommandRules{Default: "maybe"},
	}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("unknown default error = %v", err)
	}
	if _, err := New(Policy{
		Commands: CommandRules{Allow: []string{"  "}},
	}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("empty pattern error = %v", err)
	}
	if _, err := New(Policy{
		Paths: PathRules{Deny: []string{"[unclosed"}},
	}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("malformed pattern error = %v", err)
	}
	// An unset default becomes ask, not allow.
	engine, err := New(Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if engine.EvaluateCommand("anything").Decision != Ask {
		t.Fatal("an unset default did not become ask")
	}
}

func TestMatchPatternEdgeCases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		pattern string
		value   string
		want    bool
	}{
		{"git status*", "git status", true},
		{"git status*", "git statuses", true},
		{"git status*", "git stat", false},
		{"**/*.pem", "deploy/tls/server.pem", true},
		{"**/*.pem", "server.pem", true},
		{"**/*.pem", "server.pem.bak", false},
		{"**/secrets/**", "app/secrets/token.txt", true},
		{"**/secrets/**", "app/secret/token.txt", false},
		{"./**", "./internal/store.go", true},
		{"./**", "/etc/passwd", false},
		{"*", "anything at all", true},
		{"", "anything", false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.pattern+"|"+test.value, func(t *testing.T) {
			t.Parallel()
			if got := matchPattern(test.pattern, test.value); got != test.want {
				t.Fatalf("matchPattern(%q, %q) = %v, want %v",
					test.pattern, test.value, got, test.want)
			}
		})
	}
}

func newTestEngine(t *testing.T, policy Policy) *Engine {
	t.Helper()
	engine, err := New(policy)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}
