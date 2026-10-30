package tasks

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// TASKS_TEST_MYSQL_DSN must point to a separate test database with migrations applied.
func TestRepositoryIntegration(t *testing.T) {
	dsn := os.Getenv("TASKS_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set TASKS_TEST_MYSQL_DSN to run MySQL integration tests")
	}

	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid TASKS_TEST_MYSQL_DSN")
	}

	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 5 * time.Second
	cfg.WriteTimeout = 5 * time.Second
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal("cannot configure integration database connector")
	}

	db := sql.OpenDB(connector)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})

	t.Run("creation-history-filtering-and-order", func(t *testing.T) {
		ctx, fixture := taskFixture(t, db)
		service := NewService(NewRepository(db))
		var created []Task
		for i, status := range []Status{StatusTodo, StatusInProgress, StatusInProgress, StatusDone} {
			var assigneeID *uint64
			if i != 0 {
				assigneeID = &fixture.assignee
			}

			task, err := service.Create(ctx, fixture.owner, CreateInput{
				TeamID: fixture.team, Title: "Integration task", Status: status, AssigneeID: assigneeID,
			})
			if err != nil {
				t.Fatal(err)
			}

			created = append(created, task)
			var entry HistoryEntry
			var changes []byte
			err = db.QueryRowContext(ctx,
				"SELECT task_id, changed_by, changes, created_at FROM task_history WHERE task_id = ?", task.ID,
			).Scan(&entry.TaskID, &entry.ChangedBy, &changes, &entry.CreatedAt)
			if err != nil {
				t.Fatal(err)
			}

			decoder := json.NewDecoder(bytes.NewReader(changes))
			decoder.UseNumber()
			if err := decoder.Decode(&entry.Changes); err != nil {
				t.Fatal(err)
			}

			if entry.ChangedBy != fixture.owner || !entry.CreatedAt.Equal(task.CreatedAt) {
				t.Fatal("incorrect persisted initial history metadata")
			}

			assertInitialChanges(t, task, entry.Changes)
		}

		if _, err := service.Create(ctx, fixture.owner, CreateInput{
			TeamID: fixture.otherTeam, Title: "Other team task", Status: StatusInProgress, AssigneeID: &fixture.assignee,
		}); err != nil {
			t.Fatal(err)
		}

		// Force equal timestamps so the test also exercises the ID tie-breaker.
		if _, err := db.ExecContext(ctx, "UPDATE tasks SET created_at = ? WHERE team_id = ?",
			time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), fixture.team); err != nil {
			t.Fatal(err)
		}

		// The oldest ID now has the newest timestamp, ahead of the tied rows.
		if _, err := db.ExecContext(ctx, "UPDATE tasks SET created_at = ? WHERE id = ?",
			time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC), created[0].ID); err != nil {
			t.Fatal(err)
		}

		list, err := service.List(ctx, fixture.owner, ListFilter{TeamID: fixture.team})
		if err != nil || len(list) != len(created) {
			t.Fatalf("unexpected persisted list: length=%d, error=%v", len(list), err)
		}

		expectedIDs := []uint64{created[0].ID, created[3].ID, created[2].ID, created[1].ID}
		for i, task := range list {
			if task.TeamID != fixture.team || task.ID != expectedIDs[i] {
				t.Fatal("team isolation or descending order failed")
			}

			if task.ID == created[0].ID && (task.AssigneeID != nil || task.ClosedAt != nil) {
				t.Fatal("nullable task fields did not round-trip through MySQL")
			}
		}

		list, err = service.List(ctx, fixture.assignee, ListFilter{
			TeamID: fixture.team, Status: StatusInProgress, AssigneeID: &fixture.assignee, Limit: 1, Offset: 1,
		})
		if err != nil || len(list) != 1 || list[0].ID != created[1].ID {
			t.Fatalf("filtering or pagination failed: %+v, %v", list, err)
		}
	})

	t.Run("history-insert-failure-rolls-back-task", func(t *testing.T) {
		ctx, fixture := taskFixture(t, db)
		now := time.Now().UTC().Truncate(time.Microsecond)
		var taskID uint64
		err := NewRepository(db).WithTeamTransaction(ctx, fixture.team, func(tx TaskTransaction) error {
			task, err := tx.Create(ctx, Task{
				TeamID: fixture.team, Title: "Must roll back", Status: StatusTodo, CreatedBy: fixture.owner,
				CreatedAt: now, UpdatedAt: now, Version: 1,
			})
			if err != nil {
				return err
			}

			taskID = task.ID
			// The deleted fixture user forces a foreign-key failure in the history INSERT.
			return tx.AddHistory(ctx, HistoryEntry{TaskID: task.ID, ChangedBy: fixture.missingUser, Changes: initialChanges(task), CreatedAt: now})
		})
		var mysqlErr *mysql.MySQLError
		if taskID == 0 || !errors.As(err, &mysqlErr) || mysqlErr.Number != 1452 {
			t.Fatalf("expected history foreign-key failure, got %v", err)
		}

		for _, query := range []string{
			"SELECT COUNT(*) FROM tasks WHERE id = ?",
			"SELECT COUNT(*) FROM task_history WHERE task_id = ?",
		} {
			var count int
			if err := db.QueryRowContext(ctx, query, taskID).Scan(&count); err != nil {
				t.Fatal(err)
			}

			if count != 0 {
				t.Fatal("failed transaction left persisted data")
			}
		}
	})

	t.Run("cursor-pagination", func(t *testing.T) {
		ctx, fixture := taskFixture(t, db)
		service := NewService(NewRepository(db))
		var created []Task
		for range 5 {
			task, err := service.Create(ctx, fixture.owner, CreateInput{TeamID: fixture.team, Title: "Cursor task", AssigneeID: &fixture.assignee})
			if err != nil {
				t.Fatal(err)
			}

			created = append(created, task)
		}

		for _, input := range []CreateInput{
			{TeamID: fixture.team, Title: "Excluded status", Status: StatusDone},
			{TeamID: fixture.otherTeam, Title: "Other team", AssigneeID: &fixture.assignee},
		} {
			if _, err := service.Create(ctx, fixture.owner, input); err != nil {
				t.Fatal(err)
			}
		}

		boundary := time.Date(2020, 1, 1, 0, 0, 0, 123456000, time.UTC)
		if _, err := db.ExecContext(ctx, "UPDATE tasks SET created_at = ? WHERE team_id = ?", boundary, fixture.team); err != nil {
			t.Fatal(err)
		}

		if _, err := db.ExecContext(ctx, "UPDATE tasks SET created_at = ? WHERE id = ?", boundary.Add(-time.Microsecond), created[4].ID); err != nil {
			t.Fatal(err)
		}

		filter := ListFilter{TeamID: fixture.team, Status: StatusTodo, AssigneeID: &fixture.assignee, Limit: 2, cursorMode: true}
		var seen []uint64
		for pageNumber := 0; ; pageNumber++ {
			rows, err := service.List(ctx, fixture.owner, filter)
			if err != nil {
				t.Fatal(err)
			}

			hasMore := len(rows) > filter.Limit
			if hasMore {
				rows = rows[:filter.Limit]
			}

			for _, task := range rows {
				if task.TeamID != fixture.team || task.Status != StatusTodo || task.AssigneeID == nil || *task.AssigneeID != fixture.assignee {
					t.Fatal("cursor query lost filters or team scope")
				}

				seen = append(seen, task.ID)
			}

			if pageNumber == 0 {
				if _, err := service.Create(ctx, fixture.owner, CreateInput{TeamID: fixture.team, Title: "Newer task", AssigneeID: &fixture.assignee}); err != nil {
					t.Fatal(err)
				}
			}

			if !hasMore {
				break
			}

			if pageNumber >= 5 {
				t.Fatal("cursor did not advance")
			}

			filter.Cursor, err = decodeListCursor(encodeListCursor(rows[len(rows)-1]))
			if err != nil {
				t.Fatal(err)
			}
		}

		if len(seen) != len(created) {
			t.Fatalf("seen %v; want five original tasks", seen)
		}

		expected := []uint64{created[3].ID, created[2].ID, created[1].ID, created[0].ID, created[4].ID}
		for i, id := range seen {
			if id != expected[i] {
				t.Fatalf("task skipped, repeated or incorrectly ordered: %v", seen)
			}
		}

		legacy := filter
		legacy.Cursor, legacy.cursorMode, legacy.Offset = nil, false, 1
		rows, err := service.List(ctx, fixture.owner, legacy)
		if err != nil || len(rows) != 2 || rows[0].ID != created[3].ID || rows[1].ID != created[2].ID {
			t.Fatalf("legacy offset pagination changed: %v", err)
		}

		if _, err := service.List(ctx, fixture.missingUser, filter); !errors.Is(err, ErrTeamNotFound) {
			t.Fatal("cursor exposed tasks to a non-member")
		}
	})

	runUpdateIntegrationTests(t, db)
	runCommentsIntegrationTests(t, db)
	runCacheIntegrationTests(t, db)
}

type integrationFixture struct {
	owner       uint64
	assignee    uint64
	team        uint64
	otherTeam   uint64
	missingUser uint64
}

func taskFixture(t *testing.T, db *sql.DB) (context.Context, *integrationFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	fixture := &integrationFixture{}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		for _, teamID := range []uint64{fixture.team, fixture.otherTeam} {
			if teamID == 0 {
				continue
			}

			for _, query := range []string{
				"DELETE FROM task_comments WHERE task_id IN (SELECT id FROM tasks WHERE team_id = ?)",
				"DELETE FROM task_history WHERE task_id IN (SELECT id FROM tasks WHERE team_id = ?)",
				"DELETE FROM tasks WHERE team_id = ?",
				"DELETE FROM team_members WHERE team_id = ?",
				"DELETE FROM teams WHERE id = ?",
			} {
				if _, err := db.ExecContext(cleanupCtx, query, teamID); err != nil {
					t.Error(err)
				}
			}
		}

		for _, userID := range []uint64{fixture.owner, fixture.assignee, fixture.missingUser} {
			if userID != 0 {
				if _, err := db.ExecContext(cleanupCtx, "DELETE FROM users WHERE id = ?", userID); err != nil {
					t.Error(err)
				}
			}
		}
	})
	insertID := func(query string, args ...any) uint64 {
		t.Helper()
		result, err := db.ExecContext(ctx, query, args...)
		if err != nil {
			t.Fatal(err)
		}

		id, err := result.LastInsertId()
		if err != nil || id <= 0 {
			t.Fatalf("invalid fixture insert ID: %v", err)
		}

		return uint64(id)
	}
	for _, target := range []*uint64{&fixture.owner, &fixture.assignee, &fixture.missingUser} {
		*target = insertID("INSERT INTO users (email, password_hash, name) VALUES (?, ?, ?)",
			"tasks-"+rand.Text()+"@example.test", "integration-test-not-a-login-hash", "Task integration user")
	}

	if _, err := db.ExecContext(ctx, "DELETE FROM users WHERE id = ?", fixture.missingUser); err != nil {
		t.Fatal(err)
	}

	for _, target := range []*uint64{&fixture.team, &fixture.otherTeam} {
		*target = insertID("INSERT INTO teams (name, created_by) VALUES (?, ?)", "Task integration team", fixture.owner)
		if _, err := db.ExecContext(ctx, "INSERT INTO team_members (team_id, user_id, role) VALUES (?, ?, 'owner'), (?, ?, 'member')",
			*target, fixture.owner, *target, fixture.assignee); err != nil {
			t.Fatal(err)
		}
	}

	return ctx, fixture
}
