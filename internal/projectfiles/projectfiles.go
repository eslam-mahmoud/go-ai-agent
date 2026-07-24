// Package projectfiles renders and persists deterministic repository snapshots
// of Madar's durable project state.
package projectfiles

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"gopkg.in/yaml.v3"
)

const (
	DirectoryName   = ".madar"
	ProjectFileName = "project.yaml"
	PlanFileName    = "plan.md"
	formatVersion   = 1
)

var (
	ErrInvalidSnapshot = errors.New("invalid project file snapshot")
	ErrUnsafeDirectory = errors.New("unsafe project file directory")
)

// Files contains the complete byte representation of the two deterministic
// project files.
type Files struct {
	ProjectYAML  []byte
	PlanMarkdown []byte
}

type yamlSnapshot struct {
	Version int         `yaml:"version"`
	Project yamlProject `yaml:"project"`
	Backlog []yamlTask  `yaml:"backlog"`
}

type yamlProject struct {
	ID                  int64                `yaml:"id"`
	Repository          string               `yaml:"repository"`
	Name                string               `yaml:"name"`
	Goal                string               `yaml:"goal"`
	Scope               string               `yaml:"scope"`
	State               domain.ProjectState  `yaml:"state"`
	Health              domain.ProjectHealth `yaml:"health"`
	ParentIssueNumber   int                  `yaml:"parent_issue_number"`
	CurrentTaskID       *int64               `yaml:"current_task_id"`
	CurrentPlanVersion  int                  `yaml:"current_plan_version"`
	ArchitectureVersion int                  `yaml:"architecture_version"`
	ReleaseTarget       string               `yaml:"release_target"`
	ReleaseReadiness    string               `yaml:"release_readiness"`
	LastManagerReviewAt *string              `yaml:"last_manager_review_at"`
	CreatedAt           string               `yaml:"created_at"`
	UpdatedAt           string               `yaml:"updated_at"`
}

type yamlTask struct {
	ID                int64             `yaml:"id"`
	Sequence          int               `yaml:"sequence"`
	IssueNumber       int               `yaml:"issue_number"`
	Title             string            `yaml:"title"`
	Goal              string            `yaml:"goal"`
	Status            domain.TaskStatus `yaml:"status"`
	Priority          int               `yaml:"priority"`
	Type              string            `yaml:"type"`
	Source            string            `yaml:"source"`
	SourceDiscoveryID *int64            `yaml:"source_discovery_id"`
	BlocksRelease     bool              `yaml:"blocks_release"`
	SelectedReason    string            `yaml:"selected_reason"`
	BranchName        string            `yaml:"branch_name"`
	PullRequestNumber int               `yaml:"pull_request_number"`
	DependencyState   string            `yaml:"dependency_state"`
}

// Render validates, orders, and serializes a persisted project snapshot.
// Volatile generation timestamps are deliberately excluded so equal inputs
// always produce byte-identical files.
func Render(project *domain.Project, tasks []*domain.Task) (Files, error) {
	if err := validateSnapshot(project, tasks); err != nil {
		return Files{}, err
	}
	ordered := append([]*domain.Task(nil), tasks...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Sequence == ordered[j].Sequence {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].Sequence < ordered[j].Sequence
	})

	snapshot := yamlSnapshot{
		Version: formatVersion,
		Project: yamlProject{
			ID:                  project.ID,
			Repository:          project.Repo,
			Name:                project.Name,
			Goal:                project.Goal,
			Scope:               project.Scope,
			State:               project.State,
			Health:              project.Health,
			ParentIssueNumber:   project.ParentIssueNumber,
			CurrentTaskID:       copyInt64(project.CurrentTaskID),
			CurrentPlanVersion:  project.CurrentPlanVersion,
			ArchitectureVersion: project.ArchitectureVersion,
			ReleaseTarget:       project.ReleaseTarget,
			ReleaseReadiness:    project.ReleaseReadiness,
			LastManagerReviewAt: formatOptionalTime(project.LastManagerReviewAt),
			CreatedAt:           formatTime(project.CreatedAt),
			UpdatedAt:           formatTime(project.UpdatedAt),
		},
		Backlog: make([]yamlTask, 0, len(ordered)),
	}
	for _, task := range ordered {
		snapshot.Backlog = append(snapshot.Backlog, yamlTask{
			ID:                task.ID,
			Sequence:          task.Sequence,
			IssueNumber:       task.IssueNumber,
			Title:             task.Title,
			Goal:              task.Goal,
			Status:            task.Status,
			Priority:          task.Priority,
			Type:              task.TaskType,
			Source:            task.Source,
			SourceDiscoveryID: copyInt64(task.SourceDiscoveryID),
			BlocksRelease:     task.BlocksRelease,
			SelectedReason:    task.SelectedReason,
			BranchName:        task.BranchName,
			PullRequestNumber: task.PRNumber,
			DependencyState:   task.DependencyState,
		})
	}

	projectYAML, err := yaml.Marshal(snapshot)
	if err != nil {
		return Files{}, fmt.Errorf("render project YAML: %w", err)
	}
	return Files{
		ProjectYAML:  projectYAML,
		PlanMarkdown: renderPlan(project, ordered),
	}, nil
}

func validateSnapshot(project *domain.Project, tasks []*domain.Task) error {
	if err := project.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSnapshot, err)
	}
	if project.ID <= 0 {
		return fmt.Errorf("%w: project ID must be positive", ErrInvalidSnapshot)
	}
	if project.CreatedAt.IsZero() || project.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: project timestamps are required", ErrInvalidSnapshot)
	}

	sequences := make(map[int]struct{}, len(tasks))
	for index, task := range tasks {
		if task == nil {
			return fmt.Errorf("%w: task %d is nil", ErrInvalidSnapshot, index)
		}
		if err := task.Validate(); err != nil {
			return fmt.Errorf("%w: task %d: %v", ErrInvalidSnapshot, index, err)
		}
		if task.ID <= 0 {
			return fmt.Errorf("%w: task %d ID must be positive", ErrInvalidSnapshot, index)
		}
		if task.ProjectID != project.ID {
			return fmt.Errorf(
				"%w: task %d belongs to project %d, not %d",
				ErrInvalidSnapshot,
				task.ID,
				task.ProjectID,
				project.ID,
			)
		}
		if task.Sequence <= 0 {
			return fmt.Errorf("%w: task %d sequence must be positive", ErrInvalidSnapshot, task.ID)
		}
		if _, exists := sequences[task.Sequence]; exists {
			return fmt.Errorf("%w: duplicate task sequence %d", ErrInvalidSnapshot, task.Sequence)
		}
		sequences[task.Sequence] = struct{}{}
	}
	return nil
}

func renderPlan(project *domain.Project, tasks []*domain.Task) []byte {
	var output bytes.Buffer
	fmt.Fprintf(&output, "# Project Plan: %s\n\n", headingText(project.Name))
	fmt.Fprintln(&output, "> Generated by Madar from durable project state. Do not edit by hand.")
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "## Goal")
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, markdownBlock(project.Goal))
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "## Scope")
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, markdownBlockOrDash(project.Scope))
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "## Project Status")
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "| Repository | State | Health | Release target | Release readiness |")
	fmt.Fprintln(&output, "| --- | --- | --- | --- | --- |")
	fmt.Fprintf(
		&output,
		"| %s | %s | %s | %s | %s |\n",
		tableCell(project.Repo),
		tableCell(string(project.State)),
		tableCell(string(project.Health)),
		tableCellOrDash(project.ReleaseTarget),
		tableCellOrDash(project.ReleaseReadiness),
	)
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "## Ordered Backlog")
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "| Seq | Issue | Status | Priority | Task | Goal | Release blocker |")
	fmt.Fprintln(&output, "| ---: | ---: | --- | ---: | --- | --- | :---: |")
	if len(tasks) == 0 {
		fmt.Fprintln(&output, "| - | - | - | - | _No tasks_ | - | - |")
	} else {
		for _, task := range tasks {
			fmt.Fprintf(
				&output,
				"| %d | %s | %s | %d | %s | %s | %s |\n",
				task.Sequence,
				displayNumber(task.IssueNumber),
				tableCell(string(task.Status)),
				task.Priority,
				tableCell(task.Title),
				tableCell(task.Goal),
				displayBool(task.BlocksRelease),
			)
		}
	}
	return output.Bytes()
}

// Write atomically replaces both generated files in workspace/.madar.
func Write(workspace string, files Files) error {
	if len(files.ProjectYAML) == 0 || len(files.PlanMarkdown) == 0 {
		return fmt.Errorf("%w: rendered files must not be empty", ErrInvalidSnapshot)
	}
	info, err := os.Stat(workspace)
	if err != nil {
		return fmt.Errorf("inspect project workspace: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("inspect project workspace: %s is not a directory", workspace)
	}

	outputDir := filepath.Join(workspace, DirectoryName)
	if err := prepareOutputDirectory(outputDir); err != nil {
		return err
	}
	projectPath := filepath.Join(outputDir, ProjectFileName)
	planPath := filepath.Join(outputDir, PlanFileName)
	for _, path := range []string{projectPath, planPath} {
		if err := validateOutputTarget(path); err != nil {
			return err
		}
	}
	if err := atomicWriteFile(projectPath, files.ProjectYAML); err != nil {
		return err
	}
	if err := atomicWriteFile(planPath, files.PlanMarkdown); err != nil {
		return err
	}
	return nil
}

func prepareOutputDirectory(path string) error {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.Mkdir(path, 0o755); err != nil {
			return fmt.Errorf("create project file directory: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("inspect project file directory: %w", err)
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%w: %s is a symbolic link", ErrUnsafeDirectory, path)
	case !info.IsDir():
		return fmt.Errorf("%w: %s is not a directory", ErrUnsafeDirectory, path)
	default:
		return nil
	}
}

func validateOutputTarget(path string) error {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("inspect project file target: %w", err)
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%w: %s is a symbolic link", ErrUnsafeDirectory, path)
	case !info.Mode().IsRegular():
		return fmt.Errorf("%w: %s is not a regular file", ErrUnsafeDirectory, path)
	default:
		return nil
	}
}

func atomicWriteFile(path string, content []byte) (err error) {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary project file: %w", err)
	}
	tempPath := file.Name()
	defer func() {
		if file != nil {
			_ = file.Close()
		}
		_ = os.Remove(tempPath)
	}()

	if err := file.Chmod(0o644); err != nil {
		return fmt.Errorf("set project file permissions: %w", err)
	}
	if _, err := file.Write(content); err != nil {
		return fmt.Errorf("write temporary project file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync temporary project file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary project file: %w", err)
	}
	file = nil
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace project file: %w", err)
	}
	return nil
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func formatOptionalTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := formatTime(*value)
	return &formatted
}

func copyInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func headingText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", " ")
	value = strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
	return strings.TrimSpace(value)
}

func markdownBlock(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(value, "\r\n", "\n"))
}

func markdownBlockOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return markdownBlock(value)
}

func tableCell(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "|", "\\|")
	value = strings.NewReplacer("\r", "<br>", "\n", "<br>").Replace(value)
	return strings.TrimSpace(value)
}

func tableCellOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return tableCell(value)
}

func displayNumber(value int) string {
	if value == 0 {
		return "-"
	}
	return strconv.Itoa(value)
}

func displayBool(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
