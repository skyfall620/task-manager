package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/skyfall620/task-manager/internal/teams"
)

type repositoryFake struct {
	team               teams.Team
	members            map[uint64]teams.Member
	tasks              []Task
	history            []HistoryEntry
	listResult         []Task
	lastFilter         ListFilter
	listCalls          int
	createCalls        int
	historyCalls       int
	createErr          error
	historyErr         error
	active             bool
	before             func()
	updateCalls        int
	updateErr          error
	historyReadCalls   int
	lastHistoryFilter  HistoryFilter
	comments           []Comment
	createCommentCalls int
	commentReadCalls   int
	commentErr         error
	commentsErr        error
	lastCommentsFilter CommentsFilter
}

func newRepositoryFake() *repositoryFake {
	return &repositoryFake{
		team: teams.Team{ID: 10, CreatedBy: 1},
		members: map[uint64]teams.Member{
			1: {TeamID: 10, UserID: 1, Role: teams.RoleOwner},
			2: {TeamID: 10, UserID: 2, Role: teams.RoleAdmin},
			3: {TeamID: 10, UserID: 3, Role: teams.RoleMember},
			4: {TeamID: 10, UserID: 4, Role: teams.RoleMember},
		},
	}
}

func (f *repositoryFake) WithTeamTransaction(ctx context.Context, teamID uint64, operation func(TaskTransaction) error) error {
	if f.before != nil {
		f.before()
	}

	if teamID != f.team.ID {
		return ErrTeamNotFound
	}

	f.active = true
	defer func() { f.active = false }()
	savedTasks, savedHistory := slices.Clone(f.tasks), slices.Clone(f.history)
	savedComments := slices.Clone(f.comments)
	if err := operation(f); err != nil {
		f.tasks, f.history = savedTasks, savedHistory
		f.comments = savedComments
		return err
	}

	return nil
}

func (f *repositoryFake) requireTransaction() {
	if !f.active {
		panic("task operation outside transaction")
	}
}

func (f *repositoryFake) Team() teams.Team {
	f.requireTransaction()
	return f.team
}

func (f *repositoryFake) Member(ctx context.Context, userID uint64) (teams.Member, error) {
	f.requireTransaction()
	member, exists := f.members[userID]
	if !exists {
		return teams.Member{}, teams.ErrMemberNotFound
	}

	return member, nil
}

func (f *repositoryFake) Create(ctx context.Context, task Task) (Task, error) {
	f.requireTransaction()
	f.createCalls++
	if f.createErr != nil {
		return Task{}, f.createErr
	}

	task.ID = uint64(len(f.tasks) + 1)
	f.tasks = append(f.tasks, task)
	return task, nil
}

func (f *repositoryFake) AddHistory(ctx context.Context, entry HistoryEntry) error {
	f.requireTransaction()
	f.historyCalls++
	if f.historyErr != nil {
		return f.historyErr
	}

	entry.ID = uint64(len(f.history) + 1)
	f.history = append(f.history, entry)
	return nil
}

func (f *repositoryFake) List(ctx context.Context, filter ListFilter) ([]Task, error) {
	f.requireTransaction()
	f.listCalls++
	f.lastFilter = filter
	return f.listResult, nil
}

func TestCreateRolesAndInitialHistory(t *testing.T) {
	for _, actor := range []uint64{1, 2, 3} {
		for _, status := range []Status{"", StatusInProgress, StatusDone} {
			repository := newRepositoryFake()
			assignee := uint64(4)
			task, err := NewService(repository).Create(context.Background(), actor, CreateInput{
				TeamID: 10, Title: " Task ", Description: "Description", Status: status, AssigneeID: &assignee,
			})
			if err != nil {
				t.Fatal(err)
			}

			if task.ID == 0 || task.CreatedBy != actor || task.TeamID != 10 || task.Title != "Task" || task.Version != 1 {
				t.Fatalf("unexpected task: %+v", task)
			}

			if task.CreatedAt.IsZero() || !task.UpdatedAt.Equal(task.CreatedAt) {
				t.Fatal("server timestamps are invalid")
			}

			if status == "" && task.Status != StatusTodo {
				t.Fatal("missing default todo status")
			}

			if status == StatusDone {
				if task.ClosedAt == nil || !task.ClosedAt.Equal(task.CreatedAt) {
					t.Fatal("done task lacks server closing timestamp")
				}
			} else if task.ClosedAt != nil {
				t.Fatal("open task has closing timestamp")
			}

			if len(repository.tasks) != 1 || len(repository.history) != 1 {
				t.Fatal("task creation did not include initial history")
			}

			entry := repository.history[0]
			if entry.TaskID != task.ID || entry.ChangedBy != actor || !entry.CreatedAt.Equal(task.CreatedAt) {
				t.Fatal("initial history metadata differs from task")
			}

			assertInitialChanges(t, task, entry.Changes)
		}
	}
}

func assertInitialChanges(t *testing.T, task Task, changes Changes) {
	t.Helper()
	taskJSON, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}

	changesJSON, err := json.Marshal(changes)
	if err != nil {
		t.Fatal(err)
	}

	var fields map[string]json.RawMessage
	var recorded map[string]struct{ Old, New json.RawMessage }
	if err := json.Unmarshal(taskJSON, &fields); err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal(changesJSON, &recorded); err != nil {
		t.Fatal(err)
	}

	if len(fields) != len(recorded) {
		t.Fatal("initial history does not cover every task field")
	}

	for field, value := range fields {
		change, exists := recorded[field]
		if !exists || string(change.Old) != "null" || !bytes.Equal(change.New, value) {
			t.Errorf("invalid initial change for %s", field)
		}
	}
}

func TestCreateRejectsInvalidAccessAndInput(t *testing.T) {
	zero, outsider := uint64(0), uint64(99)
	for _, test := range []struct {
		name  string
		actor uint64
		input CreateInput
		want  error
	}{
		{"no authentication", 0, CreateInput{TeamID: 10, Title: "Task"}, ErrUnauthenticated},
		{"no team", 1, CreateInput{Title: "Task"}, ErrInvalidInput},
		{"unknown team", 1, CreateInput{TeamID: 99, Title: "Task"}, ErrTeamNotFound},
		{"outsider", 99, CreateInput{TeamID: 10, Title: "Task"}, ErrTeamNotFound},
		{"empty title", 1, CreateInput{TeamID: 10, Title: " "}, ErrInvalidInput},
		{"long title", 1, CreateInput{TeamID: 10, Title: strings.Repeat("я", 256)}, ErrInvalidInput},
		{"long description", 1, CreateInput{TeamID: 10, Title: "Task", Description: strings.Repeat("a", 65536)}, ErrInvalidInput},
		{"invalid status", 1, CreateInput{TeamID: 10, Title: "Task", Status: "closed"}, ErrInvalidInput},
		{"zero assignee", 1, CreateInput{TeamID: 10, Title: "Task", AssigneeID: &zero}, ErrInvalidAssignee},
		{"foreign assignee", 1, CreateInput{TeamID: 10, Title: "Task", AssigneeID: &outsider}, ErrInvalidAssignee},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := newRepositoryFake()
			_, err := NewService(repository).Create(context.Background(), test.actor, test.input)
			if !errors.Is(err, test.want) || repository.createCalls != 0 || repository.historyCalls != 0 {
				t.Fatalf("error=%v, create calls=%d, history calls=%d", err, repository.createCalls, repository.historyCalls)
			}
		})
	}
}

func TestCreateFailuresDoNotReturnTask(t *testing.T) {
	for _, failHistory := range []bool{false, true} {
		repository := newRepositoryFake()
		expected := errors.New("write failed")
		if failHistory {
			repository.historyErr = expected
		} else {
			repository.createErr = expected
		}

		task, err := NewService(repository).Create(context.Background(), 1, CreateInput{TeamID: 10, Title: "Task"})
		if !errors.Is(err, expected) || task.ID != 0 || len(repository.tasks) != 0 || len(repository.history) != 0 {
			t.Fatalf("failed transaction returned task: %+v, %v", task, err)
		}

		if !failHistory && repository.historyCalls != 0 {
			t.Fatal("history attempted after failed task insert")
		}
	}
}

func TestListPermissionsAndFilters(t *testing.T) {
	assignee := uint64(4)
	for _, actor := range []uint64{1, 2, 3} {
		repository := newRepositoryFake()
		service := NewService(repository)
		result, err := service.List(context.Background(), actor, ListFilter{TeamID: 10})
		if err != nil || result == nil || len(result) != 0 || repository.lastFilter.Limit != DefaultLimit {
			t.Fatalf("unexpected default list: %+v, %v", result, err)
		}

		filter := ListFilter{TeamID: 10, Status: StatusInProgress, AssigneeID: &assignee, Limit: 2, Offset: 3}
		if _, err := service.List(context.Background(), actor, filter); err != nil {
			t.Fatal(err)
		}

		if repository.lastFilter != filter {
			t.Fatal("filter changed before reaching repository")
		}
	}

	zero := uint64(0)
	for _, test := range []struct {
		actor  uint64
		filter ListFilter
		want   error
	}{
		{0, ListFilter{TeamID: 10}, ErrUnauthenticated},
		{99, ListFilter{TeamID: 10}, ErrTeamNotFound},
		{1, ListFilter{TeamID: 99}, ErrTeamNotFound},
		{1, ListFilter{}, ErrInvalidInput},
		{1, ListFilter{TeamID: 10, Status: "invalid"}, ErrInvalidInput},
		{1, ListFilter{TeamID: 10, Limit: -1}, ErrInvalidInput},
		{1, ListFilter{TeamID: 10, Limit: 101}, ErrInvalidInput},
		{1, ListFilter{TeamID: 10, Offset: -1}, ErrInvalidInput},
		{1, ListFilter{TeamID: 10, AssigneeID: &zero}, ErrInvalidInput},
	} {
		repository := newRepositoryFake()
		_, err := NewService(repository).List(context.Background(), test.actor, test.filter)
		if !errors.Is(err, test.want) || repository.listCalls != 0 {
			t.Errorf("error=%v, list calls=%d; want %v, 0", err, repository.listCalls, test.want)
		}
	}
}

func TestMembershipCheckedInsideTransaction(t *testing.T) {
	for _, operation := range []string{"create", "list"} {
		repository := newRepositoryFake()
		repository.before = func() { delete(repository.members, 3) }
		service := NewService(repository)
		var err error
		if operation == "create" {
			_, err = service.Create(context.Background(), 3, CreateInput{TeamID: 10, Title: "Task"})
		} else {
			_, err = service.List(context.Background(), 3, ListFilter{TeamID: 10})
		}

		if !errors.Is(err, ErrTeamNotFound) || repository.createCalls != 0 || repository.listCalls != 0 {
			t.Fatal("removed member retained task access")
		}
	}
}
