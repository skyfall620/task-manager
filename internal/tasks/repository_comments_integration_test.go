package tasks

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/skyfall620/task-manager/internal/teams"
)

func runCommentsIntegrationTests(t *testing.T, db *sql.DB) {
	t.Helper()
	t.Run("comments-members-and-persistence", func(t *testing.T) { testCommentPersistence(t, db) })
	t.Run("comments-access-and-isolation", func(t *testing.T) { testCommentIsolation(t, db) })
	t.Run("comments-order-and-pagination", func(t *testing.T) { testCommentOrder(t, db) })
}

type commentActors struct {
	admin    uint64
	member   uint64
	outsider uint64
}

func commentFixture(t *testing.T, db *sql.DB) (context.Context, *integrationFixture, *commentActors) {
	t.Helper()
	actors := &commentActors{}
	// Register this first so team fixtures and their comments are removed before users.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, id := range []uint64{actors.admin, actors.member, actors.outsider} {
			if id == 0 {
				continue
			}

			if _, err := db.ExecContext(ctx, "DELETE FROM users WHERE id = ?", id); err != nil {
				t.Error(err)
			}
		}
	})
	ctx, fixture := taskFixture(t, db)
	for _, target := range []*uint64{&actors.admin, &actors.member, &actors.outsider} {
		result, err := db.ExecContext(ctx, "INSERT INTO users (email, password_hash, name) VALUES (?, ?, ?)",
			"comments-"+rand.Text()+"@example.test", "integration-test-not-a-login-hash", "Comment integration user")
		if err != nil {
			t.Fatal(err)
		}

		id, err := result.LastInsertId()
		if err != nil || id <= 0 {
			t.Fatalf("invalid comment fixture user ID: %v", err)
		}

		*target = uint64(id)
	}

	if _, err := db.ExecContext(ctx,
		"INSERT INTO team_members (team_id, user_id, role) VALUES (?, ?, 'admin'), (?, ?, 'member'), (?, ?, 'member')",
		fixture.team, actors.admin, fixture.team, actors.member, fixture.otherTeam, actors.outsider); err != nil {
		t.Fatal(err)
	}

	return ctx, fixture, actors
}

func testCommentPersistence(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, fixture, actors := commentFixture(t, db)
	service := NewService(NewRepository(db))
	// Closed tasks remain open for discussion by every current team member.
	task, err := service.Create(ctx, fixture.owner, CreateInput{
		TeamID: fixture.team, Title: "Closed task", Status: StatusDone, AssigneeID: &fixture.assignee,
	})
	if err != nil {
		t.Fatal(err)
	}

	before := persistedTask(t, ctx, db, task.ID)
	beforeHistory, err := service.History(ctx, fixture.owner, task.ID, HistoryFilter{})
	if err != nil {
		t.Fatal(err)
	}

	var created []Comment
	for _, actor := range []uint64{fixture.owner, fixture.assignee, actors.admin, actors.member} {
		comment, err := service.CreateComment(ctx, actor, task.ID, CreateCommentInput{Content: " \u2003 Текст 😀 'quoted'\n "})
		if err != nil {
			t.Fatal(err)
		}

		if comment.ID == 0 || comment.UserID != actor || comment.TaskID != task.ID || comment.Content != "Текст 😀 'quoted'" {
			t.Fatalf("unexpected stored comment: %+v", comment)
		}

		var stored Comment
		if err := db.QueryRowContext(ctx,
			"SELECT id, task_id, user_id, content, created_at FROM task_comments WHERE id = ?", comment.ID,
		).Scan(&stored.ID, &stored.TaskID, &stored.UserID, &stored.Content, &stored.CreatedAt); err != nil {
			t.Fatal(err)
		}

		if !reflect.DeepEqual(stored, comment) {
			t.Fatal("comment did not round-trip through MySQL")
		}

		created = append(created, comment)
	}

	boundary, err := service.CreateComment(ctx, actors.member, task.ID, CreateCommentInput{Content: strings.Repeat("😀", MaxCommentLength)})
	if err != nil {
		t.Fatal(err)
	}

	created = append(created, boundary)
	var length, byteLength int
	if err := db.QueryRowContext(ctx, "SELECT CHAR_LENGTH(content), OCTET_LENGTH(content) FROM task_comments WHERE id = ?",
		boundary.ID).Scan(&length, &byteLength); err != nil {
		t.Fatal(err)
	}

	if length != MaxCommentLength || byteLength != 4*MaxCommentLength {
		t.Fatal("Unicode length boundary was not preserved")
	}

	other, err := service.Create(ctx, fixture.owner, CreateInput{TeamID: fixture.team, Title: "Another task"})
	if err != nil {
		t.Fatal(err)
	}

	empty, err := service.Comments(ctx, actors.member, other.ID, CommentsFilter{})
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatal("new task must have an empty comment list")
	}

	if _, err := service.CreateComment(ctx, actors.member, other.ID, CreateCommentInput{Content: "Other task comment"}); err != nil {
		t.Fatal(err)
	}

	for _, actor := range []uint64{fixture.owner, fixture.assignee, actors.admin, actors.member} {
		comments, err := service.Comments(ctx, actor, task.ID, CommentsFilter{})
		if err != nil || !reflect.DeepEqual(comments, created) {
			t.Fatalf("member could not read task-scoped comments: %v", err)
		}
	}

	afterHistory, err := service.History(ctx, fixture.owner, task.ID, HistoryFilter{})
	if err != nil || !reflect.DeepEqual(beforeHistory, afterHistory) || !reflect.DeepEqual(before, persistedTask(t, ctx, db, task.ID)) {
		t.Fatal("comments changed task fields, version or history")
	}
}

func testCommentIsolation(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, fixture, actors := commentFixture(t, db)
	repository := NewRepository(db)
	service := NewService(repository)
	task := createIntegrationTask(t, ctx, service, fixture)
	other, err := service.Create(ctx, fixture.owner, CreateInput{TeamID: fixture.otherTeam, Title: "Foreign team task"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.CreateComment(ctx, actors.outsider, other.ID, CreateCommentInput{Content: "Foreign team comment"}); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct{ actor, taskID uint64 }{
		{actors.outsider, task.ID}, {actors.member, other.ID}, {actors.admin, other.ID},
	} {
		_, createErr := service.CreateComment(ctx, test.actor, test.taskID, CreateCommentInput{Content: "Denied"})
		_, readErr := service.Comments(ctx, test.actor, test.taskID, CommentsFilter{})
		if !errors.Is(createErr, ErrTaskNotFound) || !errors.Is(readErr, ErrTaskNotFound) {
			t.Fatalf("foreign team access accepted: create=%v, read=%v", createErr, readErr)
		}
	}

	for _, operation := range []string{"create", "read"} {
		err := repository.WithTeamTransaction(ctx, fixture.team, func(tx TaskTransaction) error {
			if operation == "create" {
				_, err := tx.CreateComment(ctx, Comment{
					TaskID: other.ID, UserID: actors.member, Content: "Wrong SQL scope", CreatedAt: time.Now().UTC(),
				})
				return err
			}

			_, err := tx.Comments(ctx, other.ID, CommentsFilter{Limit: DefaultLimit})
			return err
		})
		if !errors.Is(err, ErrTaskNotFound) {
			t.Fatalf("repository %s ignored locked team scope: %v", operation, err)
		}
	}

	comment, err := service.CreateComment(ctx, actors.member, task.ID, CreateCommentInput{Content: "Keep after leaving"})
	if err != nil {
		t.Fatal(err)
	}

	if err := teams.NewService(teams.NewRepository(db)).Remove(ctx, fixture.owner, fixture.team, actors.member); err != nil {
		t.Fatal(err)
	}

	_, createErr := service.CreateComment(ctx, actors.member, task.ID, CreateCommentInput{Content: "Denied after removal"})
	_, readErr := service.Comments(ctx, actors.member, task.ID, CommentsFilter{})
	if !errors.Is(createErr, ErrTaskNotFound) || !errors.Is(readErr, ErrTaskNotFound) {
		t.Fatal("removed member retained comment access")
	}

	comments, err := service.Comments(ctx, fixture.owner, task.ID, CommentsFilter{})
	if err != nil || len(comments) != 1 || comments[0].ID != comment.ID || comments[0].UserID != actors.member {
		t.Fatal("remaining members lost comments from a former member")
	}

	comments, err = service.Comments(ctx, actors.outsider, other.ID, CommentsFilter{})
	if err != nil || len(comments) != 1 || comments[0].Content != "Foreign team comment" {
		t.Fatal("cross-team attempts changed foreign comments")
	}
}

func testCommentOrder(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, fixture, actors := commentFixture(t, db)
	service := NewService(NewRepository(db))
	task := createIntegrationTask(t, ctx, service, fixture)
	var created []Comment
	for _, text := range []string{"First", "Second", "Third"} {
		comment, err := service.CreateComment(ctx, actors.member, task.ID, CreateCommentInput{Content: text})
		if err != nil {
			t.Fatal(err)
		}

		created = append(created, comment)
	}

	if _, err := db.ExecContext(ctx, "UPDATE task_comments SET created_at = ? WHERE task_id = ?",
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), task.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, "UPDATE task_comments SET created_at = ? WHERE id = ?",
		time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC), created[2].ID); err != nil {
		t.Fatal(err)
	}

	comments, err := service.Comments(ctx, actors.admin, task.ID, CommentsFilter{})
	if err != nil || len(comments) != 3 || comments[0].ID != created[2].ID || comments[1].ID != created[0].ID || comments[2].ID != created[1].ID {
		t.Fatalf("comment timestamp order or ID tie-breaker failed: %+v, %v", comments, err)
	}

	page, err := service.Comments(ctx, actors.member, task.ID, CommentsFilter{Limit: 2, Offset: 1})
	if err != nil || len(page) != 2 || page[0].ID != created[0].ID || page[1].ID != created[1].ID {
		t.Fatalf("unexpected comment page: %+v, %v", page, err)
	}

	page, err = service.Comments(ctx, actors.member, task.ID, CommentsFilter{Offset: 99})
	if err != nil || page == nil || len(page) != 0 {
		t.Fatal("offset beyond the end must return an empty list")
	}
}
