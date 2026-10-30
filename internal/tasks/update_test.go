package tasks

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/skyfall620/task-manager/internal/teams"
)

func (f *repositoryFake) TeamIDForTask(ctx context.Context, taskID uint64) (uint64, error) {
	for _, task := range f.tasks {
		if task.ID == taskID {
			return task.TeamID, nil
		}
	}

	return 0, ErrTaskNotFound
}

func (f *repositoryFake) LockTask(ctx context.Context, taskID uint64) (Task, error) {
	f.requireTransaction()
	for _, task := range f.tasks {
		if task.ID == taskID && task.TeamID == f.team.ID {
			return task, nil
		}
	}

	return Task{}, ErrTaskNotFound
}

func (f *repositoryFake) Update(ctx context.Context, task Task, expectedVersion uint64) error {
	f.requireTransaction()
	f.updateCalls++
	if f.updateErr != nil {
		return f.updateErr
	}

	for i, current := range f.tasks {
		if current.ID == task.ID && current.TeamID == f.team.ID && current.Version == expectedVersion {
			f.tasks[i] = task
			return nil
		}
	}

	return ErrVersionConflict
}

func (f *repositoryFake) History(ctx context.Context, taskID uint64, filter HistoryFilter) ([]HistoryEntry, error) {
	f.requireTransaction()
	f.historyReadCalls++
	f.lastHistoryFilter = filter
	if _, err := f.LockTask(ctx, taskID); err != nil {
		return nil, err
	}

	var result []HistoryEntry
	for _, entry := range f.history {
		if entry.TaskID == taskID {
			result = append(result, entry)
		}
	}

	if filter.Offset >= len(result) {
		return nil, nil
	}

	end := min(len(result), filter.Offset+filter.Limit)
	return result[filter.Offset:end], nil
}

func updateFixture(t *testing.T) (*repositoryFake, Task) {
	t.Helper()
	repository := newRepositoryFake()
	repository.members[5] = teams.Member{TeamID: 10, UserID: 5, Role: teams.RoleMember}
	assignee := uint64(4)
	task, err := NewService(repository).Create(context.Background(), 3, CreateInput{
		TeamID: 10, Title: "Task", Description: "Description", AssigneeID: &assignee,
	})
	if err != nil {
		t.Fatal(err)
	}

	return repository, task
}

func inputForTask(task Task) UpdateInput {
	return UpdateInput{
		Title: task.Title, Description: task.Description, Status: task.Status,
		AssigneeID: task.AssigneeID, Version: task.Version,
	}
}

func TestUpdateRoleModel(t *testing.T) {
	for _, test := range []struct {
		name   string
		actor  uint64
		change func(*UpdateInput)
		want   error
	}{
		{"owner title", 1, func(i *UpdateInput) { i.Title = "New" }, nil},
		{"admin description", 2, func(i *UpdateInput) { i.Description = "New" }, nil},
		{"creator title", 3, func(i *UpdateInput) { i.Title = "New" }, nil},
		{"creator unassigns", 3, func(i *UpdateInput) { i.AssigneeID = nil }, nil},
		{"creator reassigns", 3, func(i *UpdateInput) { id := uint64(5); i.AssigneeID = &id }, nil},
		{"assignee status", 4, func(i *UpdateInput) { i.Status = StatusDone }, nil},
		{"assignee title denied", 4, func(i *UpdateInput) { i.Title = "New" }, ErrForbidden},
		{"assignee description denied", 4, func(i *UpdateInput) { i.Description = "New" }, ErrForbidden},
		{"assignee unassign denied", 4, func(i *UpdateInput) { i.AssigneeID = nil }, ErrForbidden},
		{"unrelated member denied", 5, func(i *UpdateInput) { i.Status = StatusDone }, ErrForbidden},
		{"unrelated member no-op denied", 5, func(*UpdateInput) {}, ErrForbidden},
		{"outsider hidden", 99, func(i *UpdateInput) { i.Status = StatusDone }, ErrTaskNotFound},
		{"foreign assignee", 1, func(i *UpdateInput) { id := uint64(99); i.AssigneeID = &id }, ErrInvalidAssignee},
		{"assignee cannot assign outsider", 4, func(i *UpdateInput) { id := uint64(99); i.AssigneeID = &id }, ErrForbidden},
		{"stale version", 1, func(i *UpdateInput) { i.Version++; i.Title = "New" }, ErrVersionConflict},
		{"stale no-op", 3, func(i *UpdateInput) { i.Version++ }, ErrVersionConflict},
		{"zero version", 1, func(i *UpdateInput) { i.Version = 0 }, ErrInvalidInput},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, current := updateFixture(t)
			input := inputForTask(current)
			test.change(&input)
			updated, err := NewService(repository).Update(context.Background(), test.actor, current.ID, input)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v; want %v", err, test.want)
			}

			if test.want != nil {
				if repository.updateCalls != 0 || len(repository.history) != 1 || updated.ID != 0 {
					t.Fatal("denied update changed task or history")
				}

				return
			}

			if updated.Version != current.Version+1 || !updated.UpdatedAt.After(current.UpdatedAt) || len(repository.history) != 2 {
				t.Fatal("successful update did not advance task and history together")
			}

			if updated.ID != current.ID || updated.TeamID != current.TeamID || updated.CreatedBy != current.CreatedBy || !updated.CreatedAt.Equal(current.CreatedAt) {
				t.Fatal("immutable fields changed")
			}

			entry := repository.history[1]
			if entry.ChangedBy != test.actor || entry.TaskID != current.ID || !entry.CreatedAt.Equal(updated.UpdatedAt) {
				t.Fatal("incorrect history metadata")
			}

			if entry.Changes["version"].Old != current.Version || entry.Changes["version"].New != updated.Version {
				t.Fatal("history does not record actual old and new versions")
			}
		})
	}
}

func TestUpdateErrorPriority(t *testing.T) {
	for _, test := range []struct {
		name            string
		actor           uint64
		invalidAssignee bool
		want            error
	}{
		{"access denied before stale version", 5, false, ErrForbidden},
		{"access denied before stale version and invalid assignee", 5, true, ErrForbidden},
		{"stale version before forbidden field change", 4, false, ErrVersionConflict},
		{"stale version before invalid assignee", 3, true, ErrVersionConflict},
		{"stale version before forbidden field change and invalid assignee", 4, true, ErrVersionConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, current := updateFixture(t)
			input := inputForTask(current)
			input.Title = "Changed title"
			input.Version++
			if test.invalidAssignee {
				assignee := uint64(99)
				input.AssigneeID = &assignee
			}

			updated, err := NewService(repository).Update(context.Background(), test.actor, current.ID, input)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v; want %v", err, test.want)
			}

			if !reflect.DeepEqual(updated, Task{}) || repository.updateCalls != 0 || !reflect.DeepEqual(repository.tasks[0], current) || len(repository.history) != 1 {
				t.Fatal("rejected update changed task or history")
			}
		})
	}
}

func TestUpdateNoOpChecksRightsAndVersion(t *testing.T) {
	for _, actor := range []uint64{1, 2, 3, 4} {
		repository, current := updateFixture(t)
		input := inputForTask(current)
		assignee := *current.AssigneeID
		input.AssigneeID = &assignee
		updated, err := NewService(repository).Update(context.Background(), actor, current.ID, input)
		if err != nil || !reflect.DeepEqual(updated, current) || repository.updateCalls != 0 || len(repository.history) != 1 {
			t.Fatalf("no-op changed state: %+v, %v", updated, err)
		}
	}
}

func TestUpdateClosingDatesAndHistory(t *testing.T) {
	repository, task := updateFixture(t)
	service := NewService(repository)
	ctx := context.Background()
	input := inputForTask(task)
	input.Status = StatusDone
	closed, err := service.Update(ctx, 4, task.ID, input)
	if err != nil || closed.ClosedAt == nil || !closed.ClosedAt.Equal(closed.UpdatedAt) {
		t.Fatalf("closing failed: %+v, %v", closed, err)
	}

	if len(repository.history[1].Changes) != 4 {
		t.Fatal("status update should record status, closed_at, updated_at and version")
	}

	input = inputForTask(closed)
	input.Title = "Closed task renamed"
	renamed, err := service.Update(ctx, 3, task.ID, input)
	if err != nil || !sameTime(renamed.ClosedAt, closed.ClosedAt) {
		t.Fatal("renaming closed task changed closing date")
	}

	if _, exists := repository.history[2].Changes["closed_at"]; exists {
		t.Fatal("unchanged closing date recorded")
	}

	input = inputForTask(renamed)
	input.Status = StatusInProgress
	reopened, err := service.Update(ctx, 4, task.ID, input)
	if err != nil || reopened.ClosedAt != nil {
		t.Fatal("reopening did not clear closing date")
	}

	for _, entry := range repository.history[1:] {
		for _, field := range []string{"id", "team_id", "created_by", "created_at", "description", "assignee_id"} {
			if _, exists := entry.Changes[field]; exists {
				t.Errorf("unchanged field %s recorded", field)
			}
		}
	}
}

func TestUpdateRollsBackOnWriteErrors(t *testing.T) {
	for _, failure := range []string{"update", "history", "conditional conflict"} {
		t.Run(failure, func(t *testing.T) {
			repository, current := updateFixture(t)
			expected := errors.New("write failed")
			switch failure {
			case "update":
				repository.updateErr = expected
			case "history":
				repository.historyErr = expected
			case "conditional conflict":
				expected = ErrVersionConflict
				repository.updateErr = expected
			}

			input := inputForTask(current)
			input.Title = "Must roll back"
			updated, err := NewService(repository).Update(context.Background(), 1, current.ID, input)
			if !errors.Is(err, expected) || updated.ID != 0 || !reflect.DeepEqual(repository.tasks[0], current) || len(repository.history) != 1 {
				t.Fatalf("failed update persisted state: %+v, %v", updated, err)
			}
		})
	}
}

func TestUpdateAndHistoryUseCurrentMembership(t *testing.T) {
	for _, operation := range []string{"update", "history"} {
		repository, task := updateFixture(t)
		repository.before = func() { delete(repository.members, 3) }
		service := NewService(repository)
		var err error
		if operation == "update" {
			_, err = service.Update(context.Background(), 3, task.ID, inputForTask(task))
		} else {
			_, err = service.History(context.Background(), 3, task.ID, HistoryFilter{})
		}

		if !errors.Is(err, ErrTaskNotFound) || repository.updateCalls != 0 || repository.historyReadCalls != 0 {
			t.Fatal("removed member retained task access")
		}
	}

	repository, task := updateFixture(t)
	repository.before = func() {
		member := repository.members[2]
		member.Role = teams.RoleMember
		repository.members[2] = member
	}
	input := inputForTask(task)
	input.Title = "Denied"
	if _, err := NewService(repository).Update(context.Background(), 2, task.ID, input); !errors.Is(err, ErrForbidden) {
		t.Fatalf("demoted admin retained permission: %v", err)
	}
}

func TestHistoryRolesAndPagination(t *testing.T) {
	for _, actor := range []uint64{1, 2, 3, 4, 5} {
		repository, task := updateFixture(t)
		service := NewService(repository)
		input := inputForTask(task)
		input.Title = "New"
		if _, err := service.Update(context.Background(), 1, task.ID, input); err != nil {
			t.Fatal(err)
		}

		entries, err := service.History(context.Background(), actor, task.ID, HistoryFilter{Limit: 1, Offset: 1})
		if err != nil || len(entries) != 1 || entries[0].ID != 2 || repository.lastHistoryFilter.Limit != 1 {
			t.Fatalf("unexpected history page: %+v, %v", entries, err)
		}

		entries, err = service.History(context.Background(), actor, task.ID, HistoryFilter{Offset: 99})
		if err != nil || entries == nil || len(entries) != 0 || repository.lastHistoryFilter.Limit != DefaultLimit {
			t.Fatal("unexpected empty history page")
		}
	}

	repository, task := updateFixture(t)
	for _, actor := range []uint64{0, 99} {
		_, err := NewService(repository).History(context.Background(), actor, task.ID, HistoryFilter{})
		want := ErrTaskNotFound
		if actor == 0 {
			want = ErrUnauthenticated
		}

		if !errors.Is(err, want) {
			t.Fatalf("unexpected history access error: %v", err)
		}
	}

	for _, filter := range []HistoryFilter{{Limit: -1}, {Limit: 101}, {Offset: -1}} {
		if _, err := NewService(repository).History(context.Background(), 1, task.ID, filter); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid history pagination accepted: %v", err)
		}
	}
}
