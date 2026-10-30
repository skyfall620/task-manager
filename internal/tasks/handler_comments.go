package tasks

import "net/http"

func (h *Handler) CreateComment(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requestActor(w, r)
	if !ok {
		return
	}

	taskID, ok := requestTaskID(w, r)
	if !ok {
		return
	}

	var input CreateCommentInput
	if !decodeRequest(w, r, &input) {
		return
	}

	comment, err := h.service.CreateComment(r.Context(), actorID, taskID, input)
	if err != nil {
		handleServiceError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusCreated, comment)
}

func (h *Handler) Comments(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requestActor(w, r)
	if !ok {
		return
	}

	taskID, ok := requestTaskID(w, r)
	if !ok {
		return
	}

	// Comments use the same pagination contract as task history.
	page, err := parseHistoryFilter(r.URL.RawQuery)
	if err != nil {
		handleServiceError(w, r, err)
		return
	}

	comments, err := h.service.Comments(r.Context(), actorID, taskID, CommentsFilter(page))
	if err != nil {
		handleServiceError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, comments)
}
