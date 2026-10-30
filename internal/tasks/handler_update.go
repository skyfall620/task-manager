package tasks

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

type updateRequest struct {
	Title       *string         `json:"title"`
	Description *string         `json:"description"`
	Status      *Status         `json:"status"`
	AssigneeID  json.RawMessage `json:"assignee_id"`
	Version     *uint64         `json:"version"`
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requestActor(w, r)
	if !ok {
		return
	}

	taskID, ok := requestTaskID(w, r)
	if !ok {
		return
	}

	var body updateRequest
	if !decodeRequest(w, r, &body) {
		return
	}

	input, err := body.input()
	if err != nil {
		handleServiceError(w, r, err)
		return
	}

	task, err := h.service.Update(r.Context(), actorID, taskID, input)
	if err != nil {
		handleServiceError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, task)
}

func (h *Handler) History(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requestActor(w, r)
	if !ok {
		return
	}

	taskID, ok := requestTaskID(w, r)
	if !ok {
		return
	}

	filter, err := parseHistoryFilter(r.URL.RawQuery)
	if err != nil {
		handleServiceError(w, r, err)
		return
	}

	history, err := h.service.History(r.Context(), actorID, taskID, filter)
	if err != nil {
		handleServiceError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, history)
}

func (r updateRequest) input() (UpdateInput, error) {
	if r.Title == nil || r.Description == nil || r.Status == nil || r.Version == nil || len(r.AssigneeID) == 0 {
		return UpdateInput{}, fmt.Errorf("%w: title, description, status, assignee_id and version are required", ErrInvalidInput)
	}

	var assigneeID *uint64
	if err := json.Unmarshal(r.AssigneeID, &assigneeID); err != nil {
		return UpdateInput{}, fmt.Errorf("%w: assignee_id must be a positive integer or null", ErrInvalidInput)
	}

	return UpdateInput{
		Title: *r.Title, Description: *r.Description, Status: *r.Status,
		AssigneeID: assigneeID, Version: *r.Version,
	}, nil
}

func requestTaskID(w http.ResponseWriter, r *http.Request) (uint64, bool) {
	id, err := positiveID(r.PathValue("id"))
	if err != nil {
		handleServiceError(w, r, fmt.Errorf("%w: positive task ID required", ErrInvalidInput))
		return 0, false
	}

	return id, true
}

func parseHistoryFilter(rawQuery string) (HistoryFilter, error) {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return HistoryFilter{}, fmt.Errorf("%w: malformed query string", ErrInvalidInput)
	}

	filter := HistoryFilter{Limit: DefaultLimit}
	for key, entries := range values {
		if (key != "limit" && key != "offset") || len(entries) != 1 || entries[0] == "" {
			return HistoryFilter{}, fmt.Errorf("%w: only single nonempty limit and offset parameters are supported", ErrInvalidInput)
		}

		value, err := strconv.Atoi(entries[0])
		if err != nil {
			return HistoryFilter{}, fmt.Errorf("%w: pagination values must be integers", ErrInvalidInput)
		}

		if key == "limit" {
			filter.Limit = value
		} else {
			filter.Offset = value
		}
	}

	if filter.Limit < 1 || filter.Limit > MaxLimit || filter.Offset < 0 {
		return HistoryFilter{}, fmt.Errorf("%w: limit must be 1 to 100 and offset must be nonnegative", ErrInvalidInput)
	}

	return filter, nil
}
