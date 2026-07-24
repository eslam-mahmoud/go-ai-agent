package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestArtifactValidation(t *testing.T) {
	digest := strings.Repeat("a", 64)
	artifact := NewArtifact(1, "plan", "Task plan", "plans/task-1.json", "application/json", digest, 128)
	if err := artifact.Validate(); err != nil {
		t.Fatal(err)
	}

	zero := int64(0)
	cases := []struct {
		name   string
		mutate func(*Artifact)
	}{
		{"project", func(a *Artifact) { a.ProjectID = 0 }},
		{"task", func(a *Artifact) { a.TaskID = &zero }},
		{"execution", func(a *Artifact) { a.ExecutionID = &zero }},
		{"kind", func(a *Artifact) { a.Kind = "" }},
		{"name", func(a *Artifact) { a.Name = "" }},
		{"path", func(a *Artifact) { a.Path = "" }},
		{"null path", func(a *Artifact) { a.Path = "bad\x00path" }},
		{"media type", func(a *Artifact) { a.MediaType = "" }},
		{"digest", func(a *Artifact) { a.SHA256 = "bad" }},
		{"size", func(a *Artifact) { a.SizeBytes = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			copy := *artifact
			tc.mutate(&copy)
			if err := copy.Validate(); !errors.Is(err, ErrInvalidArtifact) {
				t.Errorf("Validate error = %v", err)
			}
		})
	}
	if err := (*Artifact)(nil).Validate(); !errors.Is(err, ErrInvalidArtifact) {
		t.Errorf("nil Validate error = %v", err)
	}
}
