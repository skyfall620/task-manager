package teams

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"unicode/utf8"
)

// TeamTransaction is usable only during the WithTeamTransaction callback.
// The repository holds the team's row lock until the transaction commits or rolls back.
type TeamTransaction interface {
	Team() Team
	Member(context.Context, uint64) (Member, error)
	FindUserID(context.Context, string) (uint64, error)
	AddMember(context.Context, Member) error
	SetRole(context.Context, uint64, Role) error
	HasAssignedTasks(context.Context, uint64) (bool, error)
	RemoveMember(context.Context, uint64) error
}

type TeamRepository interface {
	Create(context.Context, Team, Member) (Team, error)
	List(context.Context, uint64) ([]Team, error)
	WithTeamTransaction(context.Context, uint64, func(TeamTransaction) error) error
}

type Service struct {
	repository TeamRepository
}

func NewService(repository TeamRepository) *Service {
	return &Service{repository: repository}
}

func (s *Service) Create(ctx context.Context, actorID uint64, name string) (Team, error) {
	if actorID == 0 {
		return Team{}, ErrUnauthenticated
	}

	name = strings.TrimSpace(name)
	if !utf8.ValidString(name) || name == "" || utf8.RuneCountInString(name) > 255 {
		return Team{}, fmt.Errorf("%w: team name must contain 1 to 255 characters", ErrInvalidInput)
	}

	team, err := s.repository.Create(ctx, Team{Name: name, CreatedBy: actorID}, Member{UserID: actorID, Role: RoleOwner})
	if err != nil {
		return Team{}, fmt.Errorf("create team: %w", err)
	}

	return team, nil
}

func (s *Service) List(ctx context.Context, actorID uint64) ([]Team, error) {
	if actorID == 0 {
		return nil, ErrUnauthenticated
	}

	teams, err := s.repository.List(ctx, actorID)
	if err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}

	if teams == nil {
		teams = []Team{}
	}

	return teams, nil
}

func (s *Service) Invite(ctx context.Context, actorID, teamID uint64, email string) (Member, error) {
	if err := validateActorAndTeam(actorID, teamID); err != nil {
		return Member{}, err
	}

	email, err := normalizeEmail(email)
	if err != nil {
		return Member{}, err
	}

	var invited Member
	err = s.repository.WithTeamTransaction(ctx, teamID, func(tx TeamTransaction) error {
		actor, err := currentActor(ctx, tx, actorID)
		if err != nil {
			return err
		}

		if !canManageMembers(tx.Team(), actor) {
			return ErrForbidden
		}

		userID, err := tx.FindUserID(ctx, email)
		if err != nil {
			return err
		}

		invited = Member{TeamID: teamID, UserID: userID, Role: RoleMember}
		return tx.AddMember(ctx, invited)
	})
	if err != nil {
		return Member{}, fmt.Errorf("invite team member: %w", err)
	}

	return invited, nil
}

func (s *Service) ChangeRole(ctx context.Context, actorID, teamID, userID uint64, role Role) (Member, error) {
	if err := validateActorAndTeam(actorID, teamID); err != nil {
		return Member{}, err
	}

	if userID == 0 || (role != RoleAdmin && role != RoleMember) {
		return Member{}, fmt.Errorf("%w: positive user ID and role admin or member required", ErrInvalidInput)
	}

	var updated Member
	err := s.repository.WithTeamTransaction(ctx, teamID, func(tx TeamTransaction) error {
		actor, err := currentActor(ctx, tx, actorID)
		if err != nil {
			return err
		}

		if !isOwner(tx.Team(), actor) {
			return ErrForbidden
		}

		target, err := tx.Member(ctx, userID)
		if err != nil {
			return err
		}

		if target.UserID == tx.Team().CreatedBy || target.Role == RoleOwner {
			return ErrForbidden
		}

		updated = target
		updated.Role = role
		if target.Role == role {
			return nil
		}

		return tx.SetRole(ctx, userID, role)
	})
	if err != nil {
		return Member{}, fmt.Errorf("change team role: %w", err)
	}

	return updated, nil
}

func (s *Service) Remove(ctx context.Context, actorID, teamID, userID uint64) error {
	if err := validateActorAndTeam(actorID, teamID); err != nil {
		return err
	}

	if userID == 0 {
		return fmt.Errorf("%w: positive user ID required", ErrInvalidInput)
	}

	err := s.repository.WithTeamTransaction(ctx, teamID, func(tx TeamTransaction) error {
		actor, err := currentActor(ctx, tx, actorID)
		if err != nil {
			return err
		}

		if !canManageMembers(tx.Team(), actor) {
			return ErrForbidden
		}

		target, err := tx.Member(ctx, userID)
		if err != nil {
			return err
		}

		if target.UserID == tx.Team().CreatedBy || target.Role == RoleOwner {
			return ErrForbidden
		}

		assigned, err := tx.HasAssignedTasks(ctx, userID)
		if err != nil {
			return err
		}

		if assigned {
			return ErrAssignedTasks
		}

		return tx.RemoveMember(ctx, userID)
	})
	if err != nil {
		return fmt.Errorf("remove team member: %w", err)
	}

	return nil
}

func currentActor(ctx context.Context, tx TeamTransaction, actorID uint64) (Member, error) {
	member, err := tx.Member(ctx, actorID)
	if errors.Is(err, ErrMemberNotFound) {
		return Member{}, ErrTeamNotFound
	}

	return member, err
}

func isOwner(team Team, member Member) bool {
	return member.Role == RoleOwner && member.UserID == team.CreatedBy
}

func canManageMembers(team Team, member Member) bool {
	return isOwner(team, member) || member.Role == RoleAdmin
}

func validateActorAndTeam(actorID, teamID uint64) error {
	if actorID == 0 {
		return ErrUnauthenticated
	}

	if teamID == 0 {
		return fmt.Errorf("%w: positive team ID required", ErrInvalidInput)
	}

	return nil
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
