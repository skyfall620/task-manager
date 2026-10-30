package auth

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type TokenManager struct {
	secret   []byte
	issuer   string
	audience string
	ttl      time.Duration
}

func NewTokenManager(secret, issuer, audience string, ttl time.Duration) (*TokenManager, error) {
	if len(strings.TrimSpace(secret)) < 32 {
		return nil, fmt.Errorf("JWT_SECRET must contain at least 32 bytes excluding surrounding whitespace")
	}

	if strings.TrimSpace(issuer) == "" || strings.TrimSpace(audience) == "" {
		return nil, fmt.Errorf("JWT issuer and audience must not be empty")
	}

	if ttl < time.Second {
		return nil, fmt.Errorf("JWT TTL must be at least one second")
	}

	return &TokenManager{secret: []byte(secret), issuer: issuer, audience: audience, ttl: ttl}, nil
}

func (m *TokenManager) Issue(userID uint64) (TokenResponse, error) {
	if userID == 0 {
		return TokenResponse{}, fmt.Errorf("cannot issue token for zero user ID")
	}

	now := time.Now().UTC().Truncate(time.Second)
	claims := jwt.RegisteredClaims{
		Issuer:    m.issuer,
		Subject:   strconv.FormatUint(userID, 10),
		Audience:  jwt.ClaimStrings{m.audience},
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(m.ttl)),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
	if err != nil {
		return TokenResponse{}, fmt.Errorf("sign access token: %w", err)
	}

	return TokenResponse{AccessToken: token, TokenType: "Bearer", ExpiresIn: int64(m.ttl / time.Second)}, nil
}

func (m *TokenManager) Validate(value string) (uint64, error) {
	claims := &jwt.RegisteredClaims{}
	token, err := jwt.ParseWithClaims(value, claims, func(token *jwt.Token) (any, error) {
		return m.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(), jwt.WithIssuer(m.issuer), jwt.WithAudience(m.audience), jwt.WithIssuedAt())
	if err != nil || !token.Valid {
		return 0, ErrInvalidToken
	}

	id, err := strconv.ParseUint(claims.Subject, 10, 64)
	if err != nil || id == 0 {
		return 0, ErrInvalidToken
	}

	return id, nil
}
