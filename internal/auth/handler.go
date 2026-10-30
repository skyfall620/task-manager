package auth

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
)

type Handler struct {
	service *Service
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var input RegisterInput
	if !decodeRequest(w, r, &input) {
		return
	}

	user, err := h.service.Register(r.Context(), input)
	if err != nil {
		handleServiceError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusCreated, user)
}

func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var input LoginInput
	if !decodeRequest(w, r, &input) {
		return
	}

	token, err := h.service.Login(r.Context(), input)
	if err != nil {
		handleServiceError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, token)
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
	case errors.Is(err, ErrInvalidInput):
		writeError(w, r, http.StatusBadRequest, "invalid_input", err.Error())
	case errors.Is(err, ErrEmailTaken):
		writeError(w, r, http.StatusConflict, "email_taken", "email already registered")
	case errors.Is(err, ErrInvalidCredentials):
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, r, http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
	default:
		// Database errors can contain submitted values; do not log their text.
		slog.ErrorContext(r.Context(), "authentication operation failed", "path", r.URL.Path)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, r, status, errorResponse{Error: errorBody{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.ErrorContext(r.Context(), "write authentication response", "error", err)
	}
}
