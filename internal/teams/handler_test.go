package teams

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

func serveAuthenticated(t *testing.T, actor uint64, handler http.HandlerFunc, method, body, teamID, userID string) *httptest.ResponseRecorder {
	t.Helper()
	tokens, err := auth.NewTokenManager(strings.Repeat("a", 32), "test", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	token, err := tokens.Issue(actor)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(method, "/api/v1/teams", strings.NewReader(body))
	request.SetPathValue("id", teamID)
	request.SetPathValue("user_id", userID)
	request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	tokens.Middleware(handler).ServeHTTP(response, request)
	return response
}

func TestTeamHTTPRoleModel(t *testing.T) {
	for _, test := range []struct {
		name      string
		operation string
		actor     uint64
		target    string
		body      string
		assigned  bool
		status    int
		code      string
	}{
		{"owner invites", "invite", 1, "", `{"email":"new@example.com"}`, false, http.StatusCreated, ""},
		{"admin invites", "invite", 2, "", `{"email":"new@example.com"}`, false, http.StatusCreated, ""},
		{"member cannot invite", "invite", 3, "", `{"email":"new@example.com"}`, false, http.StatusForbidden, "forbidden"},
		{"outsider", "invite", 8, "", `{"email":"new@example.com"}`, false, http.StatusNotFound, "team_not_found"},
		{"duplicate invitation", "invite", 1, "", `{"email":"member@example.com"}`, false, http.StatusConflict, "already_member"},
		{"unknown user", "invite", 1, "", `{"email":"unknown@example.com"}`, false, http.StatusNotFound, "user_not_found"},
		{"owner changes role", "role", 1, "4", `{"role":"admin"}`, false, http.StatusOK, ""},
		{"admin cannot change role", "role", 2, "4", `{"role":"admin"}`, false, http.StatusForbidden, "forbidden"},
		{"cannot change owner", "role", 1, "1", `{"role":"member"}`, false, http.StatusForbidden, "forbidden"},
		{"cannot create second owner", "role", 1, "4", `{"role":"owner"}`, false, http.StatusBadRequest, "invalid_input"},
		{"unknown target", "role", 1, "99", `{"role":"admin"}`, false, http.StatusNotFound, "member_not_found"},
		{"admin removes", "remove", 2, "4", "", false, http.StatusNoContent, ""},
		{"assigned member", "remove", 1, "4", "", true, http.StatusConflict, "assigned_tasks"},
		{"cannot remove owner", "remove", 2, "1", "", false, http.StatusForbidden, "forbidden"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := newRepositoryFake()
			repository.assigned[4] = test.assigned
			handler := NewHandler(NewService(repository))
			method, operation := http.MethodPost, http.HandlerFunc(handler.Invite)
			switch test.operation {
			case "role":
				method, operation = http.MethodPatch, handler.ChangeRole
			case "remove":
				method, operation = http.MethodDelete, handler.Remove
			}

			response := serveAuthenticated(t, test.actor, operation, method, test.body, "10", test.target)
			if response.Code != test.status {
				t.Fatalf("status=%d, want %d; body=%s", response.Code, test.status, response.Body.String())
			}

			if test.code != "" {
				var body errorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != test.code {
					t.Fatalf("unexpected error response: %s", response.Body.String())
				}
			} else if test.status == http.StatusNoContent {
				if response.Body.Len() != 0 {
					t.Error("204 response contains a body")
				}
			} else {
				var member Member
				if err := json.Unmarshal(response.Body.Bytes(), &member); err != nil || member.TeamID != 10 || member.UserID == 0 {
					t.Fatalf("unexpected membership response: %s", response.Body.String())
				}
			}
		})
	}
}

func TestCreateAndListHTTP(t *testing.T) {
	repository := newRepositoryFake()
	handler := NewHandler(NewService(repository))
	response := serveAuthenticated(t, 7, handler.Create, http.MethodPost, `{"name":"New team"}`, "", "")
	var team Team
	if err := json.Unmarshal(response.Body.Bytes(), &team); err != nil || response.Code != http.StatusCreated || team.CreatedBy != 7 {
		t.Fatalf("unexpected create response: status=%d, body=%s", response.Code, response.Body.String())
	}

	response = serveAuthenticated(t, 7, handler.List, http.MethodGet, "", "", "")
	var list []Team
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil || response.Code != http.StatusOK || len(list) != 1 || list[0].ID != team.ID {
		t.Fatalf("unexpected list response: %s", response.Body.String())
	}

	response = serveAuthenticated(t, 8, handler.List, http.MethodGet, "", "", "")
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "[]" {
		t.Fatalf("unexpected outsider list: %s", response.Body.String())
	}
}

func TestTeamHTTPInvalidRequests(t *testing.T) {
	for _, test := range []struct {
		name   string
		body   string
		teamID string
		status int
	}{
		{"zero ID", `{"email":"new@example.com"}`, "0", http.StatusBadRequest},
		{"invalid ID", `{"email":"new@example.com"}`, "bad", http.StatusBadRequest},
		{"overflow ID", `{"email":"new@example.com"}`, "18446744073709551616", http.StatusBadRequest},
		{"broken JSON", `{`, "10", http.StatusBadRequest},
		{"missing email", `{}`, "10", http.StatusBadRequest},
		{"role in invitation", `{"email":"new@example.com","role":"admin"}`, "10", http.StatusBadRequest},
		{"multiple JSON values", `{} {}`, "10", http.StatusBadRequest},
		{"large body", `{"email":"` + strings.Repeat("a", 17*1024) + `"}`, "10", http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := newRepositoryFake()
			response := serveAuthenticated(t, 1, NewHandler(NewService(repository)).Invite, http.MethodPost, test.body, test.teamID, "")
			if response.Code != test.status || repository.writes != 0 {
				t.Fatalf("status=%d, writes=%d; want %d, 0", response.Code, repository.writes, test.status)
			}
		})
	}

	repository := newRepositoryFake()
	response := serveAuthenticated(t, 1, NewHandler(NewService(repository)).Create, http.MethodPost, `{"name":"Team","created_by":99}`, "", "")
	if response.Code != http.StatusBadRequest || repository.writes != 0 {
		t.Fatal("client could supply team creator")
	}
}

func TestTeamHTTPHidesDatabaseErrors(t *testing.T) {
	repository := newRepositoryFake()
	repository.transactionErr = errors.New("private database details")
	response := serveAuthenticated(t, 1, NewHandler(NewService(repository)).Remove, http.MethodDelete, "", "10", "4")
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "private") {
		t.Fatalf("unexpected database error response: %s", response.Body.String())
	}
}
