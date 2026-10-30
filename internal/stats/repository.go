package stats

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/skyfall620/task-manager/internal/teams"
)

type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

func (r *Repository) WithTeamTransaction(ctx context.Context, teamID uint64, operation func(ReportTransaction) error) (err error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin stats transaction: %w", err)
	}

	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback stats transaction: %w", rollbackErr))
		}
	}()
	var team teams.Team
	// Membership mutations lock this same row FOR UPDATE. Keep access stable
	// through the report without blocking concurrent task operations.
	err = tx.QueryRowContext(ctx,
		"SELECT id, name, created_by, created_at FROM teams WHERE id = ? FOR SHARE", teamID,
	).Scan(&team.ID, &team.Name, &team.CreatedBy, &team.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return teams.ErrTeamNotFound
	}

	if err != nil {
		return fmt.Errorf("lock stats team: %w", err)
	}

	if err := operation(&reportTransaction{tx: tx, team: team}); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit stats transaction: %w", err)
	}

	return nil
}

type reportTransaction struct {
	tx   *sql.Tx
	team teams.Team
}

func (t *reportTransaction) Team() teams.Team {
	return t.team
}

func (t *reportTransaction) Member(ctx context.Context, userID uint64) (teams.Member, error) {
	member := teams.Member{TeamID: t.team.ID, UserID: userID}
	err := t.tx.QueryRowContext(ctx,
		"SELECT role FROM team_members WHERE team_id = ? AND user_id = ? FOR SHARE", t.team.ID, userID,
	).Scan(&member.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return teams.Member{}, teams.ErrMemberNotFound
	}

	if err != nil {
		return teams.Member{}, fmt.Errorf("lock stats member: %w", err)
	}

	return member, nil
}

// Aggregate before joining comments or the top three: those joins must not
// multiply task counts or give tasks with more comments a larger weight.
const reportQuery = `
WITH report_time AS (
    SELECT CAST(? AS DATETIME(6)) AS as_of
), team_tasks AS (
    SELECT id, status, assignee_id, created_at, closed_at
    FROM tasks WHERE team_id = ?
), task_metrics AS (
    SELECT
        COALESCE(SUM(status = 'todo'), 0) AS todo,
        COALESCE(SUM(status = 'in_progress'), 0) AS in_progress,
        COALESCE(SUM(status = 'done'), 0) AS done,
        AVG(CASE WHEN status = 'done' AND closed_at IS NOT NULL
            THEN TIMESTAMPDIFF(MICROSECOND, created_at, closed_at)
        END) / 1e6 AS average_close_seconds
    FROM team_tasks
), comment_metrics AS (
    SELECT COUNT(*) AS comments_count
    FROM task_comments AS c
    JOIN team_tasks AS t ON t.id = c.task_id
), top_assignees AS (
    SELECT t.assignee_id, COUNT(*) AS closed_tasks
    FROM team_tasks AS t
    CROSS JOIN report_time AS p
    WHERE t.status = 'done' AND t.assignee_id IS NOT NULL
        AND t.closed_at >= DATE_SUB(p.as_of, INTERVAL 30 DAY)
        AND t.closed_at <= p.as_of
    GROUP BY t.assignee_id
    ORDER BY closed_tasks DESC, t.assignee_id ASC
    LIMIT 3
)
SELECT m.todo, m.in_progress, m.done, m.average_close_seconds,
       c.comments_count, a.assignee_id, u.name, a.closed_tasks
FROM task_metrics AS m
CROSS JOIN comment_metrics AS c
LEFT JOIN top_assignees AS a ON TRUE
LEFT JOIN users AS u ON u.id = a.assignee_id
ORDER BY a.closed_tasks DESC, a.assignee_id ASC`

func (t *reportTransaction) Report(ctx context.Context, asOf time.Time) (Report, error) {
	// A single statement gives every aggregate the same MySQL snapshot.
	rows, err := t.tx.QueryContext(ctx, reportQuery, asOf.UTC().Truncate(time.Microsecond), t.team.ID)
	if err != nil {
		return Report{}, fmt.Errorf("select team stats: %w", err)
	}

	defer func() {
		_ = rows.Close()
	}()
	report := Report{TeamID: t.team.ID, TopAssignees: []AssigneeStats{}}
	for rows.Next() {
		var average sql.NullFloat64
		var userID, closedTasks sql.Null[uint64]
		var name sql.NullString
		if err := rows.Scan(&report.TasksByStatus.Todo, &report.TasksByStatus.InProgress,
			&report.TasksByStatus.Done, &average, &report.CommentsCount, &userID, &name, &closedTasks); err != nil {
			return Report{}, fmt.Errorf("scan team stats: %w", err)
		}

		if average.Valid {
			value := average.Float64
			report.AverageCloseTimeSeconds = &value
		}

		if userID.Valid {
			report.TopAssignees = append(report.TopAssignees, AssigneeStats{
				UserID: userID.V, Name: name.String, ClosedTasks: closedTasks.V,
			})
		}
	}

	if err := rows.Err(); err != nil {
		return Report{}, fmt.Errorf("iterate team stats: %w", err)
	}

	return report, nil
}
