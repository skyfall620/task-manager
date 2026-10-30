package tasks

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestCommentHTTPAllMembers(t *testing.T) {
	for _, actor := range []uint64{1, 2, 3, 4, 5} {
		repository, task := updateFixture(t)
		handler := NewHandler(NewService(repository))
		response := serveTaskHandler(t, actor, handler.CreateComment, http.MethodPost, "1", "/comments", `{"content":"  Комментарий  "}`)
		var comment Comment
		if err := json.Unmarshal(response.Body.Bytes(), &comment); err != nil || response.Code != http.StatusCreated {
			t.Fatalf("unexpected create response: %d %s", response.Code, response.Body.String())
		}

		if comment.ID == 0 || comment.TaskID != task.ID || comment.UserID != actor || comment.Content != "Комментарий" || comment.CreatedAt.IsZero() {
			t.Fatalf("incorrect comment response: %+v", comment)
		}

		response = serveTaskHandler(t, actor, handler.Comments, http.MethodGet, "1", "/comments?limit=1&offset=0", "")
		var comments []Comment
		if err := json.Unmarshal(response.Body.Bytes(), &comments); err != nil || response.Code != http.StatusOK || len(comments) != 1 || comments[0].ID != comment.ID {
			t.Fatalf("unexpected comments response: %d %s", response.Code, response.Body.String())
		}

		if repository.updateCalls != 0 || repository.historyCalls != 1 {
			t.Fatal("comment endpoints modified task or history")
		}
	}
}

func TestCommentHTTPInvalidBodies(t *testing.T) {
	for _, body := range []string{
		`{}`, `null`, `[]`, `{`, `{} {}`, `{"content":null}`, `{"content":123}`,
		`{"content":""}`, `{"content":" \t\n\u2003 "}`,
		`{"content":"` + strings.Repeat("😀", MaxCommentLength+1) + `"}`,
	} {
		repository, _ := updateFixture(t)
		response := serveTaskHandler(t, 1, NewHandler(NewService(repository)).CreateComment, http.MethodPost, "1", "/comments", body)
		if response.Code != http.StatusBadRequest || repository.createCommentCalls != 0 {
			t.Fatalf("invalid body accepted: status=%d", response.Code)
		}
	}

	for _, field := range []string{"id", "task_id", "user_id", "created_at", "version"} {
		repository, _ := updateFixture(t)
		body := `{"content":"Comment","` + field + `":1}`
		response := serveTaskHandler(t, 1, NewHandler(NewService(repository)).CreateComment, http.MethodPost, "1", "/comments", body)
		if response.Code != http.StatusBadRequest || repository.createCommentCalls != 0 {
			t.Fatalf("client-controlled %s accepted", field)
		}
	}

	repository, _ := updateFixture(t)
	body, err := json.Marshal(CreateCommentInput{Content: strings.Repeat("😀", MaxCommentLength)})
	if err != nil {
		t.Fatal(err)
	}

	response := serveTaskHandler(t, 1, NewHandler(NewService(repository)).CreateComment, http.MethodPost, "1", "/comments", string(body))
	if response.Code != http.StatusCreated {
		t.Fatalf("valid Unicode boundary rejected: %d %s", response.Code, response.Body.String())
	}
}

func TestCommentHTTPAccessAndTaskIDs(t *testing.T) {
	for _, test := range []struct {
		actor  uint64
		id     string
		status int
	}{{99, "1", http.StatusNotFound}, {1, "99", http.StatusNotFound}, {1, "0", http.StatusBadRequest}, {1, "bad", http.StatusBadRequest}} {
		repository, _ := updateFixture(t)
		handler := NewHandler(NewService(repository))
		for _, operation := range []struct {
			method  string
			handler http.HandlerFunc
		}{{http.MethodPost, handler.CreateComment}, {http.MethodGet, handler.Comments}} {
			response := serveTaskHandler(t, test.actor, operation.handler, operation.method, test.id, "/comments", `{"content":"Denied"}`)
			if response.Code != test.status || repository.createCommentCalls != 0 || repository.commentReadCalls != 0 {
				t.Fatalf("unexpected access response: %d %s", response.Code, response.Body.String())
			}

			if test.status == http.StatusNotFound {
				var body errorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != "task_not_found" {
					t.Fatal("missing and foreign tasks must use the same error")
				}
			}
		}
	}
}

func TestCommentHTTPPagination(t *testing.T) {
	repository, _ := updateFixture(t)
	handler := NewHandler(NewService(repository))
	response := serveTaskHandler(t, 5, handler.Comments, http.MethodGet, "1", "/comments", "")
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "[]" || repository.lastCommentsFilter.Limit != DefaultLimit {
		t.Fatal("unexpected empty comments response")
	}

	for _, content := range []string{"First", "Second", "Third"} {
		response = serveTaskHandler(t, 5, handler.CreateComment, http.MethodPost, "1", "/comments", `{"content":"`+content+`"}`)
		if response.Code != http.StatusCreated {
			t.Fatal("could not create pagination fixture")
		}
	}

	response = serveTaskHandler(t, 1, handler.Comments, http.MethodGet, "1", "/comments?limit=1&offset=1", "")
	var comments []Comment
	if err := json.Unmarshal(response.Body.Bytes(), &comments); err != nil || response.Code != http.StatusOK || len(comments) != 1 || comments[0].Content != "Second" {
		t.Fatalf("unexpected comments page: %s", response.Body.String())
	}

	for _, query := range []string{"limit=0", "limit=101", "limit=", "limit=bad", "offset=-1", "offset=", "offset=1&offset=2", "team_id=10"} {
		calls := repository.commentReadCalls
		response = serveTaskHandler(t, 1, handler.Comments, http.MethodGet, "1", "/comments?"+query, "")
		if response.Code != http.StatusBadRequest || repository.commentReadCalls != calls {
			t.Fatalf("invalid comments query accepted: %s", query)
		}
	}
}

func TestCommentHTTPDoesNotExposeDatabaseErrors(t *testing.T) {
	repository, _ := updateFixture(t)
	repository.commentErr = errors.New("private database details")
	repository.commentsErr = repository.commentErr
	handler := NewHandler(NewService(repository))
	for _, operation := range []struct {
		method  string
		handler http.HandlerFunc
	}{{http.MethodPost, handler.CreateComment}, {http.MethodGet, handler.Comments}} {
		response := serveTaskHandler(t, 1, operation.handler, operation.method, "1", "/comments", `{"content":"Comment"}`)
		if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "private") {
			t.Fatalf("database details exposed: %s", response.Body.String())
		}
	}
}
