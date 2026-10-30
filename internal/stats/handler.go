package stats

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/skyfall620/task-manager/internal/auth"
	"github.com/skyfall620/task-manager/internal/teams"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	actorID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		handleServiceError(w, r, teams.ErrUnauthenticated)
		return
	}

	teamID, err := strconv.ParseUint(r.PathValue("team_id"), 10, 64)
	if err != nil || teamID == 0 {
		writeError(w, r, http.StatusBadRequest, "invalid_input", "team_id must be a positive integer")
		return
	}

	report, err := h.service.Report(r.Context(), actorID, teamID)
	if err != nil {
		handleServiceError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, report)
}

func handleServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, teams.ErrUnauthenticated):
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, r, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
	case errors.Is(err, teams.ErrInvalidInput):
		writeError(w, r, http.StatusBadRequest, "invalid_input", "team_id must be a positive integer")
	case errors.Is(err, teams.ErrTeamNotFound):
		writeError(w, r, http.StatusNotFound, "team_not_found", "team not found")
	case errors.Is(err, teams.ErrForbidden):
		writeError(w, r, http.StatusForbidden, "forbidden", "insufficient permissions")
	default:
		// SQL errors can contain data; use the same safe logging policy as teams.
		slog.ErrorContext(r.Context(), "stats operation failed", "path", r.URL.Path)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, r, status, errorResponse{Error: errorBody{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.ErrorContext(r.Context(), "write stats response", "error", err)
	}
}
