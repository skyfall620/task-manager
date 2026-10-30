package tasks

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/skyfall620/task-manager/internal/auth"
)

func serveAuthenticated(t *testing.T, actor uint64, handler http.HandlerFunc, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	tokens, err := auth.NewTokenManager(strings.Repeat("a", 32), "test", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	token, err := tokens.Issue(actor)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	response := httptest.NewRecorder()
	tokens.Middleware(handler).ServeHTTP(response, request)
	return response
}

func TestCreateHTTP(t *testing.T) {
	repository := newRepositoryFake()
	handler := NewHandler(NewService(repository))
	response := serveAuthenticated(t, 3, handler.Create, http.MethodPost, "/api/v1/tasks",
		`{"team_id":10,"title":"Task","description":"Text","assignee_id":4}`)
	var task Task
	if err := json.Unmarshal(response.Body.Bytes(), &task); err != nil || response.Code != http.StatusCreated {
		t.Fatalf("unexpected create response: %d %s", response.Code, response.Body.String())
	}

	if task.CreatedBy != 3 || task.Version != 1 || task.Status != StatusTodo || task.AssigneeID == nil || *task.AssigneeID != 4 {
		t.Fatalf("unexpected server fields: %+v", task)
	}

	if len(repository.history) != 1 {
		t.Fatal("initial history missing")
	}

	response = serveAuthenticated(t, 1, handler.Create, http.MethodPost, "/api/v1/tasks",
		`{"team_id":10,"title":"Unassigned","assignee_id":null,"status":"done"}`)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil || response.Code != http.StatusCreated {
		t.Fatalf("unexpected unassigned response: %s", response.Body.String())
	}

	if string(fields["assignee_id"]) != "null" || string(fields["closed_at"]) == "null" {
		t.Fatal("invalid nullable task fields")
	}
}

func TestCreateRejectsServerFields(t *testing.T) {
	for _, field := range []string{"id", "created_by", "created_at", "updated_at", "closed_at", "version"} {
		t.Run(field, func(t *testing.T) {
			repository := newRepositoryFake()
			body := `{"team_id":10,"title":"Task","` + field + `":1}`
			response := serveAuthenticated(t, 1, NewHandler(NewService(repository)).Create, http.MethodPost, "/api/v1/tasks", body)
			if response.Code != http.StatusBadRequest || repository.createCalls != 0 {
				t.Fatalf("server field accepted: status=%d", response.Code)
			}
		})
	}
}

func TestCreateHTTPAccessAndErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		actor  uint64
		body   string
		status int
		code   string
	}{
		{"outsider", 99, `{"team_id":10,"title":"Task"}`, http.StatusNotFound, "team_not_found"},
		{"missing team", 1, `{"title":"Task"}`, http.StatusBadRequest, "invalid_input"},
		{"foreign assignee", 1, `{"team_id":10,"title":"Task","assignee_id":99}`, http.StatusBadRequest, "invalid_assignee"},
		{"invalid status", 1, `{"team_id":10,"title":"Task","status":"closed"}`, http.StatusBadRequest, "invalid_input"},
		{"malformed JSON", 1, `{`, http.StatusBadRequest, "invalid_request"},
		{"multiple JSON objects", 1, `{} {}`, http.StatusBadRequest, "invalid_request"},
		{"large body", 1, `{"description":"` + strings.Repeat("a", 513*1024) + `"}`, http.StatusRequestEntityTooLarge, "body_too_large"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := newRepositoryFake()
			response := serveAuthenticated(t, test.actor, NewHandler(NewService(repository)).Create, http.MethodPost, "/api/v1/tasks", test.body)
			var body errorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != test.status || body.Error.Code != test.code {
				t.Fatalf("unexpected error: %d %s", response.Code, response.Body.String())
			}

			if repository.createCalls != 0 {
				t.Fatal("invalid request attempted insert")
			}
		})
	}

	repository := newRepositoryFake()
	repository.historyErr = errors.New("private database error")
	response := serveAuthenticated(t, 1, NewHandler(NewService(repository)).Create, http.MethodPost, "/api/v1/tasks", `{"team_id":10,"title":"Task"}`)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "private") || len(repository.tasks) != 0 {
		t.Fatalf("unexpected history error response: %s", response.Body.String())
	}
}

func TestListHTTPFilters(t *testing.T) {
	repository := newRepositoryFake()
	repository.listResult = []Task{{ID: 7, TeamID: 10, Status: StatusInProgress}}
	handler := NewHandler(NewService(repository))
	response := serveAuthenticated(t, 3, handler.List, http.MethodGet,
		"/api/v1/tasks?team_id=10&status=in_progress&assignee_id=4&limit=2&offset=3", "")
	var result []Task
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != http.StatusOK || len(result) != 1 || result[0].ID != 7 {
		t.Fatalf("unexpected list: %s", response.Body.String())
	}

	filter := repository.lastFilter
	if filter.TeamID != 10 || filter.Status != StatusInProgress || filter.AssigneeID == nil || *filter.AssigneeID != 4 || filter.Limit != 2 || filter.Offset != 3 {
		t.Fatalf("unexpected filter: %+v", filter)
	}

	repository.listResult = nil
	response = serveAuthenticated(t, 3, handler.List, http.MethodGet, "/api/v1/tasks?team_id=10", "")
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "[]" || repository.lastFilter.Limit != DefaultLimit {
		t.Fatalf("unexpected empty list: %s", response.Body.String())
	}
}

func TestListHTTPRejectsInvalidFilters(t *testing.T) {
	for _, query := range []string{
		"", "team_id=", "team_id=0", "team_id=-1", "team_id=bad", "team_id=18446744073709551616",
		"team_id=10&team_id=20", "team_id=10&status=closed", "team_id=10&status=",
		"team_id=10&assignee_id=0", "team_id=10&assignee_id=bad", "team_id=10&assignee_id=",
		"team_id=10&limit=0", "team_id=10&limit=101", "team_id=10&limit=-1", "team_id=10&limit=bad",
		"team_id=10&offset=-1", "team_id=10&offset=bad", "team_id=10&limit=2&limit=3",
		"team_id=10&unknown=value", "team_id=10;status=todo",
	} {
		t.Run(query, func(t *testing.T) {
			repository := newRepositoryFake()
			response := serveAuthenticated(t, 1, NewHandler(NewService(repository)).List, http.MethodGet, "/api/v1/tasks?"+query, "")
			if response.Code != http.StatusBadRequest || repository.listCalls != 0 {
				t.Fatalf("invalid filter accepted: status=%d, calls=%d", response.Code, repository.listCalls)
			}
		})
	}

	for _, test := range []struct {
		actor uint64
		team  string
	}{{99, "10"}, {1, "99"}} {
		repository := newRepositoryFake()
		response := serveAuthenticated(t, test.actor, NewHandler(NewService(repository)).List, http.MethodGet, "/api/v1/tasks?team_id="+test.team, "")
		if response.Code != http.StatusNotFound || repository.listCalls != 0 {
			t.Fatal("foreign or missing team was exposed")
		}
	}
}
