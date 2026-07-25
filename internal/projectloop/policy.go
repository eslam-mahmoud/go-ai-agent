package projectloop

import (
	"fmt"

	"github.com/eslam-mahmoud/go-ai-agent/internal/config"
	"github.com/eslam-mahmoud/go-ai-agent/internal/engine"
	"github.com/eslam-mahmoud/go-ai-agent/internal/policy"
)

// BuildToolRules turns the configured safety policy into the tool-permission
// rules handed to the provider. The daemon calls this once and gives the
// result to the engine adapter, so every run is constrained without each mode
// having to remember to ask.
//
// An absent policy block produces empty rules, which the adapter passes over
// entirely — upgrading without one changes nothing.
func BuildToolRules(cfg *config.Config) (engine.ToolRules, error) {
	if cfg == nil {
		return engine.ToolRules{}, nil
	}
	settings := cfg.Policy
	if settings.CommandDefault == "" &&
		len(settings.CommandAllow) == 0 &&
		len(settings.CommandDeny) == 0 &&
		len(settings.WritablePaths) == 0 &&
		len(settings.DeniedPaths) == 0 &&
		len(settings.RequireApproval) == 0 {
		return engine.ToolRules{}, nil
	}
	evaluator, err := policy.New(policy.Policy{
		Commands: policy.CommandRules{
			Default: policy.Decision(settings.CommandDefault),
			Allow:   settings.CommandAllow,
			Deny:    settings.CommandDeny,
		},
		Paths: policy.PathRules{
			Writable: settings.WritablePaths,
			Deny:     settings.DeniedPaths,
		},
		RequireApproval: settings.RequireApproval,
	})
	if err != nil {
		return engine.ToolRules{}, fmt.Errorf("load safety policy: %w", err)
	}
	rules := evaluator.ToolRules()
	return engine.ToolRules{
		Allow: rules.Allow,
		Ask:   rules.Ask,
		Deny:  rules.Deny,
	}, nil
}
