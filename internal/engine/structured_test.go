package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const testObjectSchema = `{
  "$schema":"https://json-schema.org/draft/2020-12/schema",
  "type":"object",
  "properties":{"status":{"type":"string"},"count":{"type":"integer"}},
  "required":["status","count"],
  "additionalProperties":false
}`

func TestOutputValidatorAcceptsAndNormalizesJSON(t *testing.T) {
	validator, err := CompileOutputSchema(json.RawMessage(testObjectSchema))
	if err != nil {
		t.Fatalf("CompileOutputSchema: %v", err)
	}
	result := &Result{OutputText: ` { "count": 9007199254740993, "status": "completed" } `}
	if err := validator.ValidateResult(result); err != nil {
		t.Fatalf("ValidateResult: %v", err)
	}
	if string(result.OutputJSON) != `{"count":9007199254740993,"status":"completed"}` {
		t.Errorf("OutputJSON = %s", result.OutputJSON)
	}
}

func TestOutputValidatorSupportsLocalReferences(t *testing.T) {
	schema := json.RawMessage(`{
		"$defs":{"status":{"type":"string","const":"completed"}},
		"type":"object",
		"properties":{"status":{"$ref":"#/$defs/status"}},
		"required":["status"]
	}`)
	validator, err := CompileOutputSchema(schema)
	if err != nil {
		t.Fatalf("CompileOutputSchema: %v", err)
	}
	result := &Result{OutputText: `{"status":"completed"}`}
	if err := validator.ValidateResult(result); err != nil {
		t.Fatalf("ValidateResult: %v", err)
	}
}

func TestCompileOutputSchemaRejectsInvalidSchemas(t *testing.T) {
	for _, schema := range []string{
		"",
		`{`,
		`{"type":"not-a-type"}`,
		`{"$ref":"https://example.com/external-schema.json"}`,
	} {
		validator, err := CompileOutputSchema(json.RawMessage(schema))
		if validator != nil || !errors.Is(err, ErrInvalidOutputSchema) {
			t.Errorf("schema %q: validator=%#v error=%v", schema, validator, err)
		}
	}
}

func TestOutputValidatorRejectsInvalidResults(t *testing.T) {
	validator, err := CompileOutputSchema(json.RawMessage(testObjectSchema))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		result *Result
		want   error
	}{
		{"nil", nil, ErrStructuredOutputMissing},
		{"empty", &Result{}, ErrStructuredOutputMissing},
		{"malformed", &Result{OutputText: `{"status":`}, ErrStructuredOutputMalformed},
		{"trailing", &Result{OutputText: `{} {}`}, ErrStructuredOutputMalformed},
		{"mismatch", &Result{OutputText: `{"status":"completed","count":"one"}`}, ErrStructuredOutputMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validator.ValidateResult(tc.result)
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
			if tc.result != nil && len(tc.result.OutputJSON) != 0 {
				t.Errorf("invalid result populated OutputJSON: %s", tc.result.OutputJSON)
			}
		})
	}
}

func TestValidationErrorsAreBounded(t *testing.T) {
	properties := strings.Repeat(`"very_long_property_name":{"type":"string"},`, 300)
	schema := `{"type":"object","properties":{` + strings.TrimSuffix(properties, ",") + `},"additionalProperties":false}`
	validator, err := CompileOutputSchema(json.RawMessage(schema))
	if err != nil {
		t.Fatal(err)
	}
	err = validator.ValidateResult(&Result{OutputText: `{"unexpected":true}`})
	if err == nil || len(err.Error()) > maxStructuredValidationErrorBytes+100 {
		t.Errorf("unbounded validation error length = %d", len(err.Error()))
	}
}
