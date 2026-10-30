package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/skyfall620/task-manager/internal/auth"
	"github.com/skyfall620/task-manager/internal/config"
	"github.com/skyfall620/task-manager/internal/stats"
	"github.com/skyfall620/task-manager/internal/tasks"
	"github.com/skyfall620/task-manager/internal/teams"
)

func TestTeamRoutesRequireJWT(t *testing.T) {
	tokens, err := auth.NewTokenManager(strings.Repeat("a", 32), "test", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// Nil services must never be reached by these unauthenticated requests.
	server := NewServer(config.Config{}, auth.NewHandler(nil), teams.NewHandler(nil), tasks.NewHandler(nil), stats.NewHandler(nil), tokens)
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/teams"},
		{http.MethodGet, "/api/v1/teams"},
		{http.MethodGet, "/api/v1/teams/10/stats"},
		{http.MethodPost, "/api/v1/teams/10/invite"},
		{http.MethodPatch, "/api/v1/teams/10/members/4"},
		{http.MethodDelete, "/api/v1/teams/10/members/4"},
		{http.MethodPost, "/api/v1/tasks"},
		{http.MethodGet, "/api/v1/tasks?team_id=10"},
		{http.MethodPut, "/api/v1/tasks/1"},
		{http.MethodGet, "/api/v1/tasks/1/history"},
		{http.MethodPost, "/api/v1/tasks/1/comments"},
		{http.MethodGet, "/api/v1/tasks/1/comments"},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			for _, header := range []string{"", "Bearer invalid"} {
				request := httptest.NewRequest(route.method, route.path, nil)
				if header != "" {
					request.Header.Set("Authorization", header)
				}

				response := httptest.NewRecorder()
				server.Handler.ServeHTTP(response, request)
				if response.Code != http.StatusUnauthorized {
					t.Fatalf("status=%d; want 401", response.Code)
				}
			}
		})
	}

	for _, route := range []struct {
		method string
		path   string
		status int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodPost, "/api/v1/register", http.StatusUnsupportedMediaType},
		{http.MethodPost, "/api/v1/login", http.StatusUnsupportedMediaType},
	} {
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, httptest.NewRequest(route.method, route.path, nil))
		if response.Code != route.status {
			t.Errorf("public route %s: status=%d; want %d", route.path, response.Code, route.status)
		}
	}
}
