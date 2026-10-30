package config

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr          string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	LogLevel          slog.Level
	MySQL             MySQLConfig
	Redis             RedisConfig
	Auth              AuthConfig
}

type AuthConfig struct {
	JWTSecret   string
	JWTIssuer   string
	JWTAudience string
	JWTTTL      time.Duration
	BcryptCost  int
}

type MySQLConfig struct {
	Addr            string
	User            string
	Password        string
	Database        string
	ConnectTimeout  time.Duration
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

type RedisConfig struct {
	Addr           string
	Username       string
	Password       string
	DB             int
	ConnectTimeout time.Duration
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	PingTimeout    time.Duration
}

// Load reads and validates process environment variables without loading .env files.
// Defaults apply only to unset variables, not explicitly empty values.
// It returns a zero Config on error.
func Load() (Config, error) {
	cfg := Config{HTTPAddr: envOrDefault("HTTP_ADDR", ":8080")}
	cfg.Auth = AuthConfig{
		JWTSecret:   os.Getenv("JWT_SECRET"),
		JWTIssuer:   envOrDefault("JWT_ISSUER", "task-manager"),
		JWTAudience: envOrDefault("JWT_AUDIENCE", "task-manager-api"),
	}
	if len(strings.TrimSpace(cfg.Auth.JWTSecret)) < 32 {
		return Config{}, fmt.Errorf("JWT_SECRET must contain at least 32 bytes excluding surrounding whitespace")
	}

	if strings.TrimSpace(cfg.Auth.JWTIssuer) == "" || strings.TrimSpace(cfg.Auth.JWTAudience) == "" {
		return Config{}, fmt.Errorf("JWT_ISSUER and JWT_AUDIENCE must not be empty")
	}

	cfg.MySQL = MySQLConfig{
		Addr:     envOrDefault("MYSQL_ADDR", "127.0.0.1:3306"),
		User:     envOrDefault("MYSQL_USER", "task_manager"),
		Password: os.Getenv("MYSQL_PASSWORD"),
		Database: envOrDefault("MYSQL_DATABASE", "task_manager"),
	}
	cfg.Redis = RedisConfig{
		Addr:     envOrDefault("REDIS_ADDR", "127.0.0.1:6379"),
		Username: os.Getenv("REDIS_USERNAME"),
		Password: os.Getenv("REDIS_PASSWORD"),
	}
	for _, address := range []struct{ name, value string }{
		{"HTTP_ADDR", cfg.HTTPAddr},
		{"MYSQL_ADDR", cfg.MySQL.Addr},
		{"REDIS_ADDR", cfg.Redis.Addr},
	} {
		if err := validateAddress(address.name, address.value); err != nil {
			return Config{}, err
		}
	}

	for _, credential := range []struct{ name, value string }{
		{"MYSQL_USER", cfg.MySQL.User},
		{"MYSQL_PASSWORD", cfg.MySQL.Password},
		{"MYSQL_DATABASE", cfg.MySQL.Database},
	} {
		if credential.value == "" {
			return Config{}, fmt.Errorf("%s must not be empty", credential.name)
		}
	}

	if err := cfg.LogLevel.UnmarshalText([]byte(envOrDefault("LOG_LEVEL", "INFO"))); err != nil {
		return Config{}, fmt.Errorf("LOG_LEVEL: %w", err)
	}

	if err := loadDurations(&cfg); err != nil {
		return Config{}, err
	}

	if err := loadIntegers(&cfg); err != nil {
		return Config{}, err
	}

	if cfg.MySQL.MaxIdleConns > cfg.MySQL.MaxOpenConns {
		return Config{}, fmt.Errorf("MYSQL_MAX_IDLE_CONNS must not exceed MYSQL_MAX_OPEN_CONNS")
	}

	if cfg.Auth.BcryptCost > 31 {
		return Config{}, fmt.Errorf("BCRYPT_COST must not exceed 31")
	}

	if cfg.Auth.JWTTTL < time.Second {
		return Config{}, fmt.Errorf("JWT_TTL must be at least one second")
	}

	return cfg, nil
}

func loadDurations(cfg *Config) error {
	timeouts := []struct {
		name     string
		fallback string
		target   *time.Duration
	}{
		{"HTTP_READ_HEADER_TIMEOUT", "5s", &cfg.ReadHeaderTimeout},
		{"HTTP_READ_TIMEOUT", "10s", &cfg.ReadTimeout},
		{"HTTP_WRITE_TIMEOUT", "15s", &cfg.WriteTimeout},
		{"HTTP_IDLE_TIMEOUT", "60s", &cfg.IdleTimeout},
		{"SHUTDOWN_TIMEOUT", "10s", &cfg.ShutdownTimeout},
		{"JWT_TTL", "1h", &cfg.Auth.JWTTTL},
		{"MYSQL_CONNECT_TIMEOUT", "5s", &cfg.MySQL.ConnectTimeout},
		{"MYSQL_READ_TIMEOUT", "5s", &cfg.MySQL.ReadTimeout},
		{"MYSQL_WRITE_TIMEOUT", "5s", &cfg.MySQL.WriteTimeout},
		{"MYSQL_CONN_MAX_LIFETIME", "3m", &cfg.MySQL.ConnMaxLifetime},
		{"MYSQL_CONN_MAX_IDLE_TIME", "1m", &cfg.MySQL.ConnMaxIdleTime},
		{"REDIS_CONNECT_TIMEOUT", "3s", &cfg.Redis.ConnectTimeout},
		{"REDIS_READ_TIMEOUT", "2s", &cfg.Redis.ReadTimeout},
		{"REDIS_WRITE_TIMEOUT", "2s", &cfg.Redis.WriteTimeout},
		{"REDIS_PING_TIMEOUT", "5s", &cfg.Redis.PingTimeout},
	}
	for _, timeout := range timeouts {
		value, err := time.ParseDuration(envOrDefault(timeout.name, timeout.fallback))
		if err != nil {
			return fmt.Errorf("%s: %w", timeout.name, err)
		}

		if value <= 0 {
			return fmt.Errorf("%s must be positive", timeout.name)
		}

		*timeout.target = value
	}

	return nil
}

func loadIntegers(cfg *Config) error {
	integers := []struct {
		name     string
		fallback string
		minimum  int
		target   *int
	}{
		{"MYSQL_MAX_OPEN_CONNS", "10", 1, &cfg.MySQL.MaxOpenConns},
		{"MYSQL_MAX_IDLE_CONNS", "5", 0, &cfg.MySQL.MaxIdleConns},
		{"REDIS_DB", "0", 0, &cfg.Redis.DB},
		{"BCRYPT_COST", "12", 4, &cfg.Auth.BcryptCost},
	}
	for _, integer := range integers {
		value, err := strconv.Atoi(envOrDefault(integer.name, integer.fallback))
		if err != nil || value < integer.minimum {
			return fmt.Errorf("%s must be an integer >= %d", integer.name, integer.minimum)
		}

		*integer.target = value
	}

	return nil
}

func validateAddress(name, value string) error {
	_, port, err := net.SplitHostPort(value)
	if err != nil {
		return fmt.Errorf("%s must use host:port format", name)
	}

	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return fmt.Errorf("%s port must be an integer between 1 and 65535", name)
	}

	return nil
}

func envOrDefault(name, fallback string) string {
	if value, exists := os.LookupEnv(name); exists {
		return value
	}

	return fallback
}
