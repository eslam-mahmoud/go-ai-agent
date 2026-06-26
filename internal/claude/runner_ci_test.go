package claude

import (
	"strings"
	"testing"
)

func TestBuildCIFixPrompt(t *testing.T) {
	output := "FAIL: TestFoo expected 1 got 2"
	prompt := BuildCIFixPrompt(output, "madar/issue-42", 99, 1, 3)

	if !strings.Contains(prompt, "attempt 1 of 3") {
		t.Errorf("prompt missing attempt info: %q", prompt)
	}
	if !strings.Contains(prompt, "TestFoo expected 1 got 2") {
		t.Errorf("prompt missing failure output: %q", prompt)
	}
	if !strings.Contains(prompt, "NEEDS_CLARIFICATION") {
		t.Errorf("prompt missing escalation instruction: %q", prompt)
	}
	if !strings.Contains(prompt, "madar/issue-42") {
		t.Errorf("prompt missing branch name: %q", prompt)
	}
	if !strings.Contains(prompt, "PR #99") {
		t.Errorf("prompt missing PR number: %q", prompt)
	}
	if !strings.Contains(prompt, "Do NOT create a new branch") {
		t.Errorf("prompt missing anti-new-branch instruction: %q", prompt)
	}
}

func TestBuildCIFixPrompt_lastAttempt(t *testing.T) {
	prompt := BuildCIFixPrompt("error output", "madar/issue-5", 12, 3, 3)
	if !strings.Contains(prompt, "attempt 3 of 3") {
		t.Errorf("prompt missing final attempt info: %q", prompt)
	}
}
