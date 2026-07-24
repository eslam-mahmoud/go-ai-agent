package mode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

func TestDurableArchitectContextComposesProjectAndRepositoryScan(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	writeScanFixture(t, workspace, "README.md", "# Madar\n\nAutonomous delivery.")
	writeScanFixture(t, workspace, "go.mod", "module example.com/madar")
	writeScanFixture(t, workspace, "docs/architecture/overview.md", "# Overview")

	risk := &domain.Discovery{
		ID:          9,
		Title:       "Cross-cutting cache change",
		Description: "Touches every read path.",
		Category:    domain.DiscoveryArchitecture,
		Severity:    domain.SeverityHigh,
		Decision:    domain.DecisionRequestArchitecture,
	}
	provider, err := NewDurableArchitectContextProvider(
		architectProjectLoaderFunc(func(projectID int64) (*ArchitectProject, error) {
			if projectID != 7 {
				t.Fatalf("loader project ID = %d", projectID)
			}
			return &ArchitectProject{
				Name:                "Madar",
				Goal:                "Ship v2",
				Scope:               "Sequential delivery",
				Repo:                "owner/repo",
				ArchitectureVersion: 2,
				OutstandingRisks:    []*domain.Discovery{risk, nil},
			}, nil
		}),
		ArchitectRuntimeContextProviderFunc(func(
			context.Context, int64,
		) (ArchitectRuntimeContext, error) {
			return ArchitectRuntimeContext{WorkDir: workspace, ExecutionID: 33}, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	architectContext, err := provider.LoadArchitectContext(context.Background(), 7)
	if err != nil {
		t.Fatalf("LoadArchitectContext: %v", err)
	}
	if architectContext.ProjectID != 7 || architectContext.ExecutionID != 33 {
		t.Fatalf("context = %#v", architectContext)
	}
	// A nil risk in the durable set must not become a phantom ID.
	if len(architectContext.OutstandingDiscoveryIDs) != 1 ||
		architectContext.OutstandingDiscoveryIDs[0] != 9 {
		t.Fatalf("outstanding IDs = %v", architectContext.OutstandingDiscoveryIDs)
	}

	var snapshot struct {
		Project struct {
			Name                string `json:"name"`
			ArchitectureVersion int    `json:"architecture_version"`
		} `json:"project"`
		Repository struct {
			Ecosystems       []string `json:"ecosystems"`
			HasArchitecture  bool     `json:"has_architecture"`
			ReadmeExcerpt    string   `json:"readme_excerpt"`
			ArchitectureDocs []struct {
				Path string `json:"path"`
			} `json:"architecture_docs"`
		} `json:"repository"`
		OutstandingRisks []struct {
			ID       int64  `json:"id"`
			Decision string `json:"decision"`
		} `json:"outstanding_risks"`
	}
	if err := json.Unmarshal(architectContext.Snapshot, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Project.Name != "Madar" || snapshot.Project.ArchitectureVersion != 2 {
		t.Fatalf("project snapshot = %#v", snapshot.Project)
	}
	if len(snapshot.Repository.Ecosystems) != 1 ||
		snapshot.Repository.Ecosystems[0] != "go" ||
		!snapshot.Repository.HasArchitecture ||
		len(snapshot.Repository.ArchitectureDocs) != 1 {
		t.Fatalf("repository snapshot = %#v", snapshot.Repository)
	}
	if !strings.Contains(snapshot.Repository.ReadmeExcerpt, "Autonomous delivery") {
		t.Fatalf("readme excerpt = %q", snapshot.Repository.ReadmeExcerpt)
	}
	if len(snapshot.OutstandingRisks) != 1 ||
		snapshot.OutstandingRisks[0].ID != 9 ||
		snapshot.OutstandingRisks[0].Decision != string(domain.DecisionRequestArchitecture) {
		t.Fatalf("risk snapshot = %#v", snapshot.OutstandingRisks)
	}

	// The composed context must satisfy the mode that consumes it.
	architect, err := NewArchitect(
		successfulArchitectEngine(validArchitectOutput(OutputCompleted, map[string]any{
			"addressed_discovery_ids": []int64{9},
		})),
		provider,
		ArchitectOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := architect.Run(context.Background(), workflow.ModeRequest{
		ProjectID: 7,
		Mode:      workflow.ModeArchitect,
	}); err != nil {
		t.Fatalf("architect run with composed context: %v", err)
	}
}

func TestDurableArchitectContextRejectsUnusableInput(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	loader := architectProjectLoaderFunc(func(int64) (*ArchitectProject, error) {
		return &ArchitectProject{Name: "Madar", Goal: "Ship"}, nil
	})
	runtime := ArchitectRuntimeContextProviderFunc(func(
		context.Context, int64,
	) (ArchitectRuntimeContext, error) {
		return ArchitectRuntimeContext{WorkDir: workspace}, nil
	})

	if _, err := NewDurableArchitectContextProvider(nil, runtime); err == nil {
		t.Error("missing loader accepted")
	}
	if _, err := NewDurableArchitectContextProvider(loader, nil); err == nil {
		t.Error("missing runtime provider accepted")
	}

	provider, err := NewDurableArchitectContextProvider(loader, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.LoadArchitectContext(context.Background(), 0); !errors.Is(
		err, ErrInvalidArchitectSnapshot,
	) {
		t.Fatalf("zero project error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.LoadArchitectContext(ctx, 7); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v", err)
	}

	nilProject, err := NewDurableArchitectContextProvider(
		architectProjectLoaderFunc(func(int64) (*ArchitectProject, error) {
			return nil, nil
		}),
		runtime,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nilProject.LoadArchitectContext(context.Background(), 7); !errors.Is(
		err, ErrInvalidArchitectSnapshot,
	) {
		t.Fatalf("nil project error = %v", err)
	}

	relative, err := NewDurableArchitectContextProvider(
		loader,
		ArchitectRuntimeContextProviderFunc(func(
			context.Context, int64,
		) (ArchitectRuntimeContext, error) {
			return ArchitectRuntimeContext{WorkDir: "relative/path"}, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := relative.LoadArchitectContext(context.Background(), 7); !errors.Is(
		err, ErrInvalidArchitectSnapshot,
	) {
		t.Fatalf("relative workspace error = %v", err)
	}
}

type architectProjectLoaderFunc func(int64) (*ArchitectProject, error)

func (load architectProjectLoaderFunc) LoadArchitectProject(
	projectID int64,
) (*ArchitectProject, error) {
	return load(projectID)
}

func writeScanFixture(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
