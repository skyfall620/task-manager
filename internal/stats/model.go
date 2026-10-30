package stats

type TaskCounts struct {
	Todo       uint64 `json:"todo"`
	InProgress uint64 `json:"in_progress"`
	Done       uint64 `json:"done"`
}

type AssigneeStats struct {
	UserID      uint64 `json:"user_id"`
	Name        string `json:"name"`
	ClosedTasks uint64 `json:"closed_tasks"`
}

type Report struct {
	TeamID                  uint64          `json:"team_id"`
	TasksByStatus           TaskCounts      `json:"tasks_by_status"`
	TopAssignees            []AssigneeStats `json:"top_assignees"`
	AverageCloseTimeSeconds *float64        `json:"average_close_time_seconds"`
	CommentsCount           uint64          `json:"comments_count"`
}
