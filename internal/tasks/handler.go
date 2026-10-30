package tasks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
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

	var input CreateInput
	if !decodeRequest(w, r, &input) {
		return
	}

	task, err := h.service.Create(r.Context(), actorID, input)
	if err != nil {
		handleServiceError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusCreated, task)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requestActor(w, r)
	if !ok {
		return
	}

	filter, err := parseListFilter(r.URL.RawQuery)
	if err != nil {
		handleServiceError(w, r, err)
		return
	}

	tasks, err := h.service.List(r.Context(), actorID, filter)
	if err != nil {
		handleServiceError(w, r, err)
		return
	}

	if filter.cursorMode && len(tasks) > filter.Limit {
		tasks = tasks[:filter.Limit]
		w.Header().Set("X-Next-Cursor", encodeListCursor(tasks[len(tasks)-1]))
	}

	writeJSON(w, r, http.StatusOK, tasks)
}

func parseListFilter(rawQuery string) (ListFilter, error) {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return ListFilter{}, fmt.Errorf("%w: malformed query string", ErrInvalidInput)
	}

	if err := validateListQuery(values); err != nil {
		return ListFilter{}, err
	}

	teamID, err := positiveID(values.Get("team_id"))
	if err != nil {
		return ListFilter{}, fmt.Errorf("%w: positive team_id required", ErrInvalidInput)
	}

	filter := ListFilter{TeamID: teamID, Status: Status(values.Get("status")), Limit: DefaultLimit, cursorMode: !values.Has("offset")}
	if values.Has("cursor") {
		if values.Has("offset") {
			return ListFilter{}, fmt.Errorf("%w: cursor and offset cannot be combined", ErrInvalidInput)
		}

		filter.Cursor, err = decodeListCursor(values.Get("cursor"))
		if err != nil {
			return ListFilter{}, err
		}
	}

	if values.Has("assignee_id") {
		id, err := positiveID(values.Get("assignee_id"))
		if err != nil {
			return ListFilter{}, fmt.Errorf("%w: assignee_id must be positive", ErrInvalidInput)
		}

		filter.AssigneeID = &id
	}

	if values.Has("limit") {
		filter.Limit, err = strconv.Atoi(values.Get("limit"))
		if err != nil || filter.Limit < 1 || filter.Limit > MaxLimit {
			return ListFilter{}, fmt.Errorf("%w: limit must be 1 to 100", ErrInvalidInput)
		}
	}

	if values.Has("offset") {
		filter.Offset, err = strconv.Atoi(values.Get("offset"))
		if err != nil || filter.Offset < 0 {
			return ListFilter{}, fmt.Errorf("%w: offset must be nonnegative", ErrInvalidInput)
		}
	}

	return filter, nil
}

func validateListQuery(values url.Values) error {
	for key, entries := range values {
		switch key {
		case "team_id", "status", "assignee_id", "limit", "offset", "cursor":
		default:
			return fmt.Errorf("%w: unsupported query parameter", ErrInvalidInput)
		}

		if len(entries) != 1 || entries[0] == "" {
			return fmt.Errorf("%w: query parameters must have one nonempty value", ErrInvalidInput)
		}
	}

	return nil
}

func positiveID(value string) (uint64, error) {
	id, err := strconv.ParseUint(value, 10, 64)
	if err != nil || id == 0 {
		return 0, ErrInvalidInput
	}

	return id, nil
}

func requestActor(w http.ResponseWriter, r *http.Request) (uint64, bool) {
	id, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		handleServiceError(w, r, ErrUnauthenticated)
	}

	return id, ok
}

func decodeRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}

	// A TEXT value can occupy 65535 bytes, with additional JSON escaping.
	r.Body = http.MaxBytesReader(w, r.Body, 512*1024)
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
		writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "request body exceeds 512 KiB")
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
	case errors.Is(err, ErrInvalidAssignee):
		writeError(w, r, http.StatusBadRequest, "invalid_assignee", "assignee must belong to team")
	case errors.Is(err, ErrTaskNotFound):
		writeError(w, r, http.StatusNotFound, "task_not_found", "task not found")
	case errors.Is(err, ErrVersionConflict):
		writeError(w, r, http.StatusConflict, "version_conflict", "task version is outdated")
	case errors.Is(err, ErrTeamNotFound):
		writeError(w, r, http.StatusNotFound, "team_not_found", "team not found")
	case errors.Is(err, ErrForbidden):
		writeError(w, r, http.StatusForbidden, "forbidden", "insufficient permissions")
	default:
		// SQL errors can contain task content; do not log their text.
		slog.ErrorContext(r.Context(), "task operation failed", "path", r.URL.Path)
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
		slog.ErrorContext(r.Context(), "write task response", "error", err)
	}
}
