package mode

import (
	"encoding/json"
	"reflect"

	"github.com/eslam-mahmoud/go-ai-agent/internal/engine"
)

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
