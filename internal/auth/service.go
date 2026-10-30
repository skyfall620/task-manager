package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

type UserRepository interface {
	Create(context.Context, User) (User, error)
	FindByEmail(context.Context, string) (User, error)
}

type Service struct {
	repository UserRepository
	tokens     *TokenManager
	bcryptCost int
	dummyHash  []byte
}

func NewService(repository UserRepository, tokens *TokenManager, cost int) (*Service, error) {
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		return nil, fmt.Errorf("bcrypt cost must be between %d and %d", bcrypt.MinCost, bcrypt.MaxCost)
	}

	password := make([]byte, 32)
	if _, err := rand.Read(password); err != nil {
		return nil, fmt.Errorf("generate dummy password: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword(password, cost)
	if err != nil {
		return nil, fmt.Errorf("generate dummy password hash: %w", err)
	}

	return &Service{repository: repository, tokens: tokens, bcryptCost: cost, dummyHash: hash}, nil
}

func (s *Service) Register(ctx context.Context, input RegisterInput) (User, error) {
	email, err := normalizeEmail(input.Email)
	if err != nil {
		return User{}, err
	}

	name := strings.TrimSpace(input.Name)
	if !utf8.ValidString(name) || name == "" || utf8.RuneCountInString(name) > 255 {
		return User{}, fmt.Errorf("%w: name must contain 1 to 255 characters", ErrInvalidInput)
	}

	if !validPassword(input.Password) {
		return User{}, fmt.Errorf("%w: password must contain at least 8 characters and at most 72 bytes", ErrInvalidInput)
	}

	if err := ctx.Err(); err != nil {
		return User{}, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(input.Password), s.bcryptCost)
	if err != nil {
		return User{}, fmt.Errorf("hash password: %w", err)
	}

	user, err := s.repository.Create(ctx, User{Email: email, Name: name, PasswordHash: string(hash)})
	if err != nil {
		return User{}, fmt.Errorf("register user: %w", err)
	}

	user.PasswordHash = ""
	return user, nil
}

func (s *Service) Login(ctx context.Context, input LoginInput) (TokenResponse, error) {
	email, err := normalizeEmail(input.Email)
	if err != nil || !validPassword(input.Password) {
		return TokenResponse{}, ErrInvalidCredentials
	}

	user, err := s.repository.FindByEmail(ctx, email)
	if errors.Is(err, ErrUserNotFound) {
		// Perform bcrypt work for unknown users as well as incorrect passwords.
		_ = bcrypt.CompareHashAndPassword(s.dummyHash, []byte(input.Password))
		return TokenResponse{}, ErrInvalidCredentials
	}

	if err != nil {
		return TokenResponse{}, fmt.Errorf("find login user: %w", err)
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(input.Password)); err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return TokenResponse{}, ErrInvalidCredentials
		}

		return TokenResponse{}, fmt.Errorf("verify stored password hash: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return TokenResponse{}, err
	}

	return s.tokens.Issue(user.ID)
}

func normalizeEmail(value string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(value))
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || len(email) > 255 {
		return "", fmt.Errorf("%w: invalid email", ErrInvalidInput)
	}

	for _, char := range email {
		if char <= ' ' || char > '~' {
			return "", fmt.Errorf("%w: email must be an ASCII address", ErrInvalidInput)
		}
	}

	return email, nil
}

func validPassword(password string) bool {
	return utf8.ValidString(password) && utf8.RuneCountInString(password) >= 8 && len(password) <= 72
}
