package tasks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/skyfall620/task-manager/internal/teams"
)

// Update checks access and the expected version against the locked task, then
// commits changes and their history atomically. Stale versions return ErrVersionConflict.
// A no-op still checks access and version but adds no history and does not advance the version.
func (s *Service) Update(ctx context.Context, actorID, taskID uint64, input UpdateInput) (Task, error) {
	if err := validateTaskActor(actorID, taskID); err != nil {
		return Task{}, err
	}

	input.Title = strings.TrimSpace(input.Title)
	if err := validateUpdate(input); err != nil {
		return Task{}, err
	}

	var result Task
	changed := false
	err := s.withTaskTeam(ctx, taskID, func(tx TaskTransaction) error {
		var err error
		result, changed, err = updateTaskInTransaction(ctx, tx, actorID, taskID, input)
		return err
	})
	if err != nil {
		return Task{}, fmt.Errorf("update task: %w", err)
	}

	if changed {
		s.invalidateTaskLists(ctx, result.TeamID)
	}

	return result, nil
}

func updateTaskInTransaction(ctx context.Context, tx TaskTransaction, actorID, taskID uint64, input UpdateInput) (Task, bool, error) {
	actor, err := authorizedMember(ctx, tx, actorID)
	if err != nil {
		return Task{}, false, err
	}

	// Lock the requested assignee before the task, following the membership
	// lock order. Report a missing assignee only after task authorization.
	var assigneeErr error
	if input.AssigneeID != nil && *input.AssigneeID != actorID {
		_, assigneeErr = tx.Member(ctx, *input.AssigneeID)
		if assigneeErr != nil && !errors.Is(assigneeErr, teams.ErrMemberNotFound) {
			return Task{}, false, assigneeErr
		}
	}

	current, err := tx.LockTask(ctx, taskID)
	if err != nil {
		return Task{}, false, err
	}

	fullAccess := actor.Role == teams.RoleOwner || actor.Role == teams.RoleAdmin || current.CreatedBy == actorID
	if !fullAccess && (current.AssigneeID == nil || *current.AssigneeID != actorID) {
		return Task{}, false, ErrForbidden
	}

	if input.Version != current.Version {
		return Task{}, false, ErrVersionConflict
	}

	if !fullAccess && (input.Title != current.Title || input.Description != current.Description || !sameAssignee(input.AssigneeID, current.AssigneeID)) {
		return Task{}, false, ErrForbidden
	}

	if assigneeErr != nil {
		return Task{}, false, ErrInvalidAssignee
	}

	updated, changes := changedTask(current, input)
	if len(changes) == 0 {
		return current, false, nil
	}

	if current.Version == ^uint64(0) {
		return Task{}, false, ErrVersionConflict
	}

	if err := tx.Update(ctx, updated, current.Version); err != nil {
		return Task{}, false, err
	}

	if err := tx.AddHistory(ctx, HistoryEntry{
		TaskID: taskID, ChangedBy: actorID, Changes: changes, CreatedAt: updated.UpdatedAt,
	}); err != nil {
		return Task{}, false, err
	}

	return updated, true, nil
}

func (s *Service) History(ctx context.Context, actorID, taskID uint64, filter HistoryFilter) ([]HistoryEntry, error) {
	if err := validateTaskActor(actorID, taskID); err != nil {
		return nil, err
	}

	if filter.Limit == 0 {
		filter.Limit = DefaultLimit
	}

	if filter.Limit < 1 || filter.Limit > MaxLimit || filter.Offset < 0 {
		return nil, fmt.Errorf("%w: limit must be 1 to 100 and offset must be nonnegative", ErrInvalidInput)
	}

	var result []HistoryEntry
	err := s.withTaskTeam(ctx, taskID, func(tx TaskTransaction) error {
		if err := authorizeMember(ctx, tx, actorID); err != nil {
			return err
		}

		var err error
		result, err = tx.History(ctx, taskID, filter)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read task history: %w", err)
	}

	if result == nil {
		result = []HistoryEntry{}
	}

	return result, nil
}

func (s *Service) withTaskTeam(ctx context.Context, taskID uint64, operation func(TaskTransaction) error) error {
	// team_id is immutable. This lookup selects the team lock without exposing task data.
	teamID, err := s.repository.TeamIDForTask(ctx, taskID)
	if err != nil {
		return err
	}

	err = s.repository.WithTeamTransaction(ctx, teamID, operation)
	if errors.Is(err, ErrTeamNotFound) {
		return ErrTaskNotFound
	}

	return err
}

func validateTaskActor(actorID, taskID uint64) error {
	if actorID == 0 {
		return ErrUnauthenticated
	}

	if taskID == 0 {
		return fmt.Errorf("%w: positive task ID required", ErrInvalidInput)
	}

	return nil
}

func validateUpdate(input UpdateInput) error {
	if !utf8.ValidString(input.Title) || input.Title == "" || utf8.RuneCountInString(input.Title) > 255 {
		return fmt.Errorf("%w: title must contain 1 to 255 characters", ErrInvalidInput)
	}

	if !utf8.ValidString(input.Description) || len(input.Description) > 65535 {
		return fmt.Errorf("%w: description must be valid UTF-8 and at most 65535 bytes", ErrInvalidInput)
	}

	if !validStatus(input.Status) || input.Version == 0 {
		return fmt.Errorf("%w: valid status and positive version required", ErrInvalidInput)
	}

	if input.AssigneeID != nil && *input.AssigneeID == 0 {
		return ErrInvalidAssignee
	}

	return nil
}

func sameAssignee(a, b *uint64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func changedTask(current Task, input UpdateInput) (Task, Changes) {
	updated := current
	changes := Changes{}
	if input.Title != current.Title {
		changes["title"] = Change{Old: current.Title, New: input.Title}
		updated.Title = input.Title
	}

	if input.Description != current.Description {
		changes["description"] = Change{Old: current.Description, New: input.Description}
		updated.Description = input.Description
	}

	if input.Status != current.Status {
		changes["status"] = Change{Old: current.Status, New: input.Status}
		updated.Status = input.Status
	}

	if !sameAssignee(input.AssigneeID, current.AssigneeID) {
		changes["assignee_id"] = Change{Old: current.AssigneeID, New: input.AssigneeID}
		updated.AssigneeID = input.AssigneeID
	}

	if len(changes) == 0 {
		return current, changes
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	// Preserve timestamp ordering at MySQL's microsecond precision even if the clock moves backwards.
	if !now.After(current.UpdatedAt) {
		now = current.UpdatedAt.Add(time.Microsecond)
	}

	if input.Status != current.Status {
		if input.Status == StatusDone {
			updated.ClosedAt = &now
		} else {
			updated.ClosedAt = nil
		}

		if !sameTime(current.ClosedAt, updated.ClosedAt) {
			changes["closed_at"] = Change{Old: current.ClosedAt, New: updated.ClosedAt}
		}
	}

	updated.UpdatedAt = now
	updated.Version++
	changes["updated_at"] = Change{Old: current.UpdatedAt, New: updated.UpdatedAt}
	changes["version"] = Change{Old: current.Version, New: updated.Version}
	return updated, changes
}

func sameTime(a, b *time.Time) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && a.Equal(*b))
}
