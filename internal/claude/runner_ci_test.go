package claude

import (
	"strings"
	"testing"
)

func TestBuildCIFixPrompt(t *testing.T) {
	output := "FAIL: TestFoo expected 1 got 2"
	prompt := BuildCIFixPrompt(output, 1, 3)

	if !strings.Contains(prompt, "attempt 1 of 3") {
		t.Errorf("prompt missing attempt info: %q", prompt)
	}
	if !strings.Contains(prompt, "TestFoo expected 1 got 2") {
		t.Errorf("prompt missing failure output: %q", prompt)
	}
	if !strings.Contains(prompt, "NEEDS_CLARIFICATION") {
		t.Errorf("prompt missing escalation instruction: %q", prompt)
	}
	if !strings.Contains(prompt, "push the corrected code") {
		t.Errorf("prompt missing push instruction: %q", prompt)
	}
}

func TestBuildCIFixPrompt_lastAttempt(t *testing.T) {
	prompt := BuildCIFixPrompt("error output", 3, 3)
	if !strings.Contains(prompt, "attempt 3 of 3") {
		t.Errorf("prompt missing final attempt info: %q", prompt)
	}
}
