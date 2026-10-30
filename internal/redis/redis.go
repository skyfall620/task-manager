package redis

import (
	"github.com/redis/go-redis/v9"

	"github.com/skyfall620/task-manager/internal/config"
)

// NewClient configures a Redis client without checking server availability.
// Callers can use Ping to check connectivity and must close the client when done.
func NewClient(cfg config.RedisConfig) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:                  cfg.Addr,
		Username:              cfg.Username,
		Password:              cfg.Password,
		DB:                    cfg.DB,
		DialTimeout:           cfg.ConnectTimeout,
		ReadTimeout:           cfg.ReadTimeout,
		WriteTimeout:          cfg.WriteTimeout,
		PoolTimeout:           cfg.ConnectTimeout,
		MaxRetries:            -1,
		ContextTimeoutEnabled: true,
	})
}
