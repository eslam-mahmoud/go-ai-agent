package projectloop

import (
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/config"
)

// Upgrading without a policy block must change nothing, so an absent policy
// has to render as no rules rather than as an empty-but-present ruleset.
func TestNoPolicyBlockProducesNoRules(t *testing.T) {
	rules, err := BuildToolRules(&config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !rules.Empty() {
		t.Fatalf("rules = %#v, want empty", rules)
	}
}

func TestNilConfigProducesNoRules(t *testing.T) {
	rules, err := BuildToolRules(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rules.Empty() {
		t.Fatalf("rules = %#v, want empty", rules)
	}
}

func TestConfiguredPolicyBecomesToolRules(t *testing.T) {
	cfg := &config.Config{}
	cfg.Policy = config.PolicyConfig{
		CommandDeny: []string{"git push --force"},
		DeniedPaths: []string{".env"},
	}
	rules, err := BuildToolRules(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if rules.Empty() {
		t.Fatal("a configured policy must produce rules")
	}
	found := false
	for _, rule := range rules.Deny {
		if rule == "Bash(git push --force)" {
			found = true
		}
	}
	if !found {
		t.Fatalf("deny = %v", rules.Deny)
	}
}

// A malformed policy must stop the daemon rather than start it unprotected.
func TestAnInvalidPolicyIsReported(t *testing.T) {
	cfg := &config.Config{}
	cfg.Policy = config.PolicyConfig{CommandDefault: "maybe"}
	if _, err := BuildToolRules(cfg); err == nil {
		t.Fatal("expected an unknown command default to be refused")
	}
}
