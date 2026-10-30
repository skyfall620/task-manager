package auth

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func TestClassifyCreateError(t *testing.T) {
	duplicate := fmt.Errorf("driver: %w", &mysql.MySQLError{Number: 1062, Message: "duplicate email"})
	if err := classifyCreateError(duplicate); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate error = %v; want email conflict", err)
	}

	other := &mysql.MySQLError{Number: 1045, Message: "access denied"}
	err := classifyCreateError(other)
	if errors.Is(err, ErrEmailTaken) || !errors.Is(err, other) {
		t.Fatalf("other MySQL error misclassified: %v", err)
	}
}
