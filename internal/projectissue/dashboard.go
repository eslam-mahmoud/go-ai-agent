// Package projectissue owns rendering and synchronizing the managed section of
// a project's human-facing GitHub dashboard issue.
package projectissue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
)

const (
	StartMarker = "<!-- madar:project-dashboard:start -->"
	EndMarker   = "<!-- madar:project-dashboard:end -->"
)

var (
	ErrInvalidDashboard = errors.New("invalid project dashboard")
	ErrInvalidMarkers   = errors.New("invalid project dashboard markers")
)

// Client is the narrow GitHub boundary required to synchronize a dashboard.
type Client interface {
	GetIssue(ctx context.Context, owner, repo string, number int) (*githubclient.Issue, error)
	CreateIssue(ctx context.Context, owner, repo, title, body string, labels []string) (*githubclient.Issue, error)
	UpdateIssueBody(ctx context.Context, owner, repo string, number int, body string) (*githubclient.Issue, error)
}

type SyncResult struct {
	Issue   *githubclient.Issue
	Created bool
}

// Render creates the complete hidden-marker section for a parent issue.
func Render(
	project *domain.Project,
	tasks []*domain.Task,
	review *domain.ManagerReview,
) (string, error) {
	if err := validateDashboard(project, tasks, review); err != nil {
		return "", err
	}
	ordered := append([]*domain.Task(nil), tasks...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Sequence == ordered[j].Sequence {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].Sequence < ordered[j].Sequence
	})

	var output bytes.Buffer
	fmt.Fprintln(&output, StartMarker)
	fmt.Fprintln(&output, "## Madar Project Dashboard")
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "### Goal")
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, markdownBlock(project.Goal))
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "### Status")
	fmt.Fprintln(&output)
	fmt.Fprintf(&output, "- **Health:** %s\n", project.Health)
	fmt.Fprintf(&output, "- **Current phase:** %s\n", project.State)
	fmt.Fprintf(&output, "- **Active issue:** %s\n", activeIssue(project, ordered))
	fmt.Fprintf(&output, "- **Progress estimate:** %d%%\n", progressEstimate(ordered, review))
	fmt.Fprintf(&output, "- **Release target:** %s\n", inlineOrDash(project.ReleaseTarget))
	fmt.Fprintf(&output, "- **Release readiness:** %s\n", inlineOrDash(project.ReleaseReadiness))
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "### Ordered Backlog")
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "| Seq | Issue | Status | Priority | Task | Release blocker |")
	fmt.Fprintln(&output, "| ---: | ---: | --- | ---: | --- | :---: |")
	if len(ordered) == 0 {
		fmt.Fprintln(&output, "| - | - | - | - | _No tasks_ | - |")
	} else {
		for _, task := range ordered {
			fmt.Fprintf(
				&output,
				"| %d | %s | %s | %d | %s | %s |\n",
				task.Sequence,
				issueReference(task.IssueNumber),
				tableCell(string(task.Status)),
				task.Priority,
				tableCell(task.Title),
				displayBool(task.BlocksRelease),
			)
		}
	}
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "### Release Blockers")
	fmt.Fprintln(&output)
	writeReleaseBlockers(&output, ordered)
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "### Risks")
	fmt.Fprintln(&output)
	writeRisks(&output, project, ordered, review)
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "### Recent Discoveries")
	fmt.Fprintln(&output)
	writeDiscoveries(&output, review)
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "### Last Manager Decision")
	fmt.Fprintln(&output)
	writeManagerDecision(&output, review)
	fmt.Fprint(&output, EndMarker)
	return output.String(), nil
}

// MergeManagedSection replaces only Madar's marked section or appends it when
// the existing body contains no markers. Human-authored bytes are preserved.
func MergeManagedSection(existingBody, managedSection string) (string, error) {
	if strings.Count(managedSection, StartMarker) != 1 ||
		strings.Count(managedSection, EndMarker) != 1 ||
		!strings.HasPrefix(managedSection, StartMarker) ||
		!strings.HasSuffix(managedSection, EndMarker) {
		return "", fmt.Errorf("%w: replacement section is not exactly marked", ErrInvalidMarkers)
	}

	startCount := strings.Count(existingBody, StartMarker)
	endCount := strings.Count(existingBody, EndMarker)
	switch {
	case startCount == 0 && endCount == 0:
		if existingBody == "" {
			return managedSection, nil
		}
		separator := "\n\n"
		if strings.HasSuffix(existingBody, "\n\n") {
			separator = ""
		} else if strings.HasSuffix(existingBody, "\n") {
			separator = "\n"
		}
		return existingBody + separator + managedSection, nil
	case startCount != 1 || endCount != 1:
		return "", fmt.Errorf(
			"%w: found %d start markers and %d end markers",
			ErrInvalidMarkers,
			startCount,
			endCount,
		)
	}

	start := strings.Index(existingBody, StartMarker)
	end := strings.Index(existingBody, EndMarker)
	if end < start {
		return "", fmt.Errorf("%w: end marker appears before start marker", ErrInvalidMarkers)
	}
	end += len(EndMarker)
	return existingBody[:start] + managedSection + existingBody[end:], nil
}

// Sync creates the parent issue when no issue number is persisted, otherwise
// it updates only the existing issue's managed section.
func Sync(
	ctx context.Context,
	client Client,
	owner, repo string,
	project *domain.Project,
	tasks []*domain.Task,
	review *domain.ManagerReview,
) (*SyncResult, error) {
	section, err := Render(project, tasks, review)
	if err != nil {
		return nil, err
	}
	if project.ParentIssueNumber == 0 {
		issue, err := client.CreateIssue(
			ctx,
			owner,
			repo,
			"[Madar] "+project.Name,
			section,
			nil,
		)
		if err != nil {
			return nil, fmt.Errorf("create parent project issue: %w", err)
		}
		if issue == nil || issue.Number <= 0 {
			return nil, errors.New("create parent project issue returned no issue")
		}
		return &SyncResult{Issue: issue, Created: true}, nil
	}

	existing, err := client.GetIssue(ctx, owner, repo, project.ParentIssueNumber)
	if err != nil {
		return nil, fmt.Errorf("get parent project issue: %w", err)
	}
	if existing == nil {
		return nil, errors.New("get parent project issue returned no issue")
	}
	body, err := MergeManagedSection(existing.Body, section)
	if err != nil {
		return nil, err
	}
	updated, err := client.UpdateIssueBody(
		ctx,
		owner,
		repo,
		project.ParentIssueNumber,
		body,
	)
	if err != nil {
		return nil, fmt.Errorf("update parent project issue: %w", err)
	}
	if updated == nil || updated.Number <= 0 {
		return nil, errors.New("update parent project issue returned no issue")
	}
	return &SyncResult{Issue: updated}, nil
}

func validateDashboard(
	project *domain.Project,
	tasks []*domain.Task,
	review *domain.ManagerReview,
) error {
	if err := project.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidDashboard, err)
	}
	if project.ID <= 0 {
		return fmt.Errorf("%w: project ID must be positive", ErrInvalidDashboard)
	}
	sequences := make(map[int]struct{}, len(tasks))
	for index, task := range tasks {
		if task == nil {
			return fmt.Errorf("%w: task %d is nil", ErrInvalidDashboard, index)
		}
		if err := task.Validate(); err != nil {
			return fmt.Errorf("%w: task %d: %v", ErrInvalidDashboard, index, err)
		}
		if task.ID <= 0 || task.Sequence <= 0 {
			return fmt.Errorf("%w: task %d must be persisted and ordered", ErrInvalidDashboard, index)
		}
		if task.ProjectID != project.ID {
			return fmt.Errorf("%w: task %d belongs to another project", ErrInvalidDashboard, task.ID)
		}
		if _, exists := sequences[task.Sequence]; exists {
			return fmt.Errorf("%w: duplicate task sequence %d", ErrInvalidDashboard, task.Sequence)
		}
		sequences[task.Sequence] = struct{}{}
	}
	if review != nil {
		if err := review.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidDashboard, err)
		}
		if review.ProjectID != project.ID {
			return fmt.Errorf("%w: manager review belongs to another project", ErrInvalidDashboard)
		}
	}
	return nil
}

func activeIssue(project *domain.Project, tasks []*domain.Task) string {
	if project.CurrentTaskID == nil {
		return "-"
	}
	for _, task := range tasks {
		if task.ID != *project.CurrentTaskID {
			continue
		}
		if task.IssueNumber > 0 {
			return fmt.Sprintf("#%d — %s", task.IssueNumber, inlineText(task.Title))
		}
		return fmt.Sprintf("task %d — %s", task.ID, inlineText(task.Title))
	}
	return fmt.Sprintf("task %d", *project.CurrentTaskID)
}

func progressEstimate(tasks []*domain.Task, review *domain.ManagerReview) int {
	if review != nil {
		return review.ProgressEstimate
	}
	if len(tasks) == 0 {
		return 0
	}
	completed := 0
	for _, task := range tasks {
		if task.Status == domain.TaskCompleted {
			completed++
		}
	}
	return completed * 100 / len(tasks)
}

func writeReleaseBlockers(output *bytes.Buffer, tasks []*domain.Task) {
	count := 0
	for _, task := range tasks {
		if !task.BlocksRelease ||
			task.Status == domain.TaskCompleted ||
			task.Status == domain.TaskCancelled {
			continue
		}
		fmt.Fprintf(
			output,
			"- %s %s — %s\n",
			issueReference(task.IssueNumber),
			inlineText(task.Title),
			task.Status,
		)
		count++
	}
	if count == 0 {
		fmt.Fprintln(output, "- None.")
	}
}

func writeRisks(
	output *bytes.Buffer,
	project *domain.Project,
	tasks []*domain.Task,
	review *domain.ManagerReview,
) {
	count := 0
	if project.Health != domain.HealthOnTrack &&
		project.Health != domain.HealthReadyForRelease {
		fmt.Fprintf(output, "- Project health is **%s**.\n", project.Health)
		count++
	}
	for _, task := range tasks {
		if task.Status == domain.TaskBlocked {
			fmt.Fprintf(output, "- %s %s is blocked.\n", issueReference(task.IssueNumber), inlineText(task.Title))
			count++
		}
		if strings.TrimSpace(task.DependencyState) != "" {
			fmt.Fprintf(
				output,
				"- %s %s dependency state: %s.\n",
				issueReference(task.IssueNumber),
				inlineText(task.Title),
				inlineText(task.DependencyState),
			)
			count++
		}
	}
	if review != nil && review.ArchitectureReviewRequired {
		fmt.Fprintln(output, "- Architecture review is required.")
		count++
	}
	if review != nil && review.HumanApprovalRequired {
		fmt.Fprintln(output, "- Human approval is required.")
		count++
	}
	if count == 0 {
		fmt.Fprintln(output, "- None recorded.")
	}
}

func writeDiscoveries(output *bytes.Buffer, review *domain.ManagerReview) {
	if review == nil {
		fmt.Fprintln(output, "- None recorded.")
		return
	}
	var decisions []json.RawMessage
	if err := json.Unmarshal(review.DiscoveryDecisions, &decisions); err != nil || len(decisions) == 0 {
		fmt.Fprintln(output, "- None recorded.")
		return
	}
	const displayedLimit = 5
	for index, decision := range decisions {
		if index == displayedLimit {
			fmt.Fprintf(output, "- …and %d more decision(s).\n", len(decisions)-displayedLimit)
			break
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, decision); err != nil {
			compact.Write(decision)
		}
		fmt.Fprintf(output, "- `%s`\n", inlineCode(compact.String()))
	}
}

func writeManagerDecision(output *bytes.Buffer, review *domain.ManagerReview) {
	if review == nil {
		fmt.Fprintln(output, "- No manager review recorded.")
		return
	}
	fmt.Fprintln(output, markdownBlock(review.OwnerUpdate))
	fmt.Fprintln(output)
	fmt.Fprintf(output, "- **Completed task decision:** %s\n", review.CompletedTaskDecision)
	if review.NextTaskID != nil || review.NextTaskIssueNumber > 0 {
		next := "-"
		if review.NextTaskID != nil {
			next = "task " + strconv.FormatInt(*review.NextTaskID, 10)
		}
		if review.NextTaskIssueNumber > 0 {
			next = "#" + strconv.Itoa(review.NextTaskIssueNumber)
		}
		fmt.Fprintf(output, "- **Next task:** %s — %s\n", next, inlineText(review.NextTaskReason))
	} else {
		fmt.Fprintln(output, "- **Next task:** -")
	}
	fmt.Fprintf(output, "- **Reviewed at:** %s\n", review.ReviewedAt.UTC().Format("2006-01-02T15:04:05Z07:00"))
}

func markdownBlock(value string) string {
	value = escapeManagedMarkers(value)
	return strings.TrimSpace(strings.ReplaceAll(value, "\r\n", "\n"))
}

func inlineText(value string) string {
	value = escapeManagedMarkers(value)
	value = strings.ReplaceAll(value, "\r\n", " ")
	value = strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
	return strings.TrimSpace(value)
}

func inlineOrDash(value string) string {
	value = inlineText(value)
	if value == "" {
		return "-"
	}
	return value
}

func tableCell(value string) string {
	value = escapeManagedMarkers(value)
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "|", "\\|")
	value = strings.NewReplacer("\r", "<br>", "\n", "<br>").Replace(value)
	return strings.TrimSpace(value)
}

func inlineCode(value string) string {
	return strings.ReplaceAll(inlineText(value), "`", "ˋ")
}

func escapeManagedMarkers(value string) string {
	return strings.NewReplacer(
		StartMarker, "&lt;!-- madar:project-dashboard:start --&gt;",
		EndMarker, "&lt;!-- madar:project-dashboard:end --&gt;",
	).Replace(value)
}

func issueReference(number int) string {
	if number == 0 {
		return "-"
	}
	return "#" + strconv.Itoa(number)
}

func displayBool(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
