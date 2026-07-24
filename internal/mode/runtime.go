package mode

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/eslam-mahmoud/go-ai-agent/internal/engine"
)

func validateCompletedModeArtifact(
	validator *engine.OutputValidator,
	raw json.RawMessage,
) (json.RawMessage, *Output, error) {
	result := &engine.Result{
		Status:     engine.ResultCompleted,
		OutputJSON: append(json.RawMessage(nil), raw...),
	}
	if err := validator.ValidateResult(result); err != nil {
		return nil, nil, err
	}
	var envelope Output
	if err := json.Unmarshal(result.OutputJSON, &envelope); err != nil {
		return nil, nil, fmt.Errorf("decode output envelope: %w", err)
	}
	if envelope.Status != OutputCompleted {
		return nil, nil, fmt.Errorf(
			"status must be %q, got %q",
			OutputCompleted,
			envelope.Status,
		)
	}
	return append(json.RawMessage(nil), result.OutputJSON...), &envelope, nil
}

func cloneEngineResult(result *engine.Result) *engine.Result {
	cloned := *result
	cloned.OutputJSON = append(json.RawMessage(nil), result.OutputJSON...)
	return &cloned
}

func cloneStrings(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func isNilEngine(provider engine.Engine) bool {
	return isNilDependency(provider)
}

func isNilDependency(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
