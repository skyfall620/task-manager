package teams

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/go-sql-driver/mysql"
)

type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

// Create atomically persists a team and its owner membership, returning only after commit.
func (r *Repository) Create(ctx context.Context, team Team, owner Member) (Team, error) {
	err := r.withTransaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "INSERT INTO teams (name, created_by) VALUES (?, ?)", team.Name, team.CreatedBy)
		if err != nil {
			return fmt.Errorf("insert team: %w", err)
		}

		id, err := result.LastInsertId()
		if err != nil {
			return fmt.Errorf("read inserted team ID: %w", err)
		}

		if id <= 0 {
			return fmt.Errorf("invalid inserted team ID")
		}

		team.ID = uint64(id)
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO team_members (team_id, user_id, role) VALUES (?, ?, ?)", team.ID, owner.UserID, owner.Role,
		); err != nil {
			return fmt.Errorf("insert team owner: %w", err)
		}

		if err := tx.QueryRowContext(ctx, "SELECT created_at FROM teams WHERE id = ?", team.ID).Scan(&team.CreatedAt); err != nil {
			return fmt.Errorf("read inserted team timestamp: %w", err)
		}

		return nil
	})
	if err != nil {
		return Team{}, err
	}

	return team, nil
}

func (r *Repository) List(ctx context.Context, userID uint64) ([]Team, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT t.id, t.name, t.created_by, t.created_at
		FROM teams AS t
		JOIN team_members AS m ON m.team_id = t.id
		WHERE m.user_id = ?
		ORDER BY t.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("select user teams: %w", err)
	}

	defer func() {
		_ = rows.Close()
	}()

	teams := []Team{}
	for rows.Next() {
		var team Team
		if err := rows.Scan(&team.ID, &team.Name, &team.CreatedBy, &team.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan user team: %w", err)
		}

		teams = append(teams, team)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate user teams: %w", err)
	}

	return teams, nil
}

// WithTeamTransaction serializes membership changes for a team before checking permissions.
// The transaction handle is valid only within operation. A successful callback is
// followed by commit; callback errors trigger rollback before the method returns.
func (r *Repository) WithTeamTransaction(ctx context.Context, teamID uint64, operation func(TeamTransaction) error) error {
	return r.withTransaction(ctx, func(tx *sql.Tx) error {
		var team Team
		// Every membership mutation takes this lock before reading permissions.
		// Waiting operations read roles after earlier transactions have committed.
		err := tx.QueryRowContext(ctx,
			"SELECT id, name, created_by, created_at FROM teams WHERE id = ? FOR UPDATE", teamID,
		).Scan(&team.ID, &team.Name, &team.CreatedBy, &team.CreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTeamNotFound
		}

		if err != nil {
			return fmt.Errorf("lock team: %w", err)
		}

		return operation(&teamTransaction{tx: tx, team: team})
	})
}

func (r *Repository) withTransaction(ctx context.Context, operation func(*sql.Tx) error) (err error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin team transaction: %w", err)
	}

	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback team transaction: %w", rollbackErr))
		}
	}()
	if err := operation(tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit team transaction: %w", err)
	}

	return nil
}

type teamTransaction struct {
	tx   *sql.Tx
	team Team
}

func (t *teamTransaction) Team() Team {
	return t.team
}

func (t *teamTransaction) Member(ctx context.Context, userID uint64) (Member, error) {
	member := Member{TeamID: t.team.ID, UserID: userID}
	err := t.tx.QueryRowContext(ctx,
		"SELECT role FROM team_members WHERE team_id = ? AND user_id = ? FOR UPDATE", t.team.ID, userID,
	).Scan(&member.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return Member{}, ErrMemberNotFound
	}

	if err != nil {
		return Member{}, fmt.Errorf("lock team member: %w", err)
	}

	return member, nil
}

func (t *teamTransaction) FindUserID(ctx context.Context, email string) (uint64, error) {
	var id uint64
	err := t.tx.QueryRowContext(ctx, "SELECT id FROM users WHERE email = ?", email).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrUserNotFound
	}

	if err != nil {
		return 0, fmt.Errorf("select invited user: %w", err)
	}

	return id, nil
}

func (t *teamTransaction) AddMember(ctx context.Context, member Member) error {
	_, err := t.tx.ExecContext(ctx,
		"INSERT INTO team_members (team_id, user_id, role) VALUES (?, ?, ?)", t.team.ID, member.UserID, member.Role,
	)
	if err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			return ErrAlreadyMember
		}

		return fmt.Errorf("insert team member: %w", err)
	}

	return nil
}

func (t *teamTransaction) SetRole(ctx context.Context, userID uint64, role Role) error {
	result, err := t.tx.ExecContext(ctx,
		"UPDATE team_members SET role = ? WHERE team_id = ? AND user_id = ?", role, t.team.ID, userID,
	)
	if err != nil {
		return fmt.Errorf("update team member role: %w", err)
	}

	return requireAffectedMember(result)
}

func (t *teamTransaction) HasAssignedTasks(ctx context.Context, userID uint64) (bool, error) {
	var assigned bool
	err := t.tx.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM tasks WHERE team_id = ? AND assignee_id = ?)", t.team.ID, userID,
	).Scan(&assigned)
	if err != nil {
		return false, fmt.Errorf("check assigned tasks: %w", err)
	}

	return assigned, nil
}

func (t *teamTransaction) RemoveMember(ctx context.Context, userID uint64) error {
	result, err := t.tx.ExecContext(ctx, "DELETE FROM team_members WHERE team_id = ? AND user_id = ?", t.team.ID, userID)
	if err != nil {
		var mysqlErr *mysql.MySQLError
		// tasks is the only table referencing a team membership in this schema.
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1451 {
			return ErrAssignedTasks
		}

		return fmt.Errorf("delete team member: %w", err)
	}

	return requireAffectedMember(result)
}

func requireAffectedMember(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read affected members: %w", err)
	}

	if count == 0 {
		return ErrMemberNotFound
	}

	return nil
}
