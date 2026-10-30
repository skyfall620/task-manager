package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLoadRejectsMissingOrShortJWTSecret(t *testing.T) {
	for _, test := range []struct{ name, secret string }{
		{"missing", ""},
		{"short", strings.Repeat("a", 31)},
		{"whitespace", strings.Repeat(" ", 32)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("JWT_SECRET", test.secret)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "JWT_SECRET") {
				t.Fatalf("want JWT_SECRET error, got %v", err)
			}

			if test.name == "short" && strings.Contains(err.Error(), test.secret) {
				t.Fatal("configuration error exposes secret")
			}
		})
	}
}

func setValidLoadEnvironment(t *testing.T) {
	t.Helper()
	defaults := map[string]string{
		"JWT_SECRET": strings.Repeat("a", 32), "JWT_ISSUER": "task-manager", "JWT_AUDIENCE": "task-manager-api",
		"JWT_TTL": "1h", "BCRYPT_COST": "12", "MYSQL_PASSWORD": "test-only-password",
		"HTTP_ADDR": ":8080", "LOG_LEVEL": "INFO", "MYSQL_ADDR": "127.0.0.1:3306",
		"MYSQL_USER": "task_manager", "MYSQL_DATABASE": "task_manager", "REDIS_ADDR": "127.0.0.1:6379",
		"HTTP_READ_HEADER_TIMEOUT": "5s", "HTTP_READ_TIMEOUT": "10s", "HTTP_WRITE_TIMEOUT": "15s",
		"HTTP_IDLE_TIMEOUT": "60s", "SHUTDOWN_TIMEOUT": "10s", "MYSQL_CONNECT_TIMEOUT": "5s",
		"MYSQL_READ_TIMEOUT": "5s", "MYSQL_WRITE_TIMEOUT": "5s", "MYSQL_CONN_MAX_LIFETIME": "3m",
		"MYSQL_CONN_MAX_IDLE_TIME": "1m", "REDIS_CONNECT_TIMEOUT": "3s", "REDIS_READ_TIMEOUT": "2s",
		"REDIS_WRITE_TIMEOUT": "2s", "REDIS_PING_TIMEOUT": "5s", "MYSQL_MAX_OPEN_CONNS": "10",
		"MYSQL_MAX_IDLE_CONNS": "5", "REDIS_DB": "0",
	}
	for name, value := range defaults {
		t.Setenv(name, value)
	}
}

func TestLoadRejectsAddressesWithoutExposingSecrets(t *testing.T) {
	const secret = "test-only-address-secret"
	for _, test := range []struct{ name, address string }{
		{"MYSQL_ADDR", "mysql://user:" + secret + "@127.0.0.1:3306/task_manager"},
		{"REDIS_ADDR", "redis://user:" + secret + "@127.0.0.1:6379/0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			setValidLoadEnvironment(t)
			t.Setenv(test.name, test.address)
			cfg, err := Load()
			if err == nil {
				t.Fatal("accepted an address containing credentials instead of host:port")
			}

			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), test.address) {
				t.Fatal("address validation error exposes credentials or the original address")
			}

			if want := test.name + " must use host:port format"; err.Error() != want {
				t.Fatalf("error = %q; want %q", err.Error(), want)
			}

			if cfg != (Config{}) {
				t.Fatal("Load returned partial configuration on error")
			}
		})
	}
}

func TestLoadAuthConfiguration(t *testing.T) {
	setValidLoadEnvironment(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Auth.JWTTTL != time.Hour || cfg.Auth.BcryptCost != 12 || cfg.Auth.JWTIssuer != "task-manager" || cfg.Auth.JWTAudience != "task-manager-api" {
		t.Fatal("unexpected auth configuration")
	}

	for _, test := range []struct{ name, value string }{
		{"JWT_ISSUER", ""}, {"JWT_AUDIENCE", " "}, {"JWT_TTL", "0s"}, {"JWT_TTL", "1ms"},
		{"JWT_TTL", "invalid"}, {"BCRYPT_COST", "3"}, {"BCRYPT_COST", "32"}, {"BCRYPT_COST", "invalid"},
	} {
		t.Run(test.name+"="+test.value, func(t *testing.T) {
			t.Setenv(test.name, test.value)
			if _, err := Load(); err == nil {
				t.Fatal("accepted invalid authentication configuration")
			}
		})
	}
}

func TestLoadRejectsInvalidDurations(t *testing.T) {
	setValidLoadEnvironment(t)
	for _, name := range []string{"HTTP_READ_TIMEOUT", "MYSQL_CONN_MAX_LIFETIME", "REDIS_PING_TIMEOUT"} {
		for _, value := range []string{"invalid", "", "0s", "-1s"} {
			t.Run(name+"="+value, func(t *testing.T) {
				t.Setenv(name, value)
				cfg, err := Load()
				if err == nil {
					t.Fatal("accepted invalid duration")
				}

				if cfg != (Config{}) {
					t.Fatal("Load returned partial configuration on error")
				}

				_, parseErr := time.ParseDuration(value)
				if parseErr != nil {
					if err.Error() != name+": "+parseErr.Error() {
						t.Fatalf("unexpected duration error: %v", err)
					}

					cause := errors.Unwrap(err)
					if cause == nil || cause.Error() != parseErr.Error() {
						t.Fatal("duration parse error lost its wrapped cause")
					}
				} else if err.Error() != name+" must be positive" || errors.Unwrap(err) != nil {
					t.Fatalf("unexpected nonpositive duration error: %v", err)
				}
			})
		}
	}
}

func TestLoadErrorPriority(t *testing.T) {
	setValidLoadEnvironment(t)
	_, parseErr := time.ParseDuration("invalid")
	if parseErr == nil {
		t.Fatal("invalid duration unexpectedly parsed")
	}

	for _, test := range []struct {
		name       string
		values     map[string]string
		want       string
		wantPrefix bool
	}{
		{
			name:   "log level before durations",
			values: map[string]string{"LOG_LEVEL": "invalid", "HTTP_READ_TIMEOUT": "invalid"},
			want:   "LOG_LEVEL: ", wantPrefix: true,
		},
		{
			name:   "duration table order",
			values: map[string]string{"HTTP_READ_TIMEOUT": "0s", "REDIS_PING_TIMEOUT": "invalid"},
			want:   "HTTP_READ_TIMEOUT must be positive",
		},
		{
			name:   "all durations before integers",
			values: map[string]string{"REDIS_PING_TIMEOUT": "invalid", "MYSQL_MAX_OPEN_CONNS": "0"},
			want:   "REDIS_PING_TIMEOUT: " + parseErr.Error(),
		},
		{
			name: "max open connections before other integers",
			values: map[string]string{
				"MYSQL_MAX_OPEN_CONNS": "invalid", "MYSQL_MAX_IDLE_CONNS": "-1",
				"REDIS_DB": "-1", "BCRYPT_COST": "3",
			},
			want: "MYSQL_MAX_OPEN_CONNS must be an integer >= 1",
		},
		{
			name: "max idle connections before Redis and bcrypt",
			values: map[string]string{
				"MYSQL_MAX_IDLE_CONNS": "-1", "REDIS_DB": "invalid", "BCRYPT_COST": "3",
			},
			want: "MYSQL_MAX_IDLE_CONNS must be an integer >= 0",
		},
		{
			name:   "Redis database before bcrypt",
			values: map[string]string{"REDIS_DB": "-1", "BCRYPT_COST": "invalid"},
			want:   "REDIS_DB must be an integer >= 0",
		},
		{
			name:   "all integers before pool validation",
			values: map[string]string{"MYSQL_MAX_OPEN_CONNS": "1", "BCRYPT_COST": "invalid"},
			want:   "BCRYPT_COST must be an integer >= 4",
		},
		{
			name:   "integer validation before JWT minimum",
			values: map[string]string{"JWT_TTL": "500ms", "BCRYPT_COST": "3"},
			want:   "BCRYPT_COST must be an integer >= 4",
		},
		{
			name:   "pool validation before JWT minimum",
			values: map[string]string{"JWT_TTL": "500ms", "MYSQL_MAX_OPEN_CONNS": "1"},
			want:   "MYSQL_MAX_IDLE_CONNS must not exceed MYSQL_MAX_OPEN_CONNS",
		},
		{
			name:   "bcrypt upper limit before JWT minimum",
			values: map[string]string{"JWT_TTL": "500ms", "BCRYPT_COST": "32"},
			want:   "BCRYPT_COST must not exceed 31",
		},
		{
			name:   "JWT minimum after other validation",
			values: map[string]string{"JWT_TTL": "500ms"},
			want:   "JWT_TTL must be at least one second",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			for name, value := range test.values {
				t.Setenv(name, value)
			}

			cfg, err := Load()
			if err == nil {
				t.Fatal("accepted invalid configuration")
			}

			if cfg != (Config{}) {
				t.Fatal("Load returned partial configuration on error")
			}

			if test.wantPrefix {
				if !strings.HasPrefix(err.Error(), test.want) {
					t.Fatalf("error=%v; want prefix %q", err, test.want)
				}
			} else if err.Error() != test.want {
				t.Fatalf("error=%v; want %q", err, test.want)
			}
		})
	}
}
