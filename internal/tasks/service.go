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

// TaskTransaction is valid only inside WithTeamTransaction. Its team and
// membership locks are held until the repository commits or rolls back.
type TaskTransaction interface {
	Team() teams.Team
	Member(context.Context, uint64) (teams.Member, error)
	Create(context.Context, Task) (Task, error)
	AddHistory(context.Context, HistoryEntry) error
	List(context.Context, ListFilter) ([]Task, error)
	LockTask(context.Context, uint64) (Task, error)
	Update(context.Context, Task, uint64) error
	History(context.Context, uint64, HistoryFilter) ([]HistoryEntry, error)
	CreateComment(context.Context, Comment) (Comment, error)
	Comments(context.Context, uint64, CommentsFilter) ([]Comment, error)
}

type TaskRepository interface {
	WithTeamTransaction(context.Context, uint64, func(TaskTransaction) error) error
	TeamIDForTask(context.Context, uint64) (uint64, error)
}

type Service struct {
	repository TaskRepository
	cache      ListCache
}

func NewService(repository TaskRepository) *Service {
	return &Service{repository: repository}
}

func (s *Service) Create(ctx context.Context, actorID uint64, input CreateInput) (Task, error) {
	if err := validateActorAndTeam(actorID, input.TeamID); err != nil {
		return Task{}, err
	}

	input.Title = strings.TrimSpace(input.Title)
	if !utf8.ValidString(input.Title) || input.Title == "" || utf8.RuneCountInString(input.Title) > 255 {
		return Task{}, fmt.Errorf("%w: title must contain 1 to 255 characters", ErrInvalidInput)
	}

	if !utf8.ValidString(input.Description) || len(input.Description) > 65535 {
		return Task{}, fmt.Errorf("%w: description must be valid UTF-8 and at most 65535 bytes", ErrInvalidInput)
	}

	if input.Status == "" {
		input.Status = StatusTodo
	}

	if !validStatus(input.Status) {
		return Task{}, fmt.Errorf("%w: status must be todo, in_progress or done", ErrInvalidInput)
	}

	if input.AssigneeID != nil && *input.AssigneeID == 0 {
		return Task{}, ErrInvalidAssignee
	}

	var task Task
	err := s.repository.WithTeamTransaction(ctx, input.TeamID, func(tx TaskTransaction) error {
		var err error
		task, err = createTaskInTransaction(ctx, tx, actorID, input)
		return err
	})
	if err != nil {
		return Task{}, fmt.Errorf("create task: %w", err)
	}

	s.invalidateTaskLists(ctx, task.TeamID)
	return task, nil
}

func createTaskInTransaction(ctx context.Context, tx TaskTransaction, actorID uint64, input CreateInput) (Task, error) {
	if err := authorizeMember(ctx, tx, actorID); err != nil {
		return Task{}, err
	}

	if input.AssigneeID != nil {
		if _, err := tx.Member(ctx, *input.AssigneeID); err != nil {
			if errors.Is(err, teams.ErrMemberNotFound) {
				return Task{}, ErrInvalidAssignee
			}

			return Task{}, err
		}
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	task := Task{
		TeamID: input.TeamID, Title: input.Title, Description: input.Description,
		Status: input.Status, CreatedBy: actorID, AssigneeID: input.AssigneeID,
		CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	if task.Status == StatusDone {
		task.ClosedAt = &now
	}

	task, err := tx.Create(ctx, task)
	if err != nil {
		return Task{}, err
	}

	if err := tx.AddHistory(ctx, HistoryEntry{
		TaskID: task.ID, ChangedBy: actorID, Changes: initialChanges(task), CreatedAt: task.CreatedAt,
	}); err != nil {
		return Task{}, err
	}

	return task, nil
}

func (s *Service) List(ctx context.Context, actorID uint64, filter ListFilter) ([]Task, error) {
	if err := validateActorAndTeam(actorID, filter.TeamID); err != nil {
		return nil, err
	}

	if filter.Status != "" && !validStatus(filter.Status) {
		return nil, fmt.Errorf("%w: invalid status filter", ErrInvalidInput)
	}

	if filter.AssigneeID != nil && *filter.AssigneeID == 0 {
		return nil, fmt.Errorf("%w: assignee_id must be positive", ErrInvalidInput)
	}

	if filter.Limit == 0 {
		filter.Limit = DefaultLimit
	}

	if filter.Limit < 1 || filter.Limit > MaxLimit || filter.Offset < 0 {
		return nil, fmt.Errorf("%w: limit must be 1 to 100 and offset must be nonnegative", ErrInvalidInput)
	}

	entry, err := s.loadCachedList(ctx, actorID, filter)
	if err != nil {
		return nil, fmt.Errorf("authorize task list: %w", err)
	}

	if entry.Found {
		return entry.Tasks, nil
	}

	var result []Task
	err = s.repository.WithTeamTransaction(ctx, filter.TeamID, func(tx TaskTransaction) error {
		if err := authorizeMember(ctx, tx, actorID); err != nil {
			return err
		}

		var err error
		result, err = tx.List(ctx, filter)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}

	if result == nil {
		result = []Task{}
	}

	s.storeCachedList(ctx, filter.TeamID, entry, result)
	return result, nil
}

func authorizeMember(ctx context.Context, tx TaskTransaction, actorID uint64) error {
	_, err := authorizedMember(ctx, tx, actorID)
	return err
}

func authorizedMember(ctx context.Context, tx TaskTransaction, actorID uint64) (teams.Member, error) {
	member, err := tx.Member(ctx, actorID)
	if errors.Is(err, teams.ErrMemberNotFound) {
		return teams.Member{}, ErrTeamNotFound
	}

	if err != nil {
		return teams.Member{}, err
	}

	switch member.Role {
	case teams.RoleAdmin, teams.RoleMember:
		return member, nil
	case teams.RoleOwner:
		if member.UserID == tx.Team().CreatedBy {
			return member, nil
		}
	}

	return teams.Member{}, ErrForbidden
}

func validateActorAndTeam(actorID, teamID uint64) error {
	if actorID == 0 {
		return ErrUnauthenticated
	}

	if teamID == 0 {
		return fmt.Errorf("%w: positive team_id required", ErrInvalidInput)
	}

	return nil
}

func validStatus(status Status) bool {
	return status == StatusTodo || status == StatusInProgress || status == StatusDone
}

func initialChanges(task Task) Changes {
	return Changes{
		"id":          {New: task.ID},
		"team_id":     {New: task.TeamID},
		"title":       {New: task.Title},
		"description": {New: task.Description},
		"status":      {New: task.Status},
		"created_by":  {New: task.CreatedBy},
		"assignee_id": {New: task.AssigneeID},
		"created_at":  {New: task.CreatedAt},
		"updated_at":  {New: task.UpdatedAt},
		"closed_at":   {New: task.ClosedAt},
		"version":     {New: task.Version},
	}
}
