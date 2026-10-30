package stats

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/skyfall620/task-manager/internal/auth"
)

func serveAuthenticated(t *testing.T, handler *Handler, actor uint64, teamID string) *httptest.ResponseRecorder {
	t.Helper()
	tokens, err := auth.NewTokenManager(strings.Repeat("a", 32), "test", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	token, err := tokens.Issue(actor)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/teams/"+teamID+"/stats", nil)
	request.SetPathValue("team_id", teamID)
	request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	response := httptest.NewRecorder()
	tokens.Middleware(http.HandlerFunc(handler.Get)).ServeHTTP(response, request)
	return response
}

func TestReportHTTPContract(t *testing.T) {
	for _, empty := range []bool{false, true} {
		repository := newRepositoryFake()
		wantJSON := `{"team_id":10,"tasks_by_status":{"todo":0,"in_progress":0,"done":0},"top_assignees":[],"average_close_time_seconds":null,"comments_count":0}`
		if !empty {
			average := 3600.5
			repository.result = Report{
				TasksByStatus:           TaskCounts{Todo: 5, InProgress: 3, Done: 7},
				TopAssignees:            []AssigneeStats{{UserID: 2, Name: "Alice", ClosedTasks: 4}},
				AverageCloseTimeSeconds: &average, CommentsCount: 12,
			}
			wantJSON = `{"team_id":10,"tasks_by_status":{"todo":5,"in_progress":3,"done":7},"top_assignees":[{"user_id":2,"name":"Alice","closed_tasks":4}],"average_close_time_seconds":3600.5,"comments_count":12}`
		}

		response := serveAuthenticated(t, NewHandler(NewService(repository)), 1, "10")
		var got, want any
		if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}

		if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
			t.Fatal(err)
		}

		if response.Code != http.StatusOK || !reflect.DeepEqual(got, want) {
			t.Fatalf("JSON contract mismatch: status=%d, body=%s", response.Code, response.Body.String())
		}

		if response.Header().Get("Content-Type") != "application/json" || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("report response headers differ from existing API conventions")
		}
	}
}

func TestReportHTTPAccessAndErrors(t *testing.T) {
	for _, test := range []struct {
		name         string
		actor        uint64
		teamID       string
		storageError bool
		status       int
		code         string
	}{
		{"admin", 2, "10", false, http.StatusOK, ""},
		{"member", 3, "10", false, http.StatusForbidden, "forbidden"},
		{"outsider", 99, "10", false, http.StatusNotFound, "team_not_found"},
		{"missing team", 1, "99", false, http.StatusNotFound, "team_not_found"},
		{"zero ID", 1, "0", false, http.StatusBadRequest, "invalid_input"},
		{"negative ID", 1, "-1", false, http.StatusBadRequest, "invalid_input"},
		{"invalid ID", 1, "bad", false, http.StatusBadRequest, "invalid_input"},
		{"overflow ID", 1, "18446744073709551616", false, http.StatusBadRequest, "invalid_input"},
		{"storage failure", 1, "10", true, http.StatusInternalServerError, "internal_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := newRepositoryFake()
			if test.storageError {
				repository.reportErr = errors.New("private database details")
			}

			response := serveAuthenticated(t, NewHandler(NewService(repository)), test.actor, test.teamID)
			if response.Code != test.status || strings.Contains(response.Body.String(), "private") {
				t.Fatalf("unexpected response: status=%d, body=%s", response.Code, response.Body.String())
			}

			if test.code != "" {
				var body errorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != test.code {
					t.Fatalf("unexpected error envelope: %s", response.Body.String())
				}
			}

			if test.status != http.StatusOK && !test.storageError && repository.reportCalls != 0 {
				t.Fatal("invalid or unauthorized HTTP request reached report query")
			}
		})
	}
}

func TestReportHandlerWithoutIdentity(t *testing.T) {
	repository := newRepositoryFake()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/teams/10/stats", nil)
	request.SetPathValue("team_id", "10")
	NewHandler(NewService(repository)).Get(response, request)
	if response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") != "Bearer" || repository.transactions != 0 {
		t.Fatal("handler accepted request without authenticated identity")
	}
}
