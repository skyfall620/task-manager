package teams

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type repositoryFake struct {
	team           Team
	members        map[uint64]Member
	users          map[string]uint64
	assigned       map[uint64]bool
	active         bool
	writes         int
	lookups        int
	listedFor      uint64
	before         func()
	transactionErr error
}

func newRepositoryFake() *repositoryFake {
	return &repositoryFake{
		team: Team{ID: 10, Name: "Team", CreatedBy: 1},
		members: map[uint64]Member{
			1: {TeamID: 10, UserID: 1, Role: RoleOwner},
			2: {TeamID: 10, UserID: 2, Role: RoleAdmin},
			3: {TeamID: 10, UserID: 3, Role: RoleMember},
			4: {TeamID: 10, UserID: 4, Role: RoleMember},
		},
		users:    map[string]uint64{"new@example.com": 5, "member@example.com": 4},
		assigned: map[uint64]bool{},
	}
}

func (f *repositoryFake) Create(ctx context.Context, team Team, owner Member) (Team, error) {
	f.writes++
	team.ID = 20
	owner.TeamID = team.ID
	f.team = team
	f.members = map[uint64]Member{owner.UserID: owner}
	return team, nil
}

func (f *repositoryFake) List(ctx context.Context, userID uint64) ([]Team, error) {
	f.listedFor = userID
	if _, ok := f.members[userID]; !ok {
		return nil, nil
	}

	return []Team{f.team}, nil
}

func (f *repositoryFake) WithTeamTransaction(ctx context.Context, teamID uint64, operation func(TeamTransaction) error) error {
	if f.transactionErr != nil {
		return f.transactionErr
	}

	if f.before != nil {
		f.before()
	}

	if f.team.ID != teamID {
		return ErrTeamNotFound
	}

	f.active = true
	defer func() { f.active = false }()
	return operation(f)
}

func (f *repositoryFake) requireTransaction() {
	if !f.active {
		panic("team access outside transaction callback")
	}
}

func (f *repositoryFake) Team() Team {
	f.requireTransaction()
	return f.team
}

func (f *repositoryFake) Member(ctx context.Context, userID uint64) (Member, error) {
	f.requireTransaction()
	member, ok := f.members[userID]
	if !ok {
		return Member{}, ErrMemberNotFound
	}

	return member, nil
}

func (f *repositoryFake) FindUserID(ctx context.Context, email string) (uint64, error) {
	f.requireTransaction()
	f.lookups++
	id, ok := f.users[email]
	if !ok {
		return 0, ErrUserNotFound
	}

	return id, nil
}

func (f *repositoryFake) AddMember(ctx context.Context, member Member) error {
	f.requireTransaction()
	if _, exists := f.members[member.UserID]; exists {
		return ErrAlreadyMember
	}

	f.writes++
	f.members[member.UserID] = member
	return nil
}

func (f *repositoryFake) SetRole(ctx context.Context, userID uint64, role Role) error {
	f.requireTransaction()
	f.writes++
	member := f.members[userID]
	member.Role = role
	f.members[userID] = member
	return nil
}

func (f *repositoryFake) HasAssignedTasks(ctx context.Context, userID uint64) (bool, error) {
	f.requireTransaction()
	return f.assigned[userID], nil
}

func (f *repositoryFake) RemoveMember(ctx context.Context, userID uint64) error {
	f.requireTransaction()
	f.writes++
	delete(f.members, userID)
	return nil
}

func TestCreateAndList(t *testing.T) {
	repository := newRepositoryFake()
	service := NewService(repository)
	team, err := service.Create(context.Background(), 7, " New team ")
	if err != nil || team.CreatedBy != 7 || team.Name != "New team" {
		t.Fatalf("unexpected created team: %+v, %v", team, err)
	}

	owner := repository.members[7]
	if owner.UserID != 7 || owner.Role != RoleOwner || owner.TeamID != team.ID {
		t.Fatalf("invalid creator membership: %+v", owner)
	}

	list, err := service.List(context.Background(), 7)
	if err != nil || len(list) != 1 || repository.listedFor != 7 {
		t.Fatalf("unexpected list: %+v, %v", list, err)
	}

	list, err = service.List(context.Background(), 8)
	if err != nil || list == nil || len(list) != 0 || repository.listedFor != 8 {
		t.Fatalf("expected empty user-scoped list, got %+v, %v", list, err)
	}

	for _, name := range []string{"", " ", strings.Repeat("я", 256)} {
		if _, err := service.Create(context.Background(), 7, name); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("accepted invalid name, error=%v", err)
		}
	}

	if repository.writes != 1 {
		t.Error("invalid create reached repository")
	}
}

func TestInviteRoleModel(t *testing.T) {
	for _, test := range []struct {
		name  string
		actor uint64
		email string
		want  error
	}{
		{"owner", 1, " NEW@EXAMPLE.COM ", nil},
		{"admin", 2, "new@example.com", nil},
		{"member", 3, "new@example.com", ErrForbidden},
		{"outsider", 8, "new@example.com", ErrTeamNotFound},
		{"unknown user", 1, "unknown@example.com", ErrUserNotFound},
		{"already member", 1, "member@example.com", ErrAlreadyMember},
		{"invalid email", 1, "bad", ErrInvalidInput},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := newRepositoryFake()
			member, err := NewService(repository).Invite(context.Background(), test.actor, 10, test.email)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v; want %v", err, test.want)
			}

			if test.want == nil {
				if member.UserID != 5 || member.TeamID != 10 || member.Role != RoleMember || repository.writes != 1 {
					t.Fatalf("invalid invitation: %+v", member)
				}
			} else if repository.writes != 0 {
				t.Fatal("failed invitation modified members")
			}

			if (test.actor == 3 || test.actor == 8) && repository.lookups != 0 {
				t.Fatal("unauthorized user could look up invited users")
			}
		})
	}
}

func TestChangeRoleModel(t *testing.T) {
	for _, test := range []struct {
		name   string
		actor  uint64
		target uint64
		role   Role
		want   error
	}{
		{"promote member", 1, 4, RoleAdmin, nil},
		{"demote admin", 1, 2, RoleMember, nil},
		{"same role", 1, 4, RoleMember, nil},
		{"admin cannot promote", 2, 4, RoleAdmin, ErrForbidden},
		{"admin cannot promote self", 2, 2, RoleAdmin, ErrForbidden},
		{"member cannot promote", 3, 4, RoleAdmin, ErrForbidden},
		{"outsider", 8, 4, RoleAdmin, ErrTeamNotFound},
		{"owner cannot demote self", 1, 1, RoleMember, ErrForbidden},
		{"ownership transfer", 1, 4, RoleOwner, ErrInvalidInput},
		{"unknown role", 1, 4, "manager", ErrInvalidInput},
		{"unknown target", 1, 99, RoleAdmin, ErrMemberNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := newRepositoryFake()
			member, err := NewService(repository).ChangeRole(context.Background(), test.actor, 10, test.target, test.role)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v; want %v", err, test.want)
			}

			if test.want == nil {
				if member.Role != test.role || repository.members[test.target].Role != test.role {
					t.Fatal("role not updated")
				}

				if test.name == "same role" && repository.writes != 0 {
					t.Error("unchanged role should not issue an UPDATE")
				}
			} else if repository.writes != 0 {
				t.Fatal("denied role change modified members")
			}
		})
	}
}

func TestRemoveRoleModel(t *testing.T) {
	for _, test := range []struct {
		name     string
		actor    uint64
		target   uint64
		assigned bool
		want     error
	}{
		{"owner removes member", 1, 4, false, nil},
		{"owner removes admin", 1, 2, false, nil},
		{"admin removes member", 2, 4, false, nil},
		{"admin removes self", 2, 2, false, nil},
		{"member cannot remove", 3, 4, false, ErrForbidden},
		{"member cannot leave", 3, 3, false, ErrForbidden},
		{"outsider", 8, 4, false, ErrTeamNotFound},
		{"owner cannot remove self", 1, 1, false, ErrForbidden},
		{"admin cannot remove owner", 2, 1, false, ErrForbidden},
		{"assigned member", 1, 4, true, ErrAssignedTasks},
		{"assigned self", 2, 2, true, ErrAssignedTasks},
		{"unknown target", 1, 99, false, ErrMemberNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := newRepositoryFake()
			repository.assigned[test.target] = test.assigned
			err := NewService(repository).Remove(context.Background(), test.actor, 10, test.target)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v; want %v", err, test.want)
			}

			if test.want == nil {
				if _, exists := repository.members[test.target]; exists || repository.writes != 1 {
					t.Fatal("member was not removed")
				}
			} else if repository.writes != 0 {
				t.Fatal("denied removal modified members")
			}
		})
	}
}

func TestPermissionsReadInsideTransaction(t *testing.T) {
	for _, state := range []struct {
		name    string
		removed bool
	}{{"demoted", false}, {"removed", true}} {
		for _, operation := range []string{"invite", "remove"} {
			t.Run(operation+"/"+state.name, func(t *testing.T) {
				repository := newRepositoryFake()
				repository.before = func() {
					if state.removed {
						delete(repository.members, 2)
					} else {
						member := repository.members[2]
						member.Role = RoleMember
						repository.members[2] = member
					}
				}
				service := NewService(repository)
				var err error
				if operation == "invite" {
					_, err = service.Invite(context.Background(), 2, 10, "new@example.com")
				} else {
					err = service.Remove(context.Background(), 2, 10, 4)
				}

				want := ErrForbidden
				if state.removed {
					want = ErrTeamNotFound
				}

				if !errors.Is(err, want) || repository.writes != 0 {
					t.Fatalf("stale permissions accepted: error=%v, writes=%d", err, repository.writes)
				}
			})
		}
	}
}

func TestOwnerRoleMustMatchTeamCreator(t *testing.T) {
	repository := newRepositoryFake()
	member := repository.members[2]
	member.Role = RoleOwner
	repository.members[2] = member
	service := NewService(repository)
	ctx := context.Background()
	_, inviteErr := service.Invite(ctx, 2, 10, "new@example.com")
	_, roleErr := service.ChangeRole(ctx, 2, 10, 4, RoleAdmin)
	removeErr := service.Remove(ctx, 2, 10, 4)
	for _, err := range []error{inviteErr, roleErr, removeErr} {
		if !errors.Is(err, ErrForbidden) {
			t.Errorf("owner role without matching creator accepted: %v", err)
		}
	}

	if repository.writes != 0 {
		t.Fatal("invalid owner identity changed members")
	}
}

func TestServicePropagatesTransactionFailure(t *testing.T) {
	repository := newRepositoryFake()
	expected := errors.New("transaction failed")
	repository.transactionErr = expected
	err := NewService(repository).Remove(context.Background(), 1, 10, 4)
	if !errors.Is(err, expected) || repository.writes != 0 {
		t.Fatalf("transaction failure lost: %v", err)
	}
}

func TestTeamIsolationAndUnauthenticatedAccess(t *testing.T) {
	repository := newRepositoryFake()
	service := NewService(repository)
	ctx := context.Background()
	_, createErr := service.Create(ctx, 0, "Team")
	_, listErr := service.List(ctx, 0)
	_, inviteErr := service.Invite(ctx, 0, 10, "new@example.com")
	_, roleErr := service.ChangeRole(ctx, 0, 10, 4, RoleAdmin)
	removeErr := service.Remove(ctx, 0, 10, 4)
	for _, err := range []error{createErr, listErr, inviteErr, roleErr, removeErr} {
		if !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("unauthenticated access error=%v", err)
		}
	}

	_, inviteErr = service.Invite(ctx, 1, 99, "new@example.com")
	_, roleErr = service.ChangeRole(ctx, 1, 99, 4, RoleAdmin)
	removeErr = service.Remove(ctx, 1, 99, 4)
	for _, err := range []error{inviteErr, roleErr, removeErr} {
		if !errors.Is(err, ErrTeamNotFound) {
			t.Errorf("other team access error=%v", err)
		}
	}

	if repository.writes != 0 {
		t.Fatal("unauthenticated or cross-team request changed members")
	}
}
