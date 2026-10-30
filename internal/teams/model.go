package teams

import (
	"errors"
	"time"
)

type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

var (
	ErrInvalidInput    = errors.New("invalid input")
	ErrUnauthenticated = errors.New("authentication required")
	ErrForbidden       = errors.New("insufficient permissions")
	ErrTeamNotFound    = errors.New("team not found")
	ErrMemberNotFound  = errors.New("member not found")
	ErrUserNotFound    = errors.New("user not found")
	ErrAlreadyMember   = errors.New("user already belongs to team")
	ErrAssignedTasks   = errors.New("member has assigned tasks")
)

type Team struct {
	ID        uint64    `json:"id"`
	Name      string    `json:"name"`
	CreatedBy uint64    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

type Member struct {
	TeamID uint64 `json:"team_id"`
	UserID uint64 `json:"user_id"`
	Role   Role   `json:"role"`
}
