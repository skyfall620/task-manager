package tasks

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func (t *taskTransaction) lockCommentsTask(ctx context.Context, taskID uint64) error {
	var id uint64
	err := t.tx.QueryRowContext(ctx, "SELECT id FROM tasks WHERE id = ? AND team_id = ? FOR SHARE",
		taskID, t.team.ID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTaskNotFound
	}

	if err != nil {
		return fmt.Errorf("lock comments task: %w", err)
	}

	return nil
}

func (t *taskTransaction) CreateComment(ctx context.Context, comment Comment) (Comment, error) {
	if err := t.lockCommentsTask(ctx, comment.TaskID); err != nil {
		return Comment{}, err
	}

	result, err := t.tx.ExecContext(ctx,
		"INSERT INTO task_comments (task_id, user_id, content, created_at) VALUES (?, ?, ?, ?)",
		comment.TaskID, comment.UserID, comment.Content, comment.CreatedAt)
	if err != nil {
		return Comment{}, fmt.Errorf("insert task comment: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return Comment{}, fmt.Errorf("read inserted comment ID: %w", err)
	}

	if id <= 0 {
		return Comment{}, fmt.Errorf("invalid inserted comment ID")
	}

	comment.ID = uint64(id)
	return comment, nil
}

func (t *taskTransaction) Comments(ctx context.Context, taskID uint64, filter CommentsFilter) ([]Comment, error) {
	if err := t.lockCommentsTask(ctx, taskID); err != nil {
		return nil, err
	}

	rows, err := t.tx.QueryContext(ctx, `
		SELECT c.id, c.task_id, c.user_id, c.content, c.created_at
		FROM task_comments AS c
		JOIN tasks AS t ON t.id = c.task_id
		WHERE c.task_id = ? AND t.team_id = ?
		ORDER BY c.created_at ASC, c.id ASC LIMIT ? OFFSET ?`, taskID, t.team.ID, filter.Limit, filter.Offset)
	if err != nil {
		return nil, fmt.Errorf("select task comments: %w", err)
	}

	defer func() {
		_ = rows.Close()
	}()
	result := []Comment{}
	for rows.Next() {
		var comment Comment
		if err := rows.Scan(&comment.ID, &comment.TaskID, &comment.UserID, &comment.Content, &comment.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan task comment: %w", err)
		}

		result = append(result, comment)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task comments: %w", err)
	}

	return result, nil
}
