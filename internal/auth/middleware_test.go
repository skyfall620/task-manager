package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func testTokens(t *testing.T) *TokenManager {
	t.Helper()
	manager, err := NewTokenManager(strings.Repeat("a", 32), "test-issuer", "test-audience", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	return manager
}

func TestMiddlewareValidToken(t *testing.T) {
	manager := testTokens(t)
	token, err := manager.Issue(42)
	if err != nil {
		t.Fatal(err)
	}

	for _, scheme := range []string{"Bearer", "bearer"} {
		t.Run(scheme, func(t *testing.T) {
			called := false
			handler := manager.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				id, ok := UserIDFromContext(r.Context())
				if !ok || id != 42 {
					t.Errorf("user ID = %d, %v; want 42, true", id, ok)
				}

				w.WriteHeader(http.StatusNoContent)
			}))
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set("Authorization", scheme+" "+token.AccessToken)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if !called || response.Code != http.StatusNoContent {
				t.Fatalf("called = %v, status = %d", called, response.Code)
			}
		})
	}

	if _, ok := UserIDFromContext(context.Background()); ok {
		t.Fatal("empty context contains user ID")
	}
}

func TestMiddlewareRejectsInvalidTokens(t *testing.T) {
	manager := testTokens(t)
	makeToken := func(change func(*jwt.RegisteredClaims), method jwt.SigningMethod, secret any) string {
		t.Helper()
		now := time.Now()
		claims := jwt.RegisteredClaims{
			Issuer: "test-issuer", Audience: jwt.ClaimStrings{"test-audience"}, Subject: "42",
			IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)), ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		}
		if change != nil {
			change(&claims)
		}

		value, err := jwt.NewWithClaims(method, claims).SignedString(secret)
		if err != nil {
			t.Fatal(err)
		}

		return value
	}
	signed := func(change func(*jwt.RegisteredClaims)) string {
		return makeToken(change, jwt.SigningMethodHS256, manager.secret)
	}
	valid := signed(nil)
	tests := []struct {
		name    string
		headers []string
	}{
		{"missing header", nil},
		{"empty bearer", []string{"Bearer"}},
		{"basic scheme", []string{"Basic " + valid}},
		{"extra fields", []string{"Bearer " + valid + " extra"}},
		{"duplicate headers", []string{"Bearer " + valid, "Bearer " + valid}},
		{"malformed token", []string{"Bearer invalid"}},
		{"bad signature", []string{"Bearer " + makeToken(nil, jwt.SigningMethodHS256, []byte(strings.Repeat("b", 32)))}},
		{"wrong algorithm", []string{"Bearer " + makeToken(nil, jwt.SigningMethodHS384, manager.secret)}},
		{"unsigned token", []string{"Bearer " + makeToken(nil, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType)}},
		{"expired", []string{"Bearer " + signed(func(c *jwt.RegisteredClaims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour)) })}},
		{"missing expiration", []string{"Bearer " + signed(func(c *jwt.RegisteredClaims) { c.ExpiresAt = nil })}},
		{"wrong issuer", []string{"Bearer " + signed(func(c *jwt.RegisteredClaims) { c.Issuer = "other" })}},
		{"missing issuer", []string{"Bearer " + signed(func(c *jwt.RegisteredClaims) { c.Issuer = "" })}},
		{"wrong audience", []string{"Bearer " + signed(func(c *jwt.RegisteredClaims) { c.Audience = jwt.ClaimStrings{"other"} })}},
		{"missing audience", []string{"Bearer " + signed(func(c *jwt.RegisteredClaims) { c.Audience = nil })}},
		{"missing subject", []string{"Bearer " + signed(func(c *jwt.RegisteredClaims) { c.Subject = "" })}},
		{"zero subject", []string{"Bearer " + signed(func(c *jwt.RegisteredClaims) { c.Subject = "0" })}},
		{"negative subject", []string{"Bearer " + signed(func(c *jwt.RegisteredClaims) { c.Subject = "-1" })}},
		{"invalid subject", []string{"Bearer " + signed(func(c *jwt.RegisteredClaims) { c.Subject = "user" })}},
		{"overflow subject", []string{"Bearer " + signed(func(c *jwt.RegisteredClaims) { c.Subject = "18446744073709551616" })}},
		{"future issued at", []string{"Bearer " + signed(func(c *jwt.RegisteredClaims) { c.IssuedAt = jwt.NewNumericDate(time.Now().Add(time.Hour)) })}},
		{"future not before", []string{"Bearer " + signed(func(c *jwt.RegisteredClaims) { c.NotBefore = jwt.NewNumericDate(time.Now().Add(time.Hour)) })}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			handler := manager.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
			request := httptest.NewRequest(http.MethodGet, "/?access_token="+valid, nil)
			for _, header := range test.headers {
				request.Header.Add("Authorization", header)
			}

			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if called || response.Code != http.StatusUnauthorized {
				t.Fatalf("called = %v, status = %d; want false, 401", called, response.Code)
			}

			if response.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Error("missing bearer challenge")
			}

			if strings.Contains(response.Body.String(), valid) {
				t.Error("response exposes token")
			}
		})
	}
}

func TestTokenManagerConfiguration(t *testing.T) {
	for _, secret := range []string{"", strings.Repeat("a", 31), strings.Repeat(" ", 32)} {
		if _, err := NewTokenManager(secret, "issuer", "audience", time.Hour); err == nil {
			t.Error("accepted weak secret")
		}
	}

	if _, err := NewTokenManager(strings.Repeat("a", 32), "", "audience", time.Hour); err == nil {
		t.Error("accepted empty issuer")
	}

	if _, err := NewTokenManager(strings.Repeat("a", 32), "issuer", "", time.Hour); err == nil {
		t.Error("accepted empty audience")
	}

	if _, err := NewTokenManager(strings.Repeat("a", 32), "issuer", "audience", time.Millisecond); err == nil {
		t.Error("accepted subsecond TTL")
	}

	if _, err := testTokens(t).Issue(0); err == nil {
		t.Error("issued token for zero user ID")
	}
}
