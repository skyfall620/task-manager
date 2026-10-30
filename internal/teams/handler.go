package teams

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"

	"github.com/skyfall620/task-manager/internal/auth"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requestActor(w, r)
	if !ok {
		return
	}

	var input struct {
		Name string `json:"name"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}

	team, err := h.service.Create(r.Context(), actorID, input.Name)
	if err != nil {
		handleServiceError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusCreated, team)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requestActor(w, r)
	if !ok {
		return
	}

	teams, err := h.service.List(r.Context(), actorID)
	if err != nil {
		handleServiceError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, teams)
}

func (h *Handler) Invite(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requestActor(w, r)
	if !ok {
		return
	}

	teamID, ok := pathID(w, r, "id")
	if !ok {
		return
	}

	var input struct {
		Email string `json:"email"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}

	member, err := h.service.Invite(r.Context(), actorID, teamID, input.Email)
	if err != nil {
		handleServiceError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusCreated, member)
}

func (h *Handler) ChangeRole(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requestActor(w, r)
	if !ok {
		return
	}

	teamID, userID, ok := memberPathIDs(w, r)
	if !ok {
		return
	}

	var input struct {
		Role Role `json:"role"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}

	member, err := h.service.ChangeRole(r.Context(), actorID, teamID, userID, input.Role)
	if err != nil {
		handleServiceError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, member)
}

func (h *Handler) Remove(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requestActor(w, r)
	if !ok {
		return
	}

	teamID, userID, ok := memberPathIDs(w, r)
	if !ok {
		return
	}

	if err := h.service.Remove(r.Context(), actorID, teamID, userID); err != nil {
		handleServiceError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func requestActor(w http.ResponseWriter, r *http.Request) (uint64, bool) {
	id, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		handleServiceError(w, r, ErrUnauthenticated)
	}

	return id, ok
}

func pathID(w http.ResponseWriter, r *http.Request, name string) (uint64, bool) {
	id, err := strconv.ParseUint(r.PathValue(name), 10, 64)
	if err != nil || id == 0 {
		writeError(w, r, http.StatusBadRequest, "invalid_input", "path IDs must be positive integers")
		return 0, false
	}

	return id, true
}

func memberPathIDs(w http.ResponseWriter, r *http.Request) (uint64, uint64, bool) {
	teamID, ok := pathID(w, r, "id")
	if !ok {
		return 0, 0, false
	}

	userID, ok := pathID(w, r, "user_id")
	return teamID, userID, ok
}

func decodeRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}

	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeDecodeError(w, r, err)
		return false
	}

	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeDecodeError(w, r, err)
		return false
	}

	return true
}

func writeDecodeError(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "request body exceeds 16 KiB")
		return
	}

	writeError(w, r, http.StatusBadRequest, "invalid_request", "request body must be one JSON object with supported fields")
}

func handleServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrUnauthenticated):
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, r, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
	case errors.Is(err, ErrInvalidInput):
		writeError(w, r, http.StatusBadRequest, "invalid_input", err.Error())
	case errors.Is(err, ErrTeamNotFound):
		writeError(w, r, http.StatusNotFound, "team_not_found", "team not found")
	case errors.Is(err, ErrMemberNotFound):
		writeError(w, r, http.StatusNotFound, "member_not_found", "member not found")
	case errors.Is(err, ErrUserNotFound):
		writeError(w, r, http.StatusNotFound, "user_not_found", "user not found")
	case errors.Is(err, ErrForbidden):
		writeError(w, r, http.StatusForbidden, "forbidden", "insufficient permissions")
	case errors.Is(err, ErrAlreadyMember):
		writeError(w, r, http.StatusConflict, "already_member", "user already belongs to team")
	case errors.Is(err, ErrAssignedTasks):
		writeError(w, r, http.StatusConflict, "assigned_tasks", "reassign member tasks before removal")
	default:
		// SQL errors may contain submitted data; keep their text out of logs.
		slog.ErrorContext(r.Context(), "team operation failed", "path", r.URL.Path)
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
		slog.ErrorContext(r.Context(), "write team response", "error", err)
	}
}
