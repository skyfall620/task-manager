package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/skyfall620/task-manager/internal/auth"
	"github.com/skyfall620/task-manager/internal/config"
	"github.com/skyfall620/task-manager/internal/stats"
	"github.com/skyfall620/task-manager/internal/tasks"
	"github.com/skyfall620/task-manager/internal/teams"
)

func NewServer(cfg config.Config, authHandler *auth.Handler, teamHandler *teams.Handler, taskHandler *tasks.Handler, statsHandler *stats.Handler, tokens *auth.TokenManager) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", health)
	mux.HandleFunc("POST /api/v1/register", authHandler.Register)
	mux.HandleFunc("POST /api/v1/login", authHandler.Login)
	mux.Handle("POST /api/v1/teams", tokens.Middleware(http.HandlerFunc(teamHandler.Create)))
	mux.Handle("GET /api/v1/teams", tokens.Middleware(http.HandlerFunc(teamHandler.List)))
	mux.Handle("GET /api/v1/teams/{team_id}/stats", tokens.Middleware(http.HandlerFunc(statsHandler.Get)))
	mux.Handle("POST /api/v1/teams/{id}/invite", tokens.Middleware(http.HandlerFunc(teamHandler.Invite)))
	mux.Handle("PATCH /api/v1/teams/{id}/members/{user_id}", tokens.Middleware(http.HandlerFunc(teamHandler.ChangeRole)))
	mux.Handle("DELETE /api/v1/teams/{id}/members/{user_id}", tokens.Middleware(http.HandlerFunc(teamHandler.Remove)))
	mux.Handle("POST /api/v1/tasks", tokens.Middleware(http.HandlerFunc(taskHandler.Create)))
	mux.Handle("GET /api/v1/tasks", tokens.Middleware(http.HandlerFunc(taskHandler.List)))
	mux.Handle("PUT /api/v1/tasks/{id}", tokens.Middleware(http.HandlerFunc(taskHandler.Update)))
	mux.Handle("GET /api/v1/tasks/{id}/history", tokens.Middleware(http.HandlerFunc(taskHandler.History)))
	mux.Handle("POST /api/v1/tasks/{id}/comments", tokens.Middleware(http.HandlerFunc(taskHandler.CreateComment)))
	mux.Handle("GET /api/v1/tasks/{id}/comments", tokens.Middleware(http.HandlerFunc(taskHandler.Comments)))

	return &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           newRateLimiter(mux, cfg.RateLimitRPS, cfg.RateLimitBurst),
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}
}

func health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
}
