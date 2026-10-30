package auth

import (
	"errors"
	"time"
)

var (
	ErrInvalidInput       = errors.New("invalid input")
	ErrEmailTaken         = errors.New("email already registered")
	ErrUserNotFound       = errors.New("user not found")
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrInvalidToken       = errors.New("invalid access token")
)

type User struct {
	ID           uint64    `json:"id"`
	Email        string    `json:"email"`
	Name         string    `json:"name"`
	CreatedAt    time.Time `json:"created_at"`
	PasswordHash string    `json:"-"`
}

type RegisterInput struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
}

type LoginInput struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type TokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
}
