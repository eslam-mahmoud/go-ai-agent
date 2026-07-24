package mode

import (
	"embed"
	"encoding/json"
	"fmt"

	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

//go:embed schemas/*.json
var schemaFiles embed.FS

var builtinSchemaFiles = map[workflow.ModeName]string{
	workflow.ModePlanner:   "schemas/planner.json",
	workflow.ModeDeveloper: "schemas/developer.json",
	workflow.ModeReviewer:  "schemas/reviewer.json",
	workflow.ModeFixer:     "schemas/fixer.json",
	workflow.ModeVerifier:  "schemas/verifier.json",
}

var builtinDescriptions = map[workflow.ModeName]string{
	workflow.ModePlanner:   "Build a bounded implementation plan and verification contract.",
	workflow.ModeDeveloper: "Implement only the selected task on its assigned branch.",
	workflow.ModeReviewer:  "Independently review the implementation and acceptance evidence.",
	workflow.ModeFixer:     "Address bounded blocking findings without unrelated changes.",
	workflow.ModeVerifier:  "Verify acceptance criteria, repository state, PR consistency, and CI.",
}

// BuiltinDefinition returns a defensive copy of the canonical delivery-mode
// metadata. Reviewer is the only built-in requiring a fresh provider session.
func BuiltinDefinition(name workflow.ModeName) (Definition, error) {
	path, exists := builtinSchemaFiles[name]
	if !exists {
		return Definition{}, fmt.Errorf("%w: built-in %q", ErrModeNotFound, name)
	}
	raw, err := schemaFiles.ReadFile(path)
	if err != nil {
		return Definition{}, fmt.Errorf("read built-in mode schema %q: %w", name, err)
	}
	if !json.Valid(raw) {
		return Definition{}, fmt.Errorf(
			"%w: built-in %q schema is not JSON",
			ErrInvalidModeDefinition,
			name,
		)
	}
	return Definition{
		Name:         name,
		Description:  builtinDescriptions[name],
		OutputSchema: append(json.RawMessage(nil), raw...),
		FreshSession: name == workflow.ModeReviewer,
	}, nil
}

func BuiltinDefinitions() ([]Definition, error) {
	names := []workflow.ModeName{
		workflow.ModePlanner,
		workflow.ModeDeveloper,
		workflow.ModeReviewer,
		workflow.ModeFixer,
		workflow.ModeVerifier,
	}
	definitions := make([]Definition, 0, len(names))
	for _, name := range names {
		definition, err := BuiltinDefinition(name)
		if err != nil {
			return nil, err
		}
		definitions = append(definitions, definition)
	}
	return definitions, nil
}
