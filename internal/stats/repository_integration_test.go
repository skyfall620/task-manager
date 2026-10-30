package stats

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"math"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/skyfall620/task-manager/internal/teams"
)

// Reuse the existing runner's DSN for a migrated, separate test database.
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
	t.Run("aggregates-and-team-isolation", func(t *testing.T) { testReportAggregates(t, db) })
	t.Run("empty-and-open-only-teams", func(t *testing.T) { testEmptyReport(t, db) })
	t.Run("utc-window-boundaries", func(t *testing.T) { testReportWindow(t, db) })
	t.Run("current-assignee-and-fractional-seconds", func(t *testing.T) { testReportCurrentAssignee(t, db) })
	t.Run("current-access-and-role-revocation", func(t *testing.T) { testReportAccess(t, db) })
}

type statsFixture struct {
	users           []uint64
	team, otherTeam uint64
}

func reportFixture(t *testing.T, db *sql.DB) (context.Context, *statsFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	fixture := &statsFixture{}
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

		for _, userID := range fixture.users {
			if _, err := db.ExecContext(cleanupCtx, "DELETE FROM users WHERE id = ?", userID); err != nil {
				t.Error(err)
			}
		}
	})
	for _, name := range []string{"Owner", "Alice", "Bob", "Carol", "Dave", "Admin", "Member", "Other owner"} {
		id := insertFixtureRow(t, ctx, db,
			"INSERT INTO users (email, password_hash, name) VALUES (?, ?, ?)",
			"stats-"+rand.Text()+"@example.test", "integration-test-not-a-login-hash", name)
		fixture.users = append(fixture.users, id)
	}

	teamService := teams.NewService(teams.NewRepository(db))
	team, err := teamService.Create(ctx, fixture.users[0], "Stats integration team")
	if err != nil {
		t.Fatal(err)
	}

	fixture.team = team.ID
	otherTeam, err := teamService.Create(ctx, fixture.users[7], "Other stats integration team")
	if err != nil {
		t.Fatal(err)
	}

	fixture.otherTeam = otherTeam.ID
	for i := 1; i <= 6; i++ {
		role := teams.RoleMember
		if i == 5 {
			role = teams.RoleAdmin
		}

		if _, err := db.ExecContext(ctx,
			"INSERT INTO team_members (team_id, user_id, role) VALUES (?, ?, ?)", fixture.team, fixture.users[i], role); err != nil {
			t.Fatal(err)
		}
	}

	for i := 1; i <= 4; i++ {
		if _, err := db.ExecContext(ctx,
			"INSERT INTO team_members (team_id, user_id, role) VALUES (?, ?, 'member')", fixture.otherTeam, fixture.users[i]); err != nil {
			t.Fatal(err)
		}
	}

	return ctx, fixture
}

func insertFixtureRow(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) uint64 {
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

func seedReportTask(t *testing.T, ctx context.Context, db *sql.DB, fixture *statsFixture,
	teamID, assigneeID uint64, status string, createdAt time.Time, closedAt *time.Time) uint64 {
	t.Helper()
	var assignee any
	if assigneeID != 0 {
		assignee = assigneeID
	}

	creator := fixture.users[0]
	if teamID == fixture.otherTeam {
		creator = fixture.users[7]
	}

	return insertFixtureRow(t, ctx, db, `
		INSERT INTO tasks (team_id, title, description, status, created_by, assignee_id,
		                   created_at, updated_at, closed_at, version)
		VALUES (?, 'Stats fixture', '', ?, ?, ?, ?, ?, ?, 1)`,
		teamID, status, creator, assignee, createdAt, createdAt, closedAt)
}

func seedComments(t *testing.T, ctx context.Context, db *sql.DB, taskID, userID uint64, count int, createdAt time.Time) {
	t.Helper()
	for range count {
		if _, err := db.ExecContext(ctx,
			"INSERT INTO task_comments (task_id, user_id, content, created_at) VALUES (?, ?, 'Fixture comment', ?)",
			taskID, userID, createdAt); err != nil {
			t.Fatal(err)
		}
	}
}

func reportAt(t *testing.T, ctx context.Context, db *sql.DB, teamID uint64, asOf time.Time) Report {
	t.Helper()
	var report Report
	err := NewRepository(db).WithTeamTransaction(ctx, teamID, func(tx ReportTransaction) error {
		var err error
		report, err = tx.Report(ctx, asOf)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return report
}

func testReportAggregates(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, fixture := reportFixture(t, db)
	asOf := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	closedAt := asOf.Add(-24 * time.Hour)
	var firstAlice, firstBob uint64
	for performer, count := range []int{4, 3, 3, 1} {
		for i := range count {
			duration := time.Duration(i+1)*time.Second + 250*time.Millisecond
			switch performer {
			case 1:
				duration = time.Duration(10+i)*time.Second + 500*time.Millisecond
			case 2:
				duration = time.Duration(20+i)*time.Second + 250*time.Millisecond
			case 3:
				duration = 40500 * time.Millisecond
			}

			id := seedReportTask(t, ctx, db, fixture, fixture.team, fixture.users[performer+1],
				"done", closedAt.Add(-duration), &closedAt)
			if performer == 0 && i == 0 {
				firstAlice = id
			}

			if performer == 1 && i == 0 {
				firstBob = id
			}
		}
	}

	oldClose := asOf.Add(-31 * 24 * time.Hour)
	seedReportTask(t, ctx, db, fixture, fixture.team, fixture.users[1], "done", oldClose.Add(-100750*time.Millisecond), &oldClose)
	seedReportTask(t, ctx, db, fixture, fixture.team, 0, "done", closedAt.Add(-200500*time.Millisecond), &closedAt)
	todo := seedReportTask(t, ctx, db, fixture, fixture.team, 0, "todo", asOf.Add(-365*24*time.Hour), nil)
	seedReportTask(t, ctx, db, fixture, fixture.team, fixture.users[1], "todo", asOf, nil)
	seedReportTask(t, ctx, db, fixture, fixture.team, fixture.users[2], "in_progress", asOf, nil)
	seedComments(t, ctx, db, firstAlice, fixture.users[0], 5, asOf.Add(-time.Hour))
	seedComments(t, ctx, db, firstBob, fixture.users[0], 2, asOf.Add(-time.Hour))
	seedComments(t, ctx, db, todo, fixture.users[0], 1, oldClose)
	for range 5 {
		id := seedReportTask(t, ctx, db, fixture, fixture.otherTeam, fixture.users[1], "done", closedAt.Add(-time.Hour), &closedAt)
		seedComments(t, ctx, db, id, fixture.users[7], 2, asOf)
	}

	report := reportAt(t, ctx, db, fixture.team, asOf)
	wantTop := []AssigneeStats{
		{UserID: fixture.users[1], Name: "Alice", ClosedTasks: 4},
		{UserID: fixture.users[2], Name: "Bob", ClosedTasks: 3},
		{UserID: fixture.users[3], Name: "Carol", ClosedTasks: 3},
	}
	if report.TeamID != fixture.team || report.TasksByStatus != (TaskCounts{Todo: 2, InProgress: 1, Done: 13}) || report.CommentsCount != 8 || !reflect.DeepEqual(report.TopAssignees, wantTop) {
		t.Fatalf("incorrect or cross-team report: %+v", report)
	}

	// Includes the old and unassigned closed tasks, once each, regardless of comments.
	if report.AverageCloseTimeSeconds == nil {
		t.Fatal("expected an all-time closing average")
	}

	if average := *report.AverageCloseTimeSeconds; math.Abs(average-451.0/13.0) > 1e-8 {
		t.Fatalf("incorrect all-time average: %.12f; want %.12f", average, 451.0/13.0)
	}
}

func testEmptyReport(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, fixture := reportFixture(t, db)
	service := NewService(NewRepository(db))
	report, err := service.Report(ctx, fixture.users[0], fixture.team)
	if err != nil || report.TeamID != fixture.team || report.TasksByStatus != (TaskCounts{}) || report.CommentsCount != 0 || report.AverageCloseTimeSeconds != nil || report.TopAssignees == nil || len(report.TopAssignees) != 0 {
		t.Fatalf("unexpected empty team report: %+v, %v", report, err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	seedReportTask(t, ctx, db, fixture, fixture.team, 0, "todo", now, nil)
	seedReportTask(t, ctx, db, fixture, fixture.team, fixture.users[1], "in_progress", now, nil)
	report, err = service.Report(ctx, fixture.users[0], fixture.team)
	if err != nil || report.TasksByStatus != (TaskCounts{Todo: 1, InProgress: 1}) || report.AverageCloseTimeSeconds != nil || report.TopAssignees == nil || len(report.TopAssignees) != 0 {
		t.Fatalf("open tasks created a closing average or top assignee: %+v, %v", report, err)
	}
}

func testReportWindow(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, fixture := reportFixture(t, db)
	asOf := time.Date(2026, 11, 2, 9, 30, 0, 123456000, time.UTC)
	cutoff := asOf.Add(-30 * 24 * time.Hour)
	for _, test := range []struct {
		assignee uint64
		closedAt time.Time
	}{
		{fixture.users[1], cutoff.Add(-time.Microsecond)},
		{fixture.users[2], cutoff},
		{fixture.users[3], cutoff.Add(time.Microsecond)},
		{fixture.users[2], asOf},
		{fixture.users[4], asOf.Add(time.Microsecond)},
		{fixture.users[3], asOf.Add(-24 * time.Hour)},
	} {
		// Creation outside the window must not exclude a recent closure.
		seedReportTask(t, ctx, db, fixture, fixture.team, test.assignee, "done", asOf.Add(-70*24*time.Hour), &test.closedAt)
	}

	report := reportAt(t, ctx, db, fixture.team, asOf.In(time.FixedZone("test-offset", 5*60*60)))
	want := []AssigneeStats{
		{UserID: fixture.users[2], Name: "Bob", ClosedTasks: 2},
		{UserID: fixture.users[3], Name: "Carol", ClosedTasks: 2},
	}
	if report.TasksByStatus.Done != 6 || !reflect.DeepEqual(report.TopAssignees, want) {
		t.Fatalf("UTC window, microsecond boundaries or tie-breaker failed: %+v", report)
	}
}

func testReportCurrentAssignee(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, fixture := reportFixture(t, db)
	asOf := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	closedAt := asOf.Add(-time.Hour)
	id := seedReportTask(t, ctx, db, fixture, fixture.team, fixture.users[1], "done", closedAt.Add(-1500001*time.Microsecond), &closedAt)
	if _, err := db.ExecContext(ctx, "UPDATE tasks SET assignee_id = ? WHERE id = ?", fixture.users[2], id); err != nil {
		t.Fatal(err)
	}

	report := reportAt(t, ctx, db, fixture.team, asOf)
	want := []AssigneeStats{{UserID: fixture.users[2], Name: "Bob", ClosedTasks: 1}}
	if !reflect.DeepEqual(report.TopAssignees, want) || report.AverageCloseTimeSeconds == nil || math.Abs(*report.AverageCloseTimeSeconds-1.500001) > 1e-8 {
		t.Fatalf("report used old assignee or lost fractional seconds: %+v", report)
	}

	if _, err := db.ExecContext(ctx, "UPDATE tasks SET status = 'in_progress', closed_at = NULL WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}

	report = reportAt(t, ctx, db, fixture.team, asOf)
	if report.TasksByStatus != (TaskCounts{InProgress: 1}) || report.AverageCloseTimeSeconds != nil || len(report.TopAssignees) != 0 {
		t.Fatalf("reopened task remained in closed-task analytics: %+v", report)
	}
}

func testReportAccess(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, fixture := reportFixture(t, db)
	service := NewService(NewRepository(db))
	for _, test := range []struct {
		actor uint64
		want  error
	}{
		{fixture.users[0], nil},
		{fixture.users[5], nil},
		{fixture.users[6], teams.ErrForbidden},
		{fixture.users[7], teams.ErrTeamNotFound},
	} {
		report, err := service.Report(ctx, test.actor, fixture.team)
		if !errors.Is(err, test.want) || (test.want != nil && !reflect.DeepEqual(report, Report{})) {
			t.Fatalf("incorrect report access for %d: %+v, %v", test.actor, report, err)
		}
	}

	teamService := teams.NewService(teams.NewRepository(db))
	if _, err := teamService.ChangeRole(ctx, fixture.users[0], fixture.team, fixture.users[5], teams.RoleMember); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Report(ctx, fixture.users[5], fixture.team); !errors.Is(err, teams.ErrForbidden) {
		t.Fatalf("demoted admin still accessed stats: %v", err)
	}

	if err := teamService.Remove(ctx, fixture.users[0], fixture.team, fixture.users[5]); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Report(ctx, fixture.users[5], fixture.team); !errors.Is(err, teams.ErrTeamNotFound) {
		t.Fatalf("removed member still accessed stats: %v", err)
	}
}
