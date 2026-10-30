package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestListCursorServiceValidation(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, filter := range []ListFilter{
		{TeamID: 10, Cursor: &listCursor{CreatedAt: now}},
		{TeamID: 10, Cursor: &listCursor{ID: 1}},
		{TeamID: 10, Cursor: &listCursor{ID: 1, CreatedAt: now.Add(time.Nanosecond)}},
		{TeamID: 10, Cursor: &listCursor{ID: 1, CreatedAt: now}, Offset: 1},
		{TeamID: 10, cursorMode: true, Offset: 1},
	} {
		repository := newRepositoryFake()
		if _, err := NewService(repository).List(context.Background(), 1, filter); !errors.Is(err, ErrInvalidInput) || repository.listCalls != 0 {
			t.Fatalf("invalid cursor filter reached repository: %v", err)
		}
	}

	repository := newRepositoryFake()
	if _, err := NewService(repository).List(context.Background(), 1, ListFilter{TeamID: 10, Limit: MaxLimit, cursorMode: true}); err != nil || repository.lastFilter.Limit != MaxLimit+1 {
		t.Fatalf("maximum page lost lookahead: %v", err)
	}
}

func TestListCursorEncoding(t *testing.T) {
	for _, year := range []int{1000, 2026, 9999} {
		task := Task{ID: ^uint64(0), CreatedAt: time.Date(year, 1, 2, 3, 4, 5, 123456000, time.UTC)}
		value := encodeListCursor(task)
		cursor, err := decodeListCursor(value)
		if err != nil || cursor.ID != task.ID || !cursor.CreatedAt.Equal(task.CreatedAt) {
			t.Fatalf("cursor round trip failed: %v", err)
		}

		if strings.ContainsAny(value, "+/=") {
			t.Fatal("cursor is not unpadded URL-safe Base64")
		}
	}

	for _, value := range []string{
		"", "invalid", strings.Repeat("!", 22), strings.Repeat("A", 22),
		encodeListCursor(Task{ID: 1, CreatedAt: time.Date(999, 1, 1, 0, 0, 0, 0, time.UTC)}),
		encodeListCursor(Task{ID: 0, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}),
	} {
		if _, err := decodeListCursor(value); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid cursor accepted: %q", value)
		}
	}
}

func TestListCursorHTTP(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 123456000, time.UTC)
	for _, test := range []struct {
		name   string
		query  string
		count  int
		header bool
	}{
		{"first page", "limit=2", 3, true},
		{"continuation", "limit=2&cursor=" + encodeListCursor(Task{ID: 9, CreatedAt: now}), 3, true},
		{"exact last page", "limit=2", 2, false},
		{"short last page", "limit=2", 1, false},
		{"empty", "limit=2", 0, false},
		{"maximum limit", "limit=100", 101, true},
		{"explicit zero offset", "limit=2&offset=0", 2, false},
		{"offset", "limit=2&offset=3", 2, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := newRepositoryFake()
			for i := 0; i < test.count; i++ {
				repository.listResult = append(repository.listResult, Task{
					ID: uint64(test.count - i), TeamID: 10, CreatedAt: now, Status: StatusTodo, Version: 1,
				})
			}

			handler := NewHandler(NewService(repository))
			response := serveAuthenticated(t, 1, handler.List, http.MethodGet, "/api/v1/tasks?team_id=10&"+test.query, "")
			var page []Task
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || response.Code != http.StatusOK || page == nil {
				t.Fatalf("unexpected array response: %s", response.Body.String())
			}

			want := test.count
			if test.header {
				want--
			}

			if len(page) != want {
				t.Fatalf("page length = %d; want %d", len(page), want)
			}

			next := response.Header().Get("X-Next-Cursor")
			if (next != "") != test.header {
				t.Fatalf("unexpected next cursor: %q", next)
			}

			if next != "" {
				cursor, err := decodeListCursor(next)
				if err != nil || cursor.ID != page[len(page)-1].ID || !cursor.CreatedAt.Equal(page[len(page)-1].CreatedAt) {
					t.Fatal("next cursor does not use the last returned row")
				}
			}
		})
	}

	for _, query := range []string{
		"cursor=", "cursor=invalid", "cursor=a&cursor=b",
		"cursor=" + encodeListCursor(Task{ID: 1, CreatedAt: now}) + "&offset=0",
		"cursor=" + encodeListCursor(Task{ID: 1, CreatedAt: now}) + "&offset=1",
	} {
		repository := newRepositoryFake()
		response := serveAuthenticated(t, 1, NewHandler(NewService(repository)).List, http.MethodGet, "/api/v1/tasks?team_id=10&"+query, "")
		var body errorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil ||
			response.Code != http.StatusBadRequest || body.Error.Code != "invalid_input" || repository.listCalls != 0 {
			t.Fatalf("invalid cursor request accepted: %s", response.Body.String())
		}
	}
}

func TestListCursorCacheHitPreservesLookahead(t *testing.T) {
	repository := newRepositoryFake()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range []uint64{3, 2, 1} {
		repository.listResult = append(repository.listResult, Task{ID: id, TeamID: 10, CreatedAt: now, Status: StatusTodo, Version: 1})
	}

	cache := &listCacheFake{repository: repository, entry: ListCacheEntry{key: "ticket"}}
	handler := NewHandler(NewServiceWithCache(repository, cache))
	first := serveAuthenticated(t, 1, handler.List, http.MethodGet, "/api/v1/tasks?team_id=10&limit=2", "")
	second := serveAuthenticated(t, 1, handler.List, http.MethodGet, "/api/v1/tasks?team_id=10&limit=2", "")
	if first.Code != http.StatusOK || second.Code != http.StatusOK ||
		first.Body.String() != second.Body.String() || first.Header().Get("X-Next-Cursor") == "" ||
		first.Header().Get("X-Next-Cursor") != second.Header().Get("X-Next-Cursor") ||
		repository.listCalls != 1 || len(cache.entry.Tasks) != 3 || cache.lastFilter.Limit != 3 {
		t.Fatal("cache hit lost lookahead or changed the cursor response")
	}

	if _, err := handler.service.List(context.Background(), 99, ListFilter{TeamID: 10, cursorMode: true}); !errors.Is(err, ErrTeamNotFound) {
		t.Fatal("cursor cache bypassed team authorization")
	}
}

func TestCursorCacheKeysAndBounds(t *testing.T) {
	cache := NewRedisListCache(nil, "cursor-test")
	now := time.Date(2026, 1, 1, 0, 0, 0, 123456000, time.UTC)
	filters := []ListFilter{
		{TeamID: 10, Limit: 3},
		{TeamID: 10, Limit: 3, cursorMode: true},
		{TeamID: 10, Limit: 3, cursorMode: true, Cursor: &listCursor{CreatedAt: now, ID: 5}},
		{TeamID: 10, Limit: 3, cursorMode: true, Cursor: &listCursor{CreatedAt: now, ID: 4}},
		{TeamID: 10, Limit: 3, cursorMode: true, Cursor: &listCursor{CreatedAt: now.Add(-time.Microsecond), ID: 5}},
	}
	keys := map[string]bool{}
	for _, filter := range filters {
		key := cache.listKey("namespace", "generation", filter)
		if keys[key] {
			t.Fatal("pagination variants share a cache key")
		}

		keys[key] = true
	}

	filter := filters[2]
	task := Task{ID: 4, TeamID: 10, Version: 1, Status: StatusTodo, CreatedAt: now}
	if !validCachedList([]Task{task}, filter) {
		t.Fatal("valid cursor page rejected")
	}

	task.ID = 5
	if validCachedList([]Task{task}, filter) {
		t.Fatal("cursor boundary included in cached page")
	}

	task.ID = 4
	if validCachedList([]Task{task, task}, filter) {
		t.Fatal("duplicate rows accepted in cursor page")
	}
}
