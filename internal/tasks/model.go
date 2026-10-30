package tasks

import (
	"errors"
	"time"
)

type Status string

const (
	StatusTodo       Status = "todo"
	StatusInProgress Status = "in_progress"
	StatusDone       Status = "done"
	DefaultLimit            = 20
	MaxLimit                = 100
	MaxCommentLength        = 10000
)

var (
	ErrInvalidInput    = errors.New("invalid input")
	ErrUnauthenticated = errors.New("authentication required")
	ErrForbidden       = errors.New("insufficient permissions")
	ErrTeamNotFound    = errors.New("team not found")
	ErrInvalidAssignee = errors.New("assignee must belong to team")
	ErrTaskNotFound    = errors.New("task not found")
	ErrVersionConflict = errors.New("task version conflict")
)

type Task struct {
	ID          uint64     `json:"id"`
	TeamID      uint64     `json:"team_id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Status      Status     `json:"status"`
	CreatedBy   uint64     `json:"created_by"`
	AssigneeID  *uint64    `json:"assignee_id"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ClosedAt    *time.Time `json:"closed_at"`
	Version     uint64     `json:"version"`
}

type CreateInput struct {
	TeamID      uint64  `json:"team_id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Status      Status  `json:"status"`
	AssigneeID  *uint64 `json:"assignee_id"`
}

type ListFilter struct {
	TeamID     uint64
	Status     Status
	AssigneeID *uint64
	Limit      int
	Offset     int
	Cursor     *listCursor
	cursorMode bool // cursorMode includes one lookahead row for the HTTP response.
}

type UpdateInput struct {
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Status      Status  `json:"status"`
	AssigneeID  *uint64 `json:"assignee_id"`
	Version     uint64  `json:"version"`
}

type HistoryFilter struct {
	Limit  int
	Offset int
}

type Comment struct {
	ID        uint64    `json:"id"`
	TaskID    uint64    `json:"task_id"`
	UserID    uint64    `json:"user_id"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

type CreateCommentInput struct {
	Content string `json:"content"`
}

type CommentsFilter struct {
	Limit  int
	Offset int
}

type Change struct {
	Old any `json:"old"`
	New any `json:"new"`
}

type Changes map[string]Change

type HistoryEntry struct {
	ID        uint64    `json:"id"`
	TaskID    uint64    `json:"task_id"`
	ChangedBy uint64    `json:"changed_by"`
	Changes   Changes   `json:"changes"`
	CreatedAt time.Time `json:"created_at"`
}
