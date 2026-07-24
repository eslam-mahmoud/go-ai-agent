package engine

import (
	"errors"
	"strings"
	"testing"
)

func TestErrorClassesHaveStableValues(t *testing.T) {
	cases := map[ErrorClass]string{
		ErrorProviderUnavailable: "provider-unavailable",
		ErrorAuthentication:      "authentication",
		ErrorRateLimit:           "rate-limit",
		ErrorTimeout:             "timeout",
		ErrorProcessExit:         "process-exit",
		ErrorInvalidOutput:       "invalid-output",
		ErrorSessionMissing:      "session-missing",
		ErrorSessionResumeFailed: "session-resume-failed",
		ErrorPolicyDenied:        "policy-denied",
		ErrorWorkspaceInvalid:    "workspace-invalid",
		ErrorCancelled:           "cancelled",
		ErrorUnknown:             "unknown",
	}

	for class, want := range cases {
		if got := string(class); got != want {
			t.Errorf("class value = %q, want %q", got, want)
		}
	}
}

func TestExecutionErrorPreservesMetadataAndCause(t *testing.T) {
	cause := errors.New("provider stopped")
	err := NewExecutionError(ErrorProcessExit, "claude", "run", cause)

	if err.Class != ErrorProcessExit {
		t.Errorf("Class = %q, want %q", err.Class, ErrorProcessExit)
	}
	if err.Provider != "claude" {
		t.Errorf("Provider = %q, want claude", err.Provider)
	}
	if err.Operation != "run" {
		t.Errorf("Operation = %q, want run", err.Operation)
	}
	if !errors.Is(err, cause) {
		t.Fatal("ExecutionError does not unwrap its cause")
	}
	if got := ClassOf(err); got != ErrorProcessExit {
		t.Errorf("ClassOf = %q, want %q", got, ErrorProcessExit)
	}
	for _, want := range []string{"claude", "run", "process-exit", "provider stopped"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestNewExecutionErrorDefaultsEmptyClassToUnknown(t *testing.T) {
	err := NewExecutionError("", "codex", "resume", errors.New("boom"))
	if err.Class != ErrorUnknown {
		t.Errorf("Class = %q, want %q", err.Class, ErrorUnknown)
	}
}

func TestClassOfPlainAndNilErrors(t *testing.T) {
	if got := ClassOf(errors.New("plain")); got != ErrorUnknown {
		t.Errorf("ClassOf(plain) = %q, want %q", got, ErrorUnknown)
	}
	if got := ClassOf(nil); got != ErrorUnknown {
		t.Errorf("ClassOf(nil) = %q, want %q", got, ErrorUnknown)
	}
}
