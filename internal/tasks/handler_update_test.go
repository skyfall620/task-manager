package tasks

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serveTaskHandler(t *testing.T, actor uint64, handler http.HandlerFunc, method, id, query, body string) *httptest.ResponseRecorder {
	t.Helper()
	return serveAuthenticated(t, actor, func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("id", id)
		handler(w, r)
	}, method, "/api/v1/tasks/"+id+query, body)
}

func updateBody(t *testing.T, task Task) string {
	t.Helper()
	body, err := json.Marshal(inputForTask(task))
	if err != nil {
		t.Fatal(err)
	}

	return string(body)
}

func TestUpdateHTTPRequiredFields(t *testing.T) {
	for _, field := range []string{"title", "description", "status", "assignee_id", "version"} {
		t.Run("missing/"+field, func(t *testing.T) {
			repository, task := updateFixture(t)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(updateBody(t, task)), &fields); err != nil {
				t.Fatal(err)
			}

			delete(fields, field)
			body, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}

			response := serveTaskHandler(t, 1, NewHandler(NewService(repository)).Update, http.MethodPut, "1", "", string(body))
			if response.Code != http.StatusBadRequest || repository.updateCalls != 0 {
				t.Fatalf("missing %s accepted: %d %s", field, response.Code, response.Body.String())
			}
		})
	}

	for _, field := range []string{"title", "description", "status", "version"} {
		t.Run("null/"+field, func(t *testing.T) {
			repository, task := updateFixture(t)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(updateBody(t, task)), &fields); err != nil {
				t.Fatal(err)
			}

			fields[field] = json.RawMessage("null")
			body, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}

			response := serveTaskHandler(t, 1, NewHandler(NewService(repository)).Update, http.MethodPut, "1", "", string(body))
			if response.Code != http.StatusBadRequest || repository.updateCalls != 0 {
				t.Fatalf("null %s accepted: %s", field, response.Body.String())
			}
		})
	}
}

func TestUpdateHTTPRejectsImmutableFields(t *testing.T) {
	for _, field := range []string{"id", "team_id", "created_by", "created_at", "updated_at", "closed_at"} {
		t.Run(field, func(t *testing.T) {
			repository, task := updateFixture(t)
			body := strings.TrimSuffix(updateBody(t, task), "}") + `,"` + field + `":1}`
			response := serveTaskHandler(t, 1, NewHandler(NewService(repository)).Update, http.MethodPut, "1", "", body)
			if response.Code != http.StatusBadRequest || repository.updateCalls != 0 {
				t.Fatalf("immutable field %s accepted: %s", field, response.Body.String())
			}
		})
	}
}

func TestUpdateHTTPUnassignAndNoOp(t *testing.T) {
	repository, task := updateFixture(t)
	handler := NewHandler(NewService(repository))
	input := inputForTask(task)
	input.AssigneeID = nil
	input.Description = ""
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}

	response := serveTaskHandler(t, 3, handler.Update, http.MethodPut, "1", "", string(body))
	var updated Task
	if err := json.Unmarshal(response.Body.Bytes(), &updated); err != nil || response.Code != http.StatusOK {
		t.Fatalf("unexpected update: %d %s", response.Code, response.Body.String())
	}

	if updated.AssigneeID != nil || updated.Description != "" || updated.Version != 2 {
		t.Fatal("explicit null or empty description was not applied")
	}

	response = serveTaskHandler(t, 3, handler.Update, http.MethodPut, "1", "", updateBody(t, updated))
	if response.Code != http.StatusOK || repository.updateCalls != 1 || len(repository.history) != 2 {
		t.Fatal("no-op added an update or history")
	}
}

func TestUpdateHTTPErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		actor  uint64
		id     string
		change func(*UpdateInput)
		status int
		code   string
	}{
		{"stale version", 3, "1", func(i *UpdateInput) { i.Version++ }, http.StatusConflict, "version_conflict"},
		{"unrelated member", 5, "1", func(i *UpdateInput) { i.Status = StatusDone }, http.StatusForbidden, "forbidden"},
		{"assignee cannot rename", 4, "1", func(i *UpdateInput) { i.Title = "New" }, http.StatusForbidden, "forbidden"},
		{"outsider", 99, "1", func(*UpdateInput) {}, http.StatusNotFound, "task_not_found"},
		{"missing task", 1, "99", func(*UpdateInput) {}, http.StatusNotFound, "task_not_found"},
		{"invalid ID", 1, "0", func(*UpdateInput) {}, http.StatusBadRequest, "invalid_input"},
		{"zero version", 1, "1", func(i *UpdateInput) { i.Version = 0 }, http.StatusBadRequest, "invalid_input"},
		{"invalid status", 1, "1", func(i *UpdateInput) { i.Status = "closed" }, http.StatusBadRequest, "invalid_input"},
		{"foreign assignee", 1, "1", func(i *UpdateInput) { id := uint64(99); i.AssigneeID = &id }, http.StatusBadRequest, "invalid_assignee"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, task := updateFixture(t)
			input := inputForTask(task)
			test.change(&input)
			body, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}

			response := serveTaskHandler(t, test.actor, NewHandler(NewService(repository)).Update, http.MethodPut, test.id, "", string(body))
			var result errorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != test.status || result.Error.Code != test.code {
				t.Fatalf("unexpected error: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestUpdateHTTPInvalidScalarTypes(t *testing.T) {
	for _, value := range []string{`"4"`, `-1`, `1.5`, `{}`, `[]`, `18446744073709551616`} {
		repository, task := updateFixture(t)
		body := strings.Replace(updateBody(t, task), `"assignee_id":4`, `"assignee_id":`+value, 1)
		response := serveTaskHandler(t, 1, NewHandler(NewService(repository)).Update, http.MethodPut, "1", "", body)
		if response.Code != http.StatusBadRequest || repository.updateCalls != 0 {
			t.Fatalf("invalid assignee type accepted: %s", value)
		}
	}
}

func TestHistoryHTTPPaginationAndAccess(t *testing.T) {
	repository, task := updateFixture(t)
	handler := NewHandler(NewService(repository))
	response := serveTaskHandler(t, 5, handler.History, http.MethodGet, "1", "/history?limit=1&offset=0", "")
	var entries []HistoryEntry
	if err := json.Unmarshal(response.Body.Bytes(), &entries); err != nil || response.Code != http.StatusOK || len(entries) != 1 {
		t.Fatalf("unexpected history response: %d %s", response.Code, response.Body.String())
	}

	if entries[0].TaskID != task.ID || entries[0].ChangedBy != task.CreatedBy || entries[0].ID == 0 || len(entries[0].Changes) == 0 {
		t.Fatal("history response lacks required fields")
	}

	for _, id := range []string{"0", "bad", "18446744073709551616"} {
		response = serveTaskHandler(t, 1, handler.History, http.MethodGet, id, "/history", "")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid history task ID accepted: %s", id)
		}
	}

	for _, query := range []string{"limit=0", "limit=101", "limit=bad", "offset=-1", "offset=", "offset=1&offset=2", "team_id=10"} {
		response = serveTaskHandler(t, 1, handler.History, http.MethodGet, "1", "/history?"+query, "")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid history query accepted: %s", query)
		}
	}

	for _, test := range []struct {
		actor uint64
		id    string
	}{{99, "1"}, {1, "99"}} {
		response = serveTaskHandler(t, test.actor, handler.History, http.MethodGet, test.id, "/history", "")
		var body errorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusNotFound || body.Error.Code != "task_not_found" {
			t.Fatalf("foreign or missing history exposed: %s", response.Body.String())
		}
	}
}
