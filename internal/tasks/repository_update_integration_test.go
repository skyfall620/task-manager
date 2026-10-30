package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func runUpdateIntegrationTests(t *testing.T, db *sql.DB) {
	t.Helper()
	t.Run("concurrent-updates", func(t *testing.T) { testConcurrentUpdates(t, db) })
	t.Run("update-history-failure-rolls-back", func(t *testing.T) { testUpdateHistoryRollback(t, db) })
	t.Run("update-history-old-new", func(t *testing.T) { testUpdateHistoryOldNew(t, db) })
	t.Run("conditional-update-and-team-scope", func(t *testing.T) { testConditionalUpdate(t, db) })
	t.Run("history-pagination-and-no-op", func(t *testing.T) { testHistoryAndNoOp(t, db) })
}

func persistedTask(t *testing.T, ctx context.Context, db *sql.DB, taskID uint64) Task {
	t.Helper()
	task, err := scanTask(db.QueryRowContext(ctx, `
		SELECT id, team_id, title, description, status, created_by, assignee_id,
		       created_at, updated_at, closed_at, version FROM tasks WHERE id = ?`, taskID))
	if err != nil {
		t.Fatal(err)
	}

	return task
}

func createIntegrationTask(t *testing.T, ctx context.Context, service *Service, fixture *integrationFixture) Task {
	t.Helper()
	task, err := service.Create(ctx, fixture.owner, CreateInput{
		TeamID: fixture.team, Title: "Original", Description: "Description", AssigneeID: &fixture.assignee,
	})
	if err != nil {
		t.Fatal(err)
	}

	return task
}

func testConcurrentUpdates(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, fixture := taskFixture(t, db)
	service := NewService(NewRepository(db))
	current := createIntegrationTask(t, ctx, service, fixture)
	type outcome struct {
		task Task
		err  error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	for _, status := range []Status{StatusDone, StatusInProgress} {
		input := inputForTask(current)
		input.Title = string(status)
		input.Description = "Written by " + string(status)
		input.Status = status
		go func() {
			<-start
			updated, err := service.Update(ctx, fixture.owner, current.ID, input)
			results <- outcome{task: updated, err: err}
		}()
	}

	close(start)
	var winner Task
	successes, conflicts := 0, 0
	for range 2 {
		select {
		case result := <-results:
			switch {
			case result.err == nil:
				successes++
				winner = result.task
			case errors.Is(result.err, ErrVersionConflict):
				conflicts++
			default:
				t.Fatalf("unexpected concurrent update error: %v", result.err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}

	if successes != 1 || conflicts != 1 || winner.Version != 2 {
		t.Fatalf("successes=%d, conflicts=%d, version=%d", successes, conflicts, winner.Version)
	}

	stored := persistedTask(t, ctx, db, current.ID)
	if !reflect.DeepEqual(stored, winner) || stored.Description != "Written by "+string(stored.Status) || stored.Title != string(stored.Status) {
		t.Fatal("concurrent updates lost or mixed field changes")
	}

	entries, err := service.History(ctx, fixture.owner, current.ID, HistoryFilter{})
	if err != nil || len(entries) != 2 {
		t.Fatalf("unexpected concurrent history: length=%d, error=%v", len(entries), err)
	}

	if entries[1].Changes["title"].Old != current.Title || entries[1].Changes["title"].New != winner.Title ||
		entries[1].Changes["version"].Old != json.Number("1") || entries[1].Changes["version"].New != json.Number("2") {
		t.Fatal("history contains changes from a losing or stale update")
	}

	retry := inputForTask(stored)
	retry.Title = "Retried with current version"
	updated, err := service.Update(ctx, fixture.owner, stored.ID, retry)
	if err != nil || updated.Version != 3 {
		t.Fatalf("retry with current version failed: %+v, %v", updated, err)
	}
}

type failingHistoryRepository struct {
	TaskRepository
	changedBy uint64
}

func (r failingHistoryRepository) WithTeamTransaction(ctx context.Context, teamID uint64, operation func(TaskTransaction) error) error {
	return r.TaskRepository.WithTeamTransaction(ctx, teamID, func(tx TaskTransaction) error {
		return operation(failingHistoryTransaction{TaskTransaction: tx, changedBy: r.changedBy})
	})
}

type failingHistoryTransaction struct {
	TaskTransaction
	changedBy uint64
}

func (t failingHistoryTransaction) AddHistory(ctx context.Context, entry HistoryEntry) error {
	// Cause an actual FK error after the service has updated the task row.
	entry.ChangedBy = t.changedBy
	return t.TaskTransaction.AddHistory(ctx, entry)
}

func testUpdateHistoryRollback(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, fixture := taskFixture(t, db)
	repository := NewRepository(db)
	service := NewService(repository)
	current := createIntegrationTask(t, ctx, service, fixture)
	before := persistedTask(t, ctx, db, current.ID)
	failing := NewService(failingHistoryRepository{TaskRepository: repository, changedBy: fixture.missingUser})
	input := inputForTask(current)
	input.Title = "Must roll back"
	input.Status = StatusDone
	input.AssigneeID = nil
	updated, err := failing.Update(ctx, fixture.owner, current.ID, input)
	var mysqlErr *mysql.MySQLError
	if updated.ID != 0 || !errors.As(err, &mysqlErr) || mysqlErr.Number != 1452 {
		t.Fatalf("expected history FK error, got %v", err)
	}

	after := persistedTask(t, ctx, db, current.ID)
	if !reflect.DeepEqual(after, before) {
		t.Fatal("history failure left updated fields, dates or version committed")
	}

	entries, err := service.History(ctx, fixture.owner, current.ID, HistoryFilter{})
	if err != nil || len(entries) != 1 {
		t.Fatalf("failed update left history: length=%d, error=%v", len(entries), err)
	}
}

func testUpdateHistoryOldNew(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, fixture := taskFixture(t, db)
	service := NewService(NewRepository(db))
	created, err := service.Create(ctx, fixture.assignee, CreateInput{
		TeamID: fixture.team, Title: "Original title", Description: "Original description", Status: StatusTodo,
	})
	if err != nil {
		t.Fatal(err)
	}

	before := persistedTask(t, ctx, db, created.ID)
	steps := []struct {
		actor uint64
		input UpdateInput
	}{
		{
			actor: fixture.owner,
			input: UpdateInput{
				Title: "Closed title", Description: "Changed description", Status: StatusDone,
				AssigneeID: &fixture.assignee, Version: 1,
			},
		},
		{
			actor: fixture.assignee,
			input: UpdateInput{
				Title: "Reopened title", Description: "", Status: StatusInProgress,
				AssigneeID: nil, Version: 2,
			},
		},
	}
	for i, step := range steps {
		updated, err := service.Update(ctx, step.actor, created.ID, step.input)
		if err != nil {
			t.Fatal(err)
		}

		// DATETIME(6) stores microseconds; require the returned timestamp to match that precision.
		updatedAt := updated.UpdatedAt.UTC().Truncate(time.Microsecond)
		if !updated.UpdatedAt.Equal(updatedAt) || !updatedAt.After(before.UpdatedAt) {
			t.Fatalf("update %d has an invalid timestamp: %v", i+1, updated.UpdatedAt)
		}

		var closedAt *time.Time
		if i == 0 {
			closedAt = &updatedAt
		}

		// Build expectations directly from the request and the previous persisted state.
		wantTask := before
		wantTask.Title = step.input.Title
		wantTask.Description = step.input.Description
		wantTask.Status = step.input.Status
		wantTask.AssigneeID = step.input.AssigneeID
		wantTask.ClosedAt = closedAt
		wantTask.UpdatedAt = updatedAt
		wantTask.Version = before.Version + 1
		stored := persistedTask(t, ctx, db, created.ID)
		if !reflect.DeepEqual(updated, wantTask) || !reflect.DeepEqual(stored, wantTask) {
			t.Fatalf("update %d task mismatch: returned=%+v, persisted=%+v, want=%+v", i+1, updated, stored, wantTask)
		}

		entries, err := service.History(ctx, fixture.owner, created.ID, HistoryFilter{})
		if err != nil || len(entries) != i+2 {
			t.Fatalf("update %d history: length=%d, error=%v", i+1, len(entries), err)
		}

		entry := entries[i+1]
		if entry.ID == 0 || entry.TaskID != created.ID || entry.ChangedBy != step.actor || !entry.CreatedAt.Equal(updatedAt) {
			t.Fatalf("update %d has incorrect history metadata: %+v", i+1, entry)
		}

		wantChanges := Changes{
			"title":       {Old: before.Title, New: step.input.Title},
			"description": {Old: before.Description, New: step.input.Description},
			"status":      {Old: before.Status, New: step.input.Status},
			"assignee_id": {Old: before.AssigneeID, New: step.input.AssigneeID},
			"closed_at":   {Old: before.ClosedAt, New: closedAt},
			"updated_at":  {Old: before.UpdatedAt, New: updatedAt},
			"version":     {Old: before.Version, New: before.Version + 1},
		}
		// JSON comparison preserves exact integers, timestamps and explicit null values.
		wantJSON, err := json.Marshal(wantChanges)
		if err != nil {
			t.Fatal(err)
		}

		gotJSON, err := json.Marshal(entry.Changes)
		if err != nil {
			t.Fatal(err)
		}

		if string(gotJSON) != string(wantJSON) {
			t.Fatalf("update %d history changes = %s; want %s", i+1, gotJSON, wantJSON)
		}

		before = stored
	}
}

func testConditionalUpdate(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, fixture := taskFixture(t, db)
	repository := NewRepository(db)
	service := NewService(repository)
	current := createIntegrationTask(t, ctx, service, fixture)
	err := repository.WithTeamTransaction(ctx, fixture.team, func(tx TaskTransaction) error {
		task, err := tx.LockTask(ctx, current.ID)
		if err != nil {
			return err
		}

		task.Title = "Wrong SQL version"
		return tx.Update(ctx, task, current.Version+1)
	})
	if !errors.Is(err, ErrVersionConflict) || !reflect.DeepEqual(persistedTask(t, ctx, db, current.ID), current) {
		t.Fatalf("SQL update ignored expected version: %v", err)
	}

	other, err := service.Create(ctx, fixture.owner, CreateInput{TeamID: fixture.otherTeam, Title: "Other team"})
	if err != nil {
		t.Fatal(err)
	}

	for _, operation := range []string{"lock", "history", "update"} {
		err := repository.WithTeamTransaction(ctx, fixture.team, func(tx TaskTransaction) error {
			switch operation {
			case "lock":
				_, err := tx.LockTask(ctx, other.ID)
				return err
			case "history":
				_, err := tx.History(ctx, other.ID, HistoryFilter{Limit: DefaultLimit})
				return err
			default:
				other.Title = "Forbidden cross-team change"
				return tx.Update(ctx, other, other.Version)
			}
		})
		want := ErrTaskNotFound
		if operation == "update" {
			want = ErrVersionConflict
		}

		if !errors.Is(err, want) {
			t.Fatalf("cross-team %s allowed: %v", operation, err)
		}
	}
}

func testHistoryAndNoOp(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, fixture := taskFixture(t, db)
	service := NewService(NewRepository(db))
	current := createIntegrationTask(t, ctx, service, fixture)
	input := inputForTask(current)
	input.Title = "Renamed"
	current, err := service.Update(ctx, fixture.owner, current.ID, input)
	if err != nil {
		t.Fatal(err)
	}

	input = inputForTask(current)
	input.Status = StatusDone
	closed, err := service.Update(ctx, fixture.assignee, current.ID, input)
	if err != nil || closed.ClosedAt == nil {
		t.Fatalf("assignee could not close task: %v", err)
	}

	input = inputForTask(closed)
	input.Title = "Closed task renamed"
	current, err = service.Update(ctx, fixture.owner, closed.ID, input)
	if err != nil || !sameTime(current.ClosedAt, closed.ClosedAt) {
		t.Fatalf("closing timestamp changed on rename: %v", err)
	}

	before := persistedTask(t, ctx, db, current.ID)
	unchanged, err := service.Update(ctx, fixture.owner, current.ID, inputForTask(current))
	if err != nil || !reflect.DeepEqual(unchanged, before) || !reflect.DeepEqual(persistedTask(t, ctx, db, current.ID), before) {
		t.Fatalf("no-op changed persisted task: %v", err)
	}

	input = inputForTask(current)
	input.Version--
	if _, err := service.Update(ctx, fixture.owner, current.ID, input); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale no-op accepted: %v", err)
	}

	input = inputForTask(current)
	input.Status = StatusTodo
	input.AssigneeID = nil
	current, err = service.Update(ctx, fixture.owner, current.ID, input)
	if err != nil || current.ClosedAt != nil || current.AssigneeID != nil || current.Version != 5 {
		t.Fatalf("reopen or unassignment failed: %+v, %v", current, err)
	}

	entries, err := service.History(ctx, fixture.assignee, current.ID, HistoryFilter{})
	if err != nil || len(entries) != 5 {
		t.Fatalf("unexpected history count: %d, %v", len(entries), err)
	}

	for i := 1; i < len(entries); i++ {
		if entries[i].CreatedAt.Before(entries[i-1].CreatedAt) || entries[i].ID <= entries[i-1].ID {
			t.Fatal("history is not chronological")
		}
	}

	page, err := service.History(ctx, fixture.assignee, current.ID, HistoryFilter{Limit: 1, Offset: 1})
	if err != nil || len(page) != 1 || page[0].ID != entries[1].ID {
		t.Fatalf("wrong history page: %+v, %v", page, err)
	}

	if _, err := service.History(ctx, fixture.missingUser, current.ID, HistoryFilter{}); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("outsider obtained task history: %v", err)
	}

	if _, err := db.ExecContext(ctx, "UPDATE task_history SET created_at = ? WHERE task_id = ?",
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), current.ID); err != nil {
		t.Fatal(err)
	}

	last := entries[len(entries)-1]
	if _, err := db.ExecContext(ctx, "UPDATE task_history SET created_at = ? WHERE id = ?",
		time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC), last.ID); err != nil {
		t.Fatal(err)
	}

	page, err = service.History(ctx, fixture.owner, current.ID, HistoryFilter{Limit: 3})
	if err != nil || len(page) != 3 || page[0].ID != last.ID || page[1].ID != entries[0].ID || page[2].ID != entries[1].ID {
		t.Fatalf("history timestamp order or ID tie-breaker failed: %+v, %v", page, err)
	}
}
