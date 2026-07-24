package engine

import (
	"errors"
	"fmt"
)

// ErrorClass is the provider-neutral classification for an execution failure.
type ErrorClass string

const (
	ErrorProviderUnavailable ErrorClass = "provider-unavailable"
	ErrorAuthentication      ErrorClass = "authentication"
	ErrorRateLimit           ErrorClass = "rate-limit"
	ErrorTimeout             ErrorClass = "timeout"
	ErrorProcessExit         ErrorClass = "process-exit"
	ErrorInvalidOutput       ErrorClass = "invalid-output"
	ErrorSessionMissing      ErrorClass = "session-missing"
	ErrorSessionResumeFailed ErrorClass = "session-resume-failed"
	ErrorPolicyDenied        ErrorClass = "policy-denied"
	ErrorWorkspaceInvalid    ErrorClass = "workspace-invalid"
	ErrorCancelled           ErrorClass = "cancelled"
	ErrorUnknown             ErrorClass = "unknown"
)

// ExecutionError adds stable workflow metadata while preserving the original
// cause for errors.Is and errors.As.
type ExecutionError struct {
	Class     ErrorClass
	Provider  string
	Operation string
	Err       error
}

func NewExecutionError(class ErrorClass, provider, operation string, cause error) *ExecutionError {
	if class == "" {
		class = ErrorUnknown
	}
	return &ExecutionError{
		Class:     class,
		Provider:  provider,
		Operation: operation,
		Err:       cause,
	}
}

func (e *ExecutionError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err == nil {
		return fmt.Sprintf("%s %s (%s)", e.Provider, e.Operation, e.Class)
	}
	return fmt.Sprintf("%s %s (%s): %v", e.Provider, e.Operation, e.Class, e.Err)
}

func (e *ExecutionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ClassOf returns the stable execution class, or unknown for unclassified
// and nil errors.
func ClassOf(err error) ErrorClass {
	var executionErr *ExecutionError
	if errors.As(err, &executionErr) {
		return executionErr.Class
	}
	return ErrorUnknown
}
