package auth

import (
	"context"
	"net/http"
	"strings"
)

type userIDKey struct{}

func UserIDFromContext(ctx context.Context) (uint64, bool) {
	id, ok := ctx.Value(userIDKey{}).(uint64)
	return id, ok && id != 0
}

func (m *TokenManager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers := r.Header.Values("Authorization")
		if len(headers) != 1 {
			unauthorized(w, r)
			return
		}

		parts := strings.Fields(headers[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			unauthorized(w, r)
			return
		}

		id, err := m.Validate(parts[1])
		if err != nil {
			unauthorized(w, r)
			return
		}

		ctx := context.WithValue(r.Context(), userIDKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func unauthorized(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, r, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
}
