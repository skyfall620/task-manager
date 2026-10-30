package teams

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Reuse the existing runner's DSN for a migrated, separate test database.
func TestRepositoryIntegration(t *testing.T) {
	dsn := os.Getenv("TASKS_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set TASKS_TEST_MYSQL_DSN to run MySQL integration tests")
	}

	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid TASKS_TEST_MYSQL_DSN")
	}

	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 5 * time.Second
	cfg.WriteTimeout = 5 * time.Second
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal("cannot configure integration database connector")
	}

	db := sql.OpenDB(connector)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})

	t.Run("owner-insert-failure-rolls-back-team", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		t.Cleanup(cancel)
		fixtureID := rand.Text()
		teamName := "teams-rollback-" + fixtureID
		emails := []string{
			"teams-creator-" + fixtureID + "@example.test",
			"teams-missing-" + fixtureID + "@example.test",
		}
		var userIDs [2]uint64
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cleanupCancel()
			// Also remove a leaked team if the rollback assertion fails.
			for _, query := range []string{
				"DELETE FROM team_members WHERE team_id IN (SELECT id FROM teams WHERE created_by = ? AND name = ?)",
				"DELETE FROM teams WHERE created_by = ? AND name = ?",
			} {
				if _, err := db.ExecContext(cleanupCtx, query, userIDs[0], teamName); err != nil {
					t.Error(err)
				}
			}

			// Emails also cover fixture inserts whose LastInsertId could not be read.
			if _, err := db.ExecContext(cleanupCtx, "DELETE FROM users WHERE email IN (?, ?)", emails[0], emails[1]); err != nil {
				t.Error(err)
			}
		})

		for i, email := range emails {
			result, err := db.ExecContext(ctx,
				"INSERT INTO users (email, password_hash, name) VALUES (?, ?, ?)",
				email, "integration-test-not-a-login-hash", "Team integration user",
			)
			if err != nil {
				t.Fatal(err)
			}

			id, err := result.LastInsertId()
			if err != nil || id <= 0 {
				t.Fatalf("invalid fixture insert ID: %v", err)
			}

			userIDs[i] = uint64(id)
		}

		if _, err := db.ExecContext(ctx, "DELETE FROM users WHERE id = ?", userIDs[1]); err != nil {
			t.Fatal(err)
		}

		// The creator exists, so only the owner INSERT violates a foreign key.
		team, err := NewRepository(db).Create(ctx,
			Team{Name: teamName, CreatedBy: userIDs[0]},
			Member{UserID: userIDs[1], Role: RoleOwner},
		)
		var mysqlErr *mysql.MySQLError
		if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1452 || !strings.Contains(err.Error(), "insert team owner:") {
			t.Fatalf("expected owner foreign-key failure, got %v", err)
		}

		if team != (Team{}) {
			t.Fatal("failed creation returned a team")
		}

		var count int
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM teams WHERE created_by = ? AND name = ?", userIDs[0], teamName,
		).Scan(&count); err != nil {
			t.Fatal(err)
		}

		if count != 0 {
			t.Fatal("owner insertion failure left a persisted team")
		}
	})
}
