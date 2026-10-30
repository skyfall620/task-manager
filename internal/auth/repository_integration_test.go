package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// AUTH_TEST_MYSQL_DSN must point to a separate test database with migrations applied.
func TestRepositoryCreateRollsBackOnTimestampReadFailure(t *testing.T) {
	dsn := os.Getenv("AUTH_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set AUTH_TEST_MYSQL_DSN to run MySQL integration tests")
	}

	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid AUTH_TEST_MYSQL_DSN")
	}

	// Keep INSERT valid but force scanning created_at into time.Time to fail.
	cfg.ParseTime = false
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

	verificationCfg := cfg.Clone()
	verificationCfg.ParseTime = true
	verificationConnector, err := mysql.NewConnector(verificationCfg)
	if err != nil {
		t.Fatal("cannot configure verification database connector")
	}

	verificationDB := sql.OpenDB(verificationConnector)
	t.Cleanup(func() {
		if err := verificationDB.Close(); err != nil {
			t.Error(err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	email := "auth-" + rand.Text() + "@example.test"
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := verificationDB.ExecContext(cleanupCtx, "DELETE FROM users WHERE email = ?", email); err != nil {
			t.Error(err)
		}
	})

	user, err := NewRepository(db).Create(ctx, User{
		Email:        email,
		PasswordHash: "integration-test-not-a-login-hash",
		Name:         "Auth integration user",
	})
	if err == nil || !strings.Contains(err.Error(), "read inserted user timestamp") ||
		!strings.Contains(err.Error(), "Scan error") || !strings.Contains(err.Error(), "created_at") {
		t.Fatalf("expected timestamp scan failure, got %v", err)
	}

	if user != (User{}) {
		t.Fatal("failed creation returned a user")
	}

	var count int
	if err := verificationDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE email = ?", email).Scan(&count); err != nil {
		t.Fatal(err)
	}

	if count != 0 {
		t.Fatal("failed creation left a persisted user")
	}
}
