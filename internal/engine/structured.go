package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const maxStructuredValidationErrorBytes = 4 * 1024

var (
	ErrInvalidOutputSchema       = errors.New("invalid output schema")
	ErrStructuredOutputMissing   = errors.New("structured output is missing")
	ErrStructuredOutputMalformed = errors.New("structured output is malformed")
	ErrStructuredOutputMismatch  = errors.New("structured output does not match schema")
)

// OutputValidator is a compiled JSON Schema used to validate a provider's
// normalized terminal result locally.
type OutputValidator struct {
	schema *jsonschema.Schema
}

// CompileOutputSchema parses and compiles a JSON Schema. Provider adapters call
// this before starting a process so an invalid request cannot consume a run.
func CompileOutputSchema(raw json.RawMessage) (*OutputValidator, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, ErrInvalidOutputSchema
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidOutputSchema, boundedValidationMessage(err))
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(localOnlySchemaLoader{})
	if err := compiler.AddResource("madar-output-schema.json", document); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidOutputSchema, boundedValidationMessage(err))
	}
	schema, err := compiler.Compile("madar-output-schema.json")
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidOutputSchema, boundedValidationMessage(err))
	}
	return &OutputValidator{schema: schema}, nil
}

type localOnlySchemaLoader struct{}

func (localOnlySchemaLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema reference %q is disabled", url)
}

// ValidateResult parses and validates a result, then stores its compact JSON in
// OutputJSON. OutputText remains unchanged for diagnostics and compatibility.
func (v *OutputValidator) ValidateResult(result *Result) error {
	if v == nil || v.schema == nil {
		return ErrInvalidOutputSchema
	}
	if result == nil {
		return ErrStructuredOutputMissing
	}

	raw := result.OutputJSON
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(result.OutputText)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return ErrStructuredOutputMissing
	}

	value, err := decodeSingleJSONValue(raw)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrStructuredOutputMalformed, boundedValidationMessage(err))
	}
	if err := v.schema.Validate(value); err != nil {
		return fmt.Errorf("%w: %s", ErrStructuredOutputMismatch, boundedValidationMessage(err))
	}

	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return fmt.Errorf("%w: %s", ErrStructuredOutputMalformed, boundedValidationMessage(err))
	}
	result.OutputJSON = append(result.OutputJSON[:0], compact.Bytes()...)
	return nil
}

func decodeSingleJSONValue(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return value, nil
}

func boundedValidationMessage(err error) string {
	message := strings.ToValidUTF8(err.Error(), "\uFFFD")
	if len(message) <= maxStructuredValidationErrorBytes {
		return message
	}
	return message[:maxStructuredValidationErrorBytes] + " [truncated]"
}
