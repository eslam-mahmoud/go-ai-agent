package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

var ErrInvalidStatus = errors.New("invalid status message")

// StatusSender posts and edits the one live status message.
type StatusSender interface {
	SendStatus(ctx context.Context, text string) (chatID, messageID int64, err error)
	EditStatus(ctx context.Context, chatID, messageID int64, text string) error
}

// StatusStore remembers which message to edit, so a restart continues editing
// rather than posting a second status message.
type StatusStore interface {
	GetStatusMessage(projectID int64) (*store.StatusMessage, error)
	SaveStatusMessage(message store.StatusMessage) error
}

// Status is the state the live message shows.
type Status struct {
	Project     *domain.Project
	CurrentTask *domain.Task
	// Mode is the delivery mode currently running, if any.
	Mode string
	// Since is when the current task started; elapsed is derived from it.
	Since time.Time
	Now   time.Time
}

// StatusOutcome reports what the publisher did.
type StatusOutcome struct {
	Sent      bool
	Edited    bool
	Unchanged bool
	// Err is a delivery failure. As with notifications it is reported rather
	// than returned, so Telegram problems cannot block execution.
	Err error
}

type StatusPublisher struct {
	sender StatusSender
	store  StatusStore
}

func NewStatusPublisher(
	sender StatusSender,
	statusStore StatusStore,
) (*StatusPublisher, error) {
	if sender == nil {
		return nil, errors.New("status sender is required")
	}
	if statusStore == nil {
		return nil, errors.New("status store is required")
	}
	return &StatusPublisher{sender: sender, store: statusStore}, nil
}

// Publish brings the live status message up to date: sending it the first
// time, editing it afterwards, and doing nothing when nothing changed.
func (publisher *StatusPublisher) Publish(
	ctx context.Context,
	status Status,
) (*StatusOutcome, error) {
	if status.Project == nil || status.Project.ID <= 0 {
		return nil, fmt.Errorf("%w: a persisted project is required", ErrInvalidStatus)
	}
	text := RenderStatus(status)
	existing, err := publisher.store.GetStatusMessage(status.Project.ID)
	if err != nil {
		return nil, err
	}
	if existing != nil && existing.LastText == text {
		// An idle project should produce no Telegram traffic at all.
		return &StatusOutcome{Unchanged: true}, nil
	}
	if existing != nil {
		if err := publisher.sender.EditStatus(
			ctx, existing.ChatID, existing.MessageID, text,
		); err == nil {
			return publisher.remember(status.Project.ID, existing.ChatID,
				existing.MessageID, text, &StatusOutcome{Edited: true})
		}
		// The message was probably deleted; fall through and send a new one
		// rather than losing the status display permanently.
	}
	chatID, messageID, err := publisher.sender.SendStatus(ctx, text)
	if err != nil {
		return &StatusOutcome{Err: err}, nil
	}
	if chatID == 0 || messageID == 0 {
		// Telegram is not configured; there is nothing to remember.
		return &StatusOutcome{}, nil
	}
	return publisher.remember(status.Project.ID, chatID, messageID, text,
		&StatusOutcome{Sent: true})
}

func (publisher *StatusPublisher) remember(
	projectID, chatID, messageID int64,
	text string,
	outcome *StatusOutcome,
) (*StatusOutcome, error) {
	if err := publisher.store.SaveStatusMessage(store.StatusMessage{
		ProjectID: projectID,
		ChatID:    chatID,
		MessageID: messageID,
		LastText:  text,
	}); err != nil {
		return nil, err
	}
	return outcome, nil
}

// deliveryStages are the checklist rows, in the order work moves through them.
var deliveryStages = []struct {
	label    string
	statuses []domain.TaskStatus
}{
	{"Manager selected task", []domain.TaskStatus{domain.TaskSelected}},
	{"Planner", []domain.TaskStatus{domain.TaskPlanning, domain.TaskWaitingInput}},
	{"Developer", []domain.TaskStatus{domain.TaskDeveloping}},
	{"Review", []domain.TaskStatus{domain.TaskReviewing, domain.TaskFixing}},
	{"Verification", []domain.TaskStatus{domain.TaskVerifying, domain.TaskWaitingCI}},
	{"Manager review", []domain.TaskStatus{domain.TaskCompleted}},
}

// RenderStatus produces the plan's live status layout. It is deterministic so
// an unchanged project renders byte-identical text and needs no edit.
func RenderStatus(status Status) string {
	var text strings.Builder
	fmt.Fprintf(&text, "%s Madar · %s\n\n",
		healthIcon(status.Project.Health), inline(status.Project.Name))
	fmt.Fprintf(&text, "Health: %s\n", inline(string(status.Project.Health)))
	fmt.Fprintf(&text, "Issue: %s\n", activeIssue(status.CurrentTask))
	fmt.Fprintf(&text, "Mode: %s\n", modeLabel(status))
	fmt.Fprintf(&text, "Elapsed: %s\n\n", elapsed(status))
	for _, stage := range deliveryStages {
		fmt.Fprintf(&text, "%s %s\n", stageIcon(stage.statuses, status.CurrentTask), stage.label)
	}
	return strings.TrimRight(text.String(), "\n")
}

func healthIcon(health domain.ProjectHealth) string {
	switch health {
	case domain.HealthOnTrack, domain.HealthReadyForRelease:
		return "🟢"
	case domain.HealthBlocked, domain.HealthOffTrack:
		return "🔴"
	default:
		return "🟡"
	}
}

func activeIssue(task *domain.Task) string {
	if task == nil {
		return "—"
	}
	if task.IssueNumber > 0 {
		return fmt.Sprintf("#%d — %s", task.IssueNumber, inline(task.Title))
	}
	return inline(task.Title)
}

func modeLabel(status Status) string {
	if mode := inline(status.Mode); mode != "" {
		return mode
	}
	if status.CurrentTask != nil {
		return inline(string(status.CurrentTask.Status))
	}
	return "idle"
}

// elapsed renders whole minutes, which is the resolution the plan shows and
// keeps the text stable between renders a few seconds apart.
func elapsed(status Status) string {
	if status.CurrentTask == nil || status.Since.IsZero() {
		return "—"
	}
	now := status.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	duration := now.Sub(status.Since)
	if duration < 0 {
		return "—"
	}
	minutes := int(duration.Minutes())
	if minutes < 60 {
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%dh%02dm", minutes/60, minutes%60)
}

// stageIcon marks a stage done, running, or pending by comparing it with the
// task's position in the lifecycle, so the checklist cannot drift from state.
func stageIcon(statuses []domain.TaskStatus, task *domain.Task) string {
	if task == nil {
		return "⬜"
	}
	current := lifecyclePosition(task.Status)
	stage := lifecyclePosition(statuses[0])
	switch {
	case current < 0 || stage < 0:
		return "⬜"
	case containsStatus(statuses, task.Status):
		return "🟡"
	case stage < current:
		return "✅"
	default:
		return "⬜"
	}
}

func containsStatus(statuses []domain.TaskStatus, status domain.TaskStatus) bool {
	for _, candidate := range statuses {
		if candidate == status {
			return true
		}
	}
	return false
}

// lifecyclePosition orders the delivery statuses. Statuses outside the lane
// return -1 so they render as pending rather than claiming progress.
func lifecyclePosition(status domain.TaskStatus) int {
	order := []domain.TaskStatus{
		domain.TaskSelected,
		domain.TaskPlanning,
		domain.TaskWaitingInput,
		domain.TaskDeveloping,
		domain.TaskReviewing,
		domain.TaskFixing,
		domain.TaskVerifying,
		domain.TaskWaitingCI,
		domain.TaskCompleted,
	}
	for index, candidate := range order {
		if candidate == status {
			return index
		}
	}
	return -1
}

var _ StatusStore = (*store.Store)(nil)
