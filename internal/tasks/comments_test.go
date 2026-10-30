package tasks

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func (f *repositoryFake) CreateComment(ctx context.Context, comment Comment) (Comment, error) {
	f.requireTransaction()
	f.createCommentCalls++
	if _, err := f.LockTask(ctx, comment.TaskID); err != nil {
		return Comment{}, err
	}

	if f.commentErr != nil {
		return Comment{}, f.commentErr
	}

	comment.ID = uint64(len(f.comments) + 1)
	f.comments = append(f.comments, comment)
	return comment, nil
}

func (f *repositoryFake) Comments(ctx context.Context, taskID uint64, filter CommentsFilter) ([]Comment, error) {
	f.requireTransaction()
	f.commentReadCalls++
	f.lastCommentsFilter = filter
	if _, err := f.LockTask(ctx, taskID); err != nil {
		return nil, err
	}

	if f.commentsErr != nil {
		return nil, f.commentsErr
	}

	var result []Comment
	for _, comment := range f.comments {
		if comment.TaskID == taskID {
			result = append(result, comment)
		}
	}

	if filter.Offset >= len(result) {
		return nil, nil
	}

	end := min(len(result), filter.Offset+filter.Limit)
	return result[filter.Offset:end], nil
}

func TestCommentsAllMembersAndTaskUnchanged(t *testing.T) {
	for _, actor := range []uint64{1, 2, 3, 4, 5} {
		repository, task := updateFixture(t)
		service := NewService(repository)
		before := repository.history[0]
		comment, err := service.CreateComment(context.Background(), actor, task.ID, CreateCommentInput{Content: " \t Текст\nкомментария \u2003"})
		if err != nil {
			t.Fatal(err)
		}

		if comment.ID == 0 || comment.TaskID != task.ID || comment.UserID != actor || comment.Content != "Текст\nкомментария" || comment.CreatedAt.IsZero() {
			t.Fatalf("unexpected created comment: %+v", comment)
		}

		comments, err := service.Comments(context.Background(), actor, task.ID, CommentsFilter{})
		if err != nil || len(comments) != 1 || !reflect.DeepEqual(comments[0], comment) || repository.lastCommentsFilter.Limit != DefaultLimit {
			t.Fatalf("unexpected comments: %+v, %v", comments, err)
		}

		if !reflect.DeepEqual(repository.tasks[0], task) || repository.updateCalls != 0 || repository.historyCalls != 1 || len(repository.history) != 1 || !reflect.DeepEqual(repository.history[0], before) {
			t.Fatal("comment creation or reading changed task or history")
		}
	}
}

func TestCommentUnicodeLength(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		valid   bool
	}{
		{"empty", "", false},
		{"whitespace", " \t\r\n\u2003", false},
		{"one character", " я ", true},
		{"maximum multibyte", " \n" + strings.Repeat("😀", MaxCommentLength) + "\t ", true},
		{"too long", strings.Repeat("я", MaxCommentLength+1), false},
		{"invalid UTF8", "\xff", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, task := updateFixture(t)
			_, err := NewService(repository).CreateComment(context.Background(), 5, task.ID, CreateCommentInput{Content: test.content})
			if test.valid {
				if err != nil || repository.createCommentCalls != 1 {
					t.Fatalf("valid comment rejected: %v", err)
				}

				return
			}

			if !errors.Is(err, ErrInvalidInput) || repository.createCommentCalls != 0 {
				t.Fatalf("invalid comment reached repository: %v", err)
			}
		})
	}
}

func TestCommentsAccessAndPaginationValidation(t *testing.T) {
	for _, test := range []struct {
		actor uint64
		id    uint64
		want  error
	}{{0, 1, ErrUnauthenticated}, {1, 0, ErrInvalidInput}, {99, 1, ErrTaskNotFound}, {1, 99, ErrTaskNotFound}} {
		repository, _ := updateFixture(t)
		service := NewService(repository)
		_, createErr := service.CreateComment(context.Background(), test.actor, test.id, CreateCommentInput{Content: "Comment"})
		_, readErr := service.Comments(context.Background(), test.actor, test.id, CommentsFilter{})
		if !errors.Is(createErr, test.want) || !errors.Is(readErr, test.want) || repository.createCommentCalls != 0 || repository.commentReadCalls != 0 {
			t.Fatalf("unexpected access errors: create=%v, read=%v", createErr, readErr)
		}
	}

	for _, filter := range []CommentsFilter{{Limit: -1}, {Limit: MaxLimit + 1}, {Offset: -1}} {
		repository, task := updateFixture(t)
		_, err := NewService(repository).Comments(context.Background(), 1, task.ID, filter)
		if !errors.Is(err, ErrInvalidInput) || repository.commentReadCalls != 0 {
			t.Fatalf("invalid pagination accepted: %v", err)
		}
	}
}

func TestCommentsCurrentMembershipAndFormerAuthors(t *testing.T) {
	for _, operation := range []string{"create", "read"} {
		repository, task := updateFixture(t)
		repository.before = func() { delete(repository.members, 5) }
		service := NewService(repository)
		var err error
		if operation == "create" {
			_, err = service.CreateComment(context.Background(), 5, task.ID, CreateCommentInput{Content: "Denied"})
		} else {
			_, err = service.Comments(context.Background(), 5, task.ID, CommentsFilter{})
		}

		if !errors.Is(err, ErrTaskNotFound) || repository.createCommentCalls != 0 || repository.commentReadCalls != 0 {
			t.Fatal("removed member retained comment access")
		}
	}

	repository, task := updateFixture(t)
	service := NewService(repository)
	comment, err := service.CreateComment(context.Background(), 5, task.ID, CreateCommentInput{Content: "Keep this comment"})
	if err != nil {
		t.Fatal(err)
	}

	delete(repository.members, 5)
	comments, err := service.Comments(context.Background(), 1, task.ID, CommentsFilter{})
	if err != nil || len(comments) != 1 || comments[0].ID != comment.ID {
		t.Fatal("current members lost comments from a former member")
	}
}

func TestCommentsPagesAndWriteErrors(t *testing.T) {
	repository, task := updateFixture(t)
	service := NewService(repository)
	ctx := context.Background()
	for _, text := range []string{"First", "Second", "Third"} {
		if _, err := service.CreateComment(ctx, 5, task.ID, CreateCommentInput{Content: text}); err != nil {
			t.Fatal(err)
		}
	}

	page, err := service.Comments(ctx, 1, task.ID, CommentsFilter{Limit: 1, Offset: 1})
	if err != nil || len(page) != 1 || page[0].Content != "Second" || repository.lastCommentsFilter.Offset != 1 {
		t.Fatalf("unexpected page: %+v, %v", page, err)
	}

	page, err = service.Comments(ctx, 1, task.ID, CommentsFilter{Offset: 99})
	if err != nil || page == nil || len(page) != 0 {
		t.Fatal("empty page must be an empty array")
	}

	expected := errors.New("database failed")
	repository.commentErr = expected
	comment, err := service.CreateComment(ctx, 5, task.ID, CreateCommentInput{Content: "Failed"})
	if !errors.Is(err, expected) || comment.ID != 0 || len(repository.comments) != 3 {
		t.Fatalf("failed insert returned or stored a comment: %+v, %v", comment, err)
	}

	repository.commentsErr = expected
	if _, err := service.Comments(ctx, 1, task.ID, CommentsFilter{}); !errors.Is(err, expected) {
		t.Fatalf("read error lost: %v", err)
	}
}
