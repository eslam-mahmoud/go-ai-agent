package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrInvalidArtifact = errors.New("invalid artifact")

// Artifact is immutable metadata for one durable file in the artifact store.
type Artifact struct {
	ID          int64
	ProjectID   int64
	TaskID      *int64
	ExecutionID *int64
	Kind        string
	Name        string
	Path        string
	MediaType   string
	SHA256      string
	SizeBytes   int64
	CreatedAt   time.Time
}

func NewArtifact(projectID int64, kind, name, path, mediaType, sha256 string, sizeBytes int64) *Artifact {
	return &Artifact{
		ProjectID: projectID,
		Kind:      kind,
		Name:      name,
		Path:      path,
		MediaType: mediaType,
		SHA256:    sha256,
		SizeBytes: sizeBytes,
	}
}

func (artifact *Artifact) Validate() error {
	if artifact == nil {
		return fmt.Errorf("%w: artifact is nil", ErrInvalidArtifact)
	}
	switch {
	case artifact.ProjectID <= 0:
		return fmt.Errorf("%w: project ID must be positive", ErrInvalidArtifact)
	case artifact.TaskID != nil && *artifact.TaskID <= 0:
		return fmt.Errorf("%w: task ID must be positive", ErrInvalidArtifact)
	case artifact.ExecutionID != nil && *artifact.ExecutionID <= 0:
		return fmt.Errorf("%w: execution ID must be positive", ErrInvalidArtifact)
	case strings.TrimSpace(artifact.Kind) == "":
		return fmt.Errorf("%w: kind is required", ErrInvalidArtifact)
	case strings.TrimSpace(artifact.Name) == "":
		return fmt.Errorf("%w: name is required", ErrInvalidArtifact)
	case strings.TrimSpace(artifact.Path) == "":
		return fmt.Errorf("%w: path is required", ErrInvalidArtifact)
	case strings.ContainsRune(artifact.Path, '\x00'):
		return fmt.Errorf("%w: path contains a null byte", ErrInvalidArtifact)
	case strings.TrimSpace(artifact.MediaType) == "":
		return fmt.Errorf("%w: media type is required", ErrInvalidArtifact)
	case !validSHA256(artifact.SHA256):
		return fmt.Errorf("%w: SHA-256 must be 64 hexadecimal characters", ErrInvalidArtifact)
	case artifact.SizeBytes < 0:
		return fmt.Errorf("%w: size cannot be negative", ErrInvalidArtifact)
	default:
		return nil
	}
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') &&
			(character < 'a' || character > 'f') &&
			(character < 'A' || character > 'F') {
			return false
		}
	}
	return true
}
