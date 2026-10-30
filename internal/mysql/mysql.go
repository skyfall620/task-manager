package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/skyfall620/task-manager/internal/config"
)

// Open creates a configured connection pool and verifies connectivity with a bounded ping.
// It closes the pool if verification fails; the caller must close a successful result.
func Open(ctx context.Context, cfg config.MySQLConfig) (*sql.DB, error) {
	driverConfig := mysql.NewConfig()
	driverConfig.Net = "tcp"
	driverConfig.Addr = cfg.Addr
	driverConfig.User = cfg.User
	driverConfig.Passwd = cfg.Password
	driverConfig.DBName = cfg.Database
	driverConfig.ParseTime = true
	driverConfig.Loc = time.UTC
	driverConfig.Timeout = cfg.ConnectTimeout
	driverConfig.ReadTimeout = cfg.ReadTimeout
	driverConfig.WriteTimeout = cfg.WriteTimeout

	connector, err := mysql.NewConnector(driverConfig)
	if err != nil {
		return nil, fmt.Errorf("configure MySQL connector: %w", err)
	}

	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		pingErr := fmt.Errorf("ping MySQL: %w", err)
		if closeErr := db.Close(); closeErr != nil {
			return nil, errors.Join(pingErr, fmt.Errorf("close MySQL: %w", closeErr))
		}

		return nil, pingErr
	}

	return db, nil
}
