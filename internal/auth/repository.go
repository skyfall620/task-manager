package auth

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

// Create inserts a user and reads the database timestamp in one transaction.
// It returns the user only after commit and reports duplicate emails as ErrEmailTaken.
func (r *Repository) Create(ctx context.Context, user User) (created User, err error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return User{}, fmt.Errorf("begin user transaction: %w", err)
	}

	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback user transaction: %w", rollbackErr))
		}
	}()

	result, err := tx.ExecContext(ctx,
		"INSERT INTO users (email, password_hash, name) VALUES (?, ?, ?)",
		user.Email, user.PasswordHash, user.Name,
	)
	if err != nil {
		return User{}, classifyCreateError(err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return User{}, fmt.Errorf("read inserted user ID: %w", err)
	}

	if id <= 0 {
		return User{}, fmt.Errorf("invalid inserted user ID")
	}

	user.ID = uint64(id)
	if err := tx.QueryRowContext(ctx, "SELECT created_at FROM users WHERE id = ?", user.ID).Scan(&user.CreatedAt); err != nil {
		return User{}, fmt.Errorf("read inserted user timestamp: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("commit user transaction: %w", err)
	}

	return user, nil
}

func (r *Repository) FindByEmail(ctx context.Context, email string) (User, error) {
	var user User
	err := r.db.QueryRowContext(ctx,
		"SELECT id, email, password_hash, name, created_at FROM users WHERE email = ?", email,
	).Scan(&user.ID, &user.Email, &user.PasswordHash, &user.Name, &user.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrUserNotFound
	}

	if err != nil {
		return User{}, fmt.Errorf("select user by email: %w", err)
	}

	return user, nil
}

func classifyCreateError(err error) error {
	var mysqlErr *mysql.MySQLError
	// The INSERT supplies no primary key; email is its only unique constraint.
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		return ErrEmailTaken
	}

	return fmt.Errorf("insert user: %w", err)
}
