package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

type repositoryStub struct {
	create func(context.Context, User) (User, error)
	find   func(context.Context, string) (User, error)
}

func (r repositoryStub) Create(ctx context.Context, user User) (User, error) {
	return r.create(ctx, user)
}

func (r repositoryStub) FindByEmail(ctx context.Context, email string) (User, error) {
	return r.find(ctx, email)
}

func testService(t *testing.T, repository UserRepository) *Service {
	t.Helper()
	service, err := NewService(repository, testTokens(t), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}

	return service
}

func TestRegisterHashesAndNormalizes(t *testing.T) {
	password := "  password  "
	service := testService(t, repositoryStub{create: func(ctx context.Context, user User) (User, error) {
		if user.Email != "user@example.com" || user.Name != "User" {
			t.Errorf("unexpected user: email=%q, name=%q", user.Email, user.Name)
		}

		if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
			t.Error("password not hashed correctly, or whitespace was trimmed")
		}

		user.ID = 7
		return user, nil
	}})
	user, err := service.Register(context.Background(), RegisterInput{Email: " USER@EXAMPLE.COM ", Name: " User ", Password: password})
	if err != nil || user.ID != 7 || user.PasswordHash != "" {
		t.Fatalf("register: id=%d, hash omitted=%v, error=%v", user.ID, user.PasswordHash == "", err)
	}
}

func TestRegisterValidation(t *testing.T) {
	called := false
	service := testService(t, repositoryStub{create: func(context.Context, User) (User, error) {
		called = true
		return User{}, nil
	}})
	for _, input := range []RegisterInput{
		{Email: "bad", Name: "User", Password: "password"},
		{Email: "User <user@example.com>", Name: "User", Password: "password"},
		{Email: "user@example.com", Name: " ", Password: "password"},
		{Email: "user@example.com", Name: strings.Repeat("я", 256), Password: "password"},
		{Email: "user@example.com", Name: "User", Password: "short"},
		{Email: "user@example.com", Name: "User", Password: strings.Repeat("a", 73)},
		{Email: "user@example.com", Name: "User", Password: strings.Repeat("я", 37)},
	} {
		if _, err := service.Register(context.Background(), input); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("want invalid input, got %v", err)
		}
	}

	if called {
		t.Error("invalid registration reached repository")
	}
}

func TestRegisterPropagatesConflictAndDatabaseFailure(t *testing.T) {
	databaseErr := errors.New("database unavailable")
	for _, expected := range []error{ErrEmailTaken, databaseErr} {
		service := testService(t, repositoryStub{create: func(context.Context, User) (User, error) { return User{}, expected }})
		_, err := service.Register(context.Background(), RegisterInput{Email: "user@example.com", Name: "User", Password: "password"})
		if !errors.Is(err, expected) {
			t.Errorf("error = %v; want %v", err, expected)
		}
	}
}

func TestLogin(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}

	databaseErr := errors.New("database unavailable")
	for _, test := range []struct {
		name     string
		password string
		findErr  error
		wantErr  error
	}{
		{"valid", "password", nil, nil},
		{"wrong password", "wrong-password", nil, ErrInvalidCredentials},
		{"unknown user", "password", ErrUserNotFound, ErrInvalidCredentials},
		{"database failure", "password", databaseErr, databaseErr},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := testService(t, repositoryStub{find: func(ctx context.Context, email string) (User, error) {
				if email != "user@example.com" {
					t.Errorf("email = %q", email)
				}

				return User{ID: 7, PasswordHash: string(hash)}, test.findErr
			}})
			token, err := service.Login(context.Background(), LoginInput{Email: " USER@EXAMPLE.COM ", Password: test.password})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v; want %v", err, test.wantErr)
			}

			if test.wantErr == nil {
				id, err := service.tokens.Validate(token.AccessToken)
				if err != nil || id != 7 || token.ExpiresIn != 3600 || token.TokenType != "Bearer" {
					t.Fatalf("invalid login token: id=%d, error=%v", id, err)
				}
			} else if token.AccessToken != "" {
				t.Error("failed login returned a token")
			}
		})
	}
}
