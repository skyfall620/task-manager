package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"
)

func TestRegisterHTTP(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"created", nil, http.StatusCreated, ""},
		{"duplicate email", classifyCreateError(&mysql.MySQLError{Number: 1062}), http.StatusConflict, "email_taken"},
		{"database failure", errors.New("private database details"), http.StatusInternalServerError, "internal_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := testService(t, repositoryStub{create: func(ctx context.Context, user User) (User, error) {
				user.ID = 7
				return user, test.err
			}})
			request := httptest.NewRequest(http.MethodPost, "/api/v1/register", strings.NewReader(`{"email":"user@example.com","password":"password","name":"User"}`))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			NewHandler(service).Register(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d; want %d", response.Code, test.status)
			}

			if strings.Contains(response.Body.String(), "password") || strings.Contains(response.Body.String(), "private database details") {
				t.Error("response exposes private data")
			}

			if response.Header().Get("Cache-Control") != "no-store" {
				t.Error("missing no-store header")
			}

			if test.code != "" {
				var body errorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != test.code {
					t.Fatalf("unexpected error response: %s", response.Body.String())
				}
			} else {
				var user User
				if err := json.Unmarshal(response.Body.Bytes(), &user); err != nil || user.ID != 7 || user.Email != "user@example.com" {
					t.Fatalf("unexpected registration response: %s", response.Body.String())
				}
			}
		})
	}
}

func TestRegisterRejectsInvalidRequests(t *testing.T) {
	called := false
	handler := NewHandler(testService(t, repositoryStub{create: func(context.Context, User) (User, error) {
		called = true
		return User{}, nil
	}}))
	for _, test := range []struct {
		name        string
		contentType string
		body        string
		status      int
	}{
		{"missing media type", "", `{}`, http.StatusUnsupportedMediaType},
		{"wrong media type", "text/plain", `{}`, http.StatusUnsupportedMediaType},
		{"broken JSON", "application/json", `{`, http.StatusBadRequest},
		{"null", "application/json", `null`, http.StatusBadRequest},
		{"array", "application/json", `[]`, http.StatusBadRequest},
		{"unknown field", "application/json", `{"admin":true}`, http.StatusBadRequest},
		{"multiple objects", "application/json", `{} {}`, http.StatusBadRequest},
		{"missing fields", "application/json", `{}`, http.StatusBadRequest},
		{"invalid email", "application/json", `{"email":"bad","password":"password","name":"User"}`, http.StatusBadRequest},
		{"large body", "application/json", `{"name":"` + strings.Repeat("a", 17*1024) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/register", strings.NewReader(test.body))
			request.Header.Set("Content-Type", test.contentType)
			response := httptest.NewRecorder()
			handler.Register(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d; want %d; body = %s", response.Code, test.status, response.Body.String())
			}
		})
	}

	if called {
		t.Error("invalid request reached repository")
	}
}

func TestLoginHTTP(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name     string
		password string
		findErr  error
		status   int
	}{
		{"valid", "password", nil, http.StatusOK},
		{"wrong password", "wrong-password", nil, http.StatusUnauthorized},
		{"unknown user", "password", ErrUserNotFound, http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := testService(t, repositoryStub{find: func(context.Context, string) (User, error) {
				return User{ID: 7, PasswordHash: string(hash)}, test.findErr
			}})
			body, err := json.Marshal(LoginInput{Email: "user@example.com", Password: test.password})
			if err != nil {
				t.Fatal(err)
			}

			request := httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(string(body)))
			request.Header.Set("Content-Type", "application/json; charset=utf-8")
			response := httptest.NewRecorder()
			NewHandler(service).Login(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d; want %d", response.Code, test.status)
			}

			if test.status == http.StatusOK {
				var token TokenResponse
				if err := json.Unmarshal(response.Body.Bytes(), &token); err != nil {
					t.Fatal(err)
				}

				if id, err := service.tokens.Validate(token.AccessToken); err != nil || id != 7 {
					t.Fatalf("invalid login token: id=%d, error=%v", id, err)
				}
			} else {
				var body errorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != "invalid_credentials" || body.Error.Message != "invalid email or password" {
					t.Fatalf("unexpected login error: %s", response.Body.String())
				}
			}
		})
	}
}
