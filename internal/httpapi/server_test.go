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
	cfg := config.Config{RateLimitRPS: 10, RateLimitBurst: 100}
	server := NewServer(cfg, auth.NewHandler(nil), teams.NewHandler(nil), tasks.NewHandler(nil), stats.NewHandler(nil), tokens)
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

func TestServerRateLimiting(t *testing.T) {
	tokens, err := auth.NewTokenManager(strings.Repeat("a", 32), "test", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{RateLimitRPS: 10, RateLimitBurst: 2}
	server := NewServer(cfg, auth.NewHandler(nil), teams.NewHandler(nil), tasks.NewHandler(nil), stats.NewHandler(nil), tokens)
	limiter := server.Handler.(*rateLimiter)
	base := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	now := base
	limiter.now = func() time.Time { return now }

	for _, test := range []struct {
		name    string
		method  string
		path    string
		elapsed time.Duration
		status  int
	}{
		{"health before requests", http.MethodGet, "/healthz", 0, http.StatusOK},
		{"health does not consume quota", http.MethodGet, "/healthz", 0, http.StatusOK},
		{"register first token", http.MethodPost, "/api/v1/register", 0, http.StatusUnsupportedMediaType},
		{"login shares quota", http.MethodPost, "/api/v1/login", 0, http.StatusUnsupportedMediaType},
		{"limit before JWT", http.MethodGet, "/api/v1/teams", 0, http.StatusTooManyRequests},
		{"limit before mux", http.MethodGet, "/unknown", 0, http.StatusTooManyRequests},
		{"only exact health path excluded", http.MethodGet, "/healthz/", 0, http.StatusTooManyRequests},
		{"health after exhaustion", http.MethodGet, "/healthz", 0, http.StatusOK},
		{"health POST unchanged", http.MethodPost, "/healthz", 0, http.StatusMethodNotAllowed},
		{"health HEAD unchanged", http.MethodHead, "/healthz", 0, http.StatusMethodNotAllowed},
		{"JWT response after refill", http.MethodGet, "/api/v1/teams", 100 * time.Millisecond, http.StatusUnauthorized},
		{"router 404 after refill", http.MethodGet, "/unknown", 200 * time.Millisecond, http.StatusNotFound},
		{"router 405 after refill", http.MethodDelete, "/api/v1/teams", 300 * time.Millisecond, http.StatusMethodNotAllowed},
	} {
		t.Run(test.name, func(t *testing.T) {
			now = base.Add(test.elapsed)
			request := httptest.NewRequest(test.method, test.path, nil)
			request.RemoteAddr = "192.0.2.1:1000"
			response := httptest.NewRecorder()
			server.Handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d; want %d", response.Code, test.status)
			}

			if (test.status == http.StatusNotFound || test.status == http.StatusMethodNotAllowed) &&
				!strings.HasPrefix(response.Header().Get("Content-Type"), "text/plain") {
				t.Fatal("router or health error is no longer text/plain")
			}
		})
	}
}
