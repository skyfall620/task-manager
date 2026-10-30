package tasks

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

func (r *Repository) TeamIDForTask(ctx context.Context, taskID uint64) (uint64, error) {
	var teamID uint64
	err := r.db.QueryRowContext(ctx, "SELECT team_id FROM tasks WHERE id = ?", taskID).Scan(&teamID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrTaskNotFound
	}

	if err != nil {
		return 0, fmt.Errorf("select task team: %w", err)
	}

	return teamID, nil
}

func (t *taskTransaction) LockTask(ctx context.Context, taskID uint64) (Task, error) {
	task, err := scanTask(t.tx.QueryRowContext(ctx, `
		SELECT id, team_id, title, description, status, created_by, assignee_id,
		       created_at, updated_at, closed_at, version
		FROM tasks WHERE id = ? AND team_id = ? FOR UPDATE`, taskID, t.team.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrTaskNotFound
	}

	if err != nil {
		return Task{}, fmt.Errorf("lock task: %w", err)
	}

	return task, nil
}

func (t *taskTransaction) Update(ctx context.Context, task Task, expectedVersion uint64) error {
	result, err := t.tx.ExecContext(ctx, `
		UPDATE tasks
		SET title = ?, description = ?, status = ?, assignee_id = ?,
		    updated_at = ?, closed_at = ?, version = version + 1
		WHERE id = ? AND team_id = ? AND version = ?`,
		task.Title, task.Description, task.Status, task.AssigneeID, task.UpdatedAt, task.ClosedAt,
		task.ID, t.team.ID, expectedVersion)
	if err != nil {
		return fmt.Errorf("update task row: %w", err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read updated task count: %w", err)
	}

	if count != 1 {
		return ErrVersionConflict
	}

	return nil
}

func (t *taskTransaction) History(ctx context.Context, taskID uint64, filter HistoryFilter) ([]HistoryEntry, error) {
	// Wait for an in-flight task update and its history to commit together.
	var id uint64
	err := t.tx.QueryRowContext(ctx, "SELECT id FROM tasks WHERE id = ? AND team_id = ? FOR SHARE",
		taskID, t.team.ID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTaskNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("lock history task: %w", err)
	}

	rows, err := t.tx.QueryContext(ctx, `
		SELECT h.id, h.task_id, h.changed_by, h.changes, h.created_at
		FROM task_history AS h
		JOIN tasks AS t ON t.id = h.task_id
		WHERE h.task_id = ? AND t.team_id = ?
		ORDER BY h.created_at ASC, h.id ASC LIMIT ? OFFSET ?`, taskID, t.team.ID, filter.Limit, filter.Offset)
	if err != nil {
		return nil, fmt.Errorf("select task history: %w", err)
	}

	defer func() {
		_ = rows.Close()
	}()
	result := []HistoryEntry{}
	for rows.Next() {
		var entry HistoryEntry
		var changes []byte
		if err := rows.Scan(&entry.ID, &entry.TaskID, &entry.ChangedBy, &changes, &entry.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan task history: %w", err)
		}

		decoder := json.NewDecoder(bytes.NewReader(changes))
		decoder.UseNumber()
		if err := decoder.Decode(&entry.Changes); err != nil {
			return nil, fmt.Errorf("decode task history: %w", err)
		}

		result = append(result, entry)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task history: %w", err)
	}

	return result, nil
}
