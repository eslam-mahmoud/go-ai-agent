package domain

import (
	"errors"
	"testing"
)

func TestDiscoveryVocabularies(t *testing.T) {
	categories := []DiscoveryCategory{
		DiscoveryBug,
		DiscoveryMissingRequirement,
		DiscoveryTechnicalDebt,
		DiscoverySecurity,
		DiscoveryArchitecture,
		DiscoveryTesting,
		DiscoveryDocumentation,
		DiscoveryObservability,
		DiscoveryPerformance,
		DiscoveryDependency,
		DiscoveryScopeChange,
	}
	for _, category := range categories {
		if !category.Valid() {
			t.Errorf("category %q is invalid", category)
		}
	}
	if DiscoveryCategory("unknown").Valid() {
		t.Error("unknown category accepted")
	}
	for _, severity := range []DiscoverySeverity{
		SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical,
	} {
		if !severity.Valid() {
			t.Errorf("severity %q is invalid", severity)
		}
	}
	if DiscoverySeverity("catastrophic").Valid() {
		t.Error("unknown severity accepted")
	}
	for _, status := range []DiscoveryStatus{
		DiscoveryUnevaluated,
		DiscoveryAccepted,
		DiscoveryRejected,
		DiscoveryDeferred,
		DiscoveryMerged,
	} {
		if !status.Valid() {
			t.Errorf("status %q is invalid", status)
		}
	}
	if DiscoveryStatus("unknown").Valid() {
		t.Error("unknown status accepted")
	}
	if DiscoveryUnevaluated.Evaluated() {
		t.Error("unevaluated status reported as evaluated")
	}
	for _, status := range []DiscoveryStatus{
		DiscoveryAccepted, DiscoveryRejected, DiscoveryDeferred, DiscoveryMerged,
	} {
		if !status.Evaluated() {
			t.Errorf("status %q not reported as evaluated", status)
		}
	}
}

func TestDiscoverySeverityRanksByOrderNotAlphabet(t *testing.T) {
	// "critical" < "low" lexically, so a string comparison would invert this.
	if !SeverityCritical.AtLeast(SeverityLow) {
		t.Error("critical is not at least low")
	}
	if SeverityLow.AtLeast(SeverityHigh) {
		t.Error("low reported as at least high")
	}
	if !SeverityHigh.AtLeast(SeverityHigh) {
		t.Error("high is not at least high")
	}
	if DiscoverySeverity("unknown").AtLeast(SeverityLow) {
		t.Error("unknown severity outranked low")
	}
}

func TestDiscoveryRecommendActionFollowsPlanPrecedence(t *testing.T) {
	tests := []struct {
		name      string
		discovery Discovery
		want      DiscoveryAction
	}{
		{
			"blocking work outranks everything",
			Discovery{
				BlocksCurrent:    true,
				Severity:         SeverityCritical,
				ArchitectureRisk: true,
				Category:         DiscoverySecurity,
			},
			ActionFixInCurrentTask,
		},
		{
			"critical outranks architecture risk",
			Discovery{Severity: SeverityCritical, ArchitectureRisk: true},
			ActionCreateNextTask,
		},
		{
			"architecture risk outranks security blocker",
			Discovery{
				Severity:         SeverityHigh,
				ArchitectureRisk: true,
				Category:         DiscoverySecurity,
			},
			ActionRequestArchitectureRvw,
		},
		{
			"high security becomes a release blocker",
			Discovery{Severity: SeverityHigh, Category: DiscoverySecurity},
			ActionCreateReleaseBlocker,
		},
		{
			"medium security is only prioritized",
			Discovery{Severity: SeverityMedium, Category: DiscoverySecurity},
			ActionEvaluatePriority,
		},
		{
			"high non-security is only prioritized",
			Discovery{Severity: SeverityHigh, Category: DiscoveryTechnicalDebt},
			ActionEvaluatePriority,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			discovery := test.discovery
			if got := discovery.RecommendAction(); got != test.want {
				t.Errorf("RecommendAction() = %q, want %q", got, test.want)
			}
		})
	}
	if (*Discovery)(nil).RecommendAction() != ActionEvaluatePriority {
		t.Error("nil discovery did not default to priority evaluation")
	}
}

func TestDiscoveryValidation(t *testing.T) {
	valid := func() *Discovery {
		discovery := NewDiscovery(7, 3, 9, "Missing retry budget", DiscoveryBug, SeverityHigh)
		return discovery
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*Discovery)
	}{
		{"missing project", func(d *Discovery) { d.ProjectID = 0 }},
		{"negative task", func(d *Discovery) { d.SourceTaskID = -1 }},
		{"negative execution", func(d *Discovery) { d.SourceExecutionID = -1 }},
		{"blank title", func(d *Discovery) { d.Title = "   " }},
		{"unknown category", func(d *Discovery) { d.Category = "nonsense" }},
		{"unknown severity", func(d *Discovery) { d.Severity = "nonsense" }},
		{"unknown status", func(d *Discovery) { d.Status = "nonsense" }},
		{"evaluated without reason", func(d *Discovery) { d.Status = DiscoveryAccepted }},
		{"negative issue", func(d *Discovery) { d.CreatedIssueNumber = -1 }},
		{"negative position", func(d *Discovery) { d.BacklogPosition = -1 }},
		{"padded external ID", func(d *Discovery) { d.ExternalID = " abc " }},
	}
	for _, test := range cases {
		test := test
		t.Run(test.name, func(t *testing.T) {
			discovery := valid()
			test.mutate(discovery)
			if err := discovery.Validate(); !errors.Is(err, ErrInvalidDiscovery) {
				t.Errorf("Validate error = %v", err)
			}
		})
	}
	evaluated := valid()
	evaluated.Status = DiscoveryAccepted
	evaluated.DecisionReason = "Accepted as the next task"
	if err := evaluated.Validate(); err != nil {
		t.Errorf("evaluated Validate: %v", err)
	}
	if err := (*Discovery)(nil).Validate(); !errors.Is(err, ErrInvalidDiscovery) {
		t.Errorf("nil Validate error = %v", err)
	}
}

func TestNormalizeDiscoveryTitleFoldsCosmeticDifferences(t *testing.T) {
	same := []string{
		"Retry budget is unbounded",
		"  retry   budget is unbounded!  ",
		"RETRY-BUDGET, IS UNBOUNDED.",
		"Retry\tbudget\nis unbounded",
	}
	want := NormalizeDiscoveryTitle(same[0])
	if want != "retry budget is unbounded" {
		t.Fatalf("normalized = %q", want)
	}
	for _, title := range same[1:] {
		if got := NormalizeDiscoveryTitle(title); got != want {
			t.Errorf("NormalizeDiscoveryTitle(%q) = %q, want %q", title, got, want)
		}
	}
	if NormalizeDiscoveryTitle("   ") != "" {
		t.Error("blank title did not normalize to empty")
	}
	if NormalizeDiscoveryTitle("a b") == NormalizeDiscoveryTitle("ab") {
		t.Error("distinct titles collapsed to the same form")
	}
}

func TestDiscoveryContentHashIsStableAndDiscriminating(t *testing.T) {
	base := NewDiscovery(7, 1, 1, "Retry budget is unbounded", DiscoveryBug, SeverityHigh)
	hash := base.ContentHash()
	if hash == "" || len(hash) != len("disc-")+16 {
		t.Fatalf("hash = %q", hash)
	}
	if base.ContentHash() != hash {
		t.Error("hash is not stable across calls")
	}

	// Cosmetic differences and non-identifying fields must not change identity.
	cosmetic := NewDiscovery(9, 2, 2, "  RETRY budget, is unbounded!  ", DiscoveryBug, SeverityLow)
	cosmetic.Description = "totally different description"
	cosmetic.BlocksCurrent = true
	if cosmetic.ContentHash() != hash {
		t.Errorf("cosmetic variant hash = %q, want %q", cosmetic.ContentHash(), hash)
	}

	// Category and title are identifying.
	other := NewDiscovery(7, 1, 1, "Retry budget is unbounded", DiscoveryTesting, SeverityHigh)
	if other.ContentHash() == hash {
		t.Error("different category produced the same hash")
	}
	renamed := NewDiscovery(7, 1, 1, "Retry budget is missing", DiscoveryBug, SeverityHigh)
	if renamed.ContentHash() == hash {
		t.Error("different title produced the same hash")
	}
	if (*Discovery)(nil).ContentHash() != "" {
		t.Error("nil discovery produced a hash")
	}
}

func TestDiscoveryFingerprintCombinesCategoryAndTitle(t *testing.T) {
	if got := DiscoveryFingerprint(DiscoveryBug, " Broken  Thing "); got != "bug|broken thing" {
		t.Errorf("DiscoveryFingerprint = %q", got)
	}
}

func TestDiscoveryRequiresArchitectureReview(t *testing.T) {
	cases := []struct {
		name      string
		discovery Discovery
		want      bool
	}{
		{
			"unjudged architecture risk",
			Discovery{ArchitectureRisk: true, Status: DiscoveryUnevaluated},
			true,
		},
		{
			"escalated without the risk flag",
			Discovery{Status: DiscoveryDeferred, Decision: DecisionRequestArchitecture},
			true,
		},
		{
			"risk resolved by another verdict",
			Discovery{
				ArchitectureRisk: true,
				Status:           DiscoveryRejected,
				Decision:         DecisionRejectOutOfScope,
			},
			false,
		},
		{
			"risk accepted as work",
			Discovery{
				ArchitectureRisk: true,
				Status:           DiscoveryAccepted,
				Decision:         DecisionCreateNextTask,
			},
			false,
		},
		{
			"no risk at all",
			Discovery{Status: DiscoveryUnevaluated},
			false,
		},
	}
	for _, test := range cases {
		test := test
		t.Run(test.name, func(t *testing.T) {
			discovery := test.discovery
			if got := discovery.RequiresArchitectureReview(); got != test.want {
				t.Errorf("RequiresArchitectureReview() = %v, want %v", got, test.want)
			}
		})
	}
	if (*Discovery)(nil).RequiresArchitectureReview() {
		t.Error("nil discovery required architecture review")
	}
}
