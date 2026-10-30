package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/skyfall620/task-manager/internal/teams"
)

type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

func (r *Repository) WithTeamTransaction(ctx context.Context, teamID uint64, operation func(TaskTransaction) error) (err error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin task transaction: %w", err)
	}

	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback task transaction: %w", rollbackErr))
		}
	}()
	var team teams.Team
	// Shared locks allow concurrent task reads and creation, while preventing
	// membership changes (which lock the same team FOR UPDATE) during authorization.
	err = tx.QueryRowContext(ctx,
		"SELECT id, name, created_by, created_at FROM teams WHERE id = ? FOR SHARE", teamID,
	).Scan(&team.ID, &team.Name, &team.CreatedBy, &team.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTeamNotFound
	}

	if err != nil {
		return fmt.Errorf("lock task team: %w", err)
	}

	if err := operation(&taskTransaction{tx: tx, team: team}); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit task transaction: %w", err)
	}

	return nil
}

type taskTransaction struct {
	tx   *sql.Tx
	team teams.Team
}

func (t *taskTransaction) Team() teams.Team {
	return t.team
}

func (t *taskTransaction) Member(ctx context.Context, userID uint64) (teams.Member, error) {
	member := teams.Member{TeamID: t.team.ID, UserID: userID}
	err := t.tx.QueryRowContext(ctx,
		"SELECT role FROM team_members WHERE team_id = ? AND user_id = ? FOR SHARE", t.team.ID, userID,
	).Scan(&member.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return teams.Member{}, teams.ErrMemberNotFound
	}

	if err != nil {
		return teams.Member{}, fmt.Errorf("lock task team member: %w", err)
	}

	return member, nil
}

func (t *taskTransaction) Create(ctx context.Context, task Task) (Task, error) {
	result, err := t.tx.ExecContext(ctx, `
		INSERT INTO tasks (team_id, title, description, status, created_by, assignee_id,
		                   created_at, updated_at, closed_at, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.team.ID, task.Title, task.Description, task.Status, task.CreatedBy, task.AssigneeID,
		task.CreatedAt, task.UpdatedAt, task.ClosedAt, task.Version)
	if err != nil {
		return Task{}, fmt.Errorf("insert task: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return Task{}, fmt.Errorf("read inserted task ID: %w", err)
	}

	if id <= 0 {
		return Task{}, fmt.Errorf("invalid inserted task ID")
	}

	task.ID = uint64(id)
	task.TeamID = t.team.ID
	return task, nil
}

func (t *taskTransaction) AddHistory(ctx context.Context, entry HistoryEntry) error {
	changes, err := json.Marshal(entry.Changes)
	if err != nil {
		return fmt.Errorf("encode task history: %w", err)
	}

	if _, err := t.tx.ExecContext(ctx,
		"INSERT INTO task_history (task_id, changed_by, changes, created_at) VALUES (?, ?, ?, ?)",
		entry.TaskID, entry.ChangedBy, string(changes), entry.CreatedAt,
	); err != nil {
		return fmt.Errorf("insert task history: %w", err)
	}

	return nil
}

func (t *taskTransaction) List(ctx context.Context, filter ListFilter) ([]Task, error) {
	// The transaction's locked team always supplies the SQL scope.
	filter.TeamID = t.team.ID
	query, args := listQuery(filter)
	rows, err := t.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select team tasks: %w", err)
	}

	defer func() {
		_ = rows.Close()
	}()
	result := []Task{}
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}

		result = append(result, task)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tasks: %w", err)
	}

	return result, nil
}

func listQuery(filter ListFilter) (string, []any) {
	query := `SELECT id, team_id, title, description, status, created_by, assignee_id,
	                 created_at, updated_at, closed_at, version FROM tasks WHERE team_id = ?`
	args := []any{filter.TeamID}
	if filter.Status != "" {
		query += " AND status = ?"
		args = append(args, filter.Status)
	}

	if filter.AssigneeID != nil {
		query += " AND assignee_id = ?"
		args = append(args, *filter.AssigneeID)
	}

	if filter.Cursor != nil {
		query += " AND (created_at < ? OR (created_at = ? AND id < ?))"
		args = append(args, filter.Cursor.CreatedAt, filter.Cursor.CreatedAt, filter.Cursor.ID)
	}

	query += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, filter.Limit)
	if !filter.cursorMode && filter.Cursor == nil {
		query += " OFFSET ?"
		args = append(args, filter.Offset)
	}

	return query, args
}

type rowScanner interface {
	Scan(...any) error
}

func scanTask(row rowScanner) (Task, error) {
	var task Task
	err := row.Scan(&task.ID, &task.TeamID, &task.Title, &task.Description, &task.Status,
		&task.CreatedBy, &task.AssigneeID, &task.CreatedAt, &task.UpdatedAt, &task.ClosedAt, &task.Version)
	return task, err
}
