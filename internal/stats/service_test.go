package stats

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/skyfall620/task-manager/internal/teams"
)

type repositoryFake struct {
	team           teams.Team
	members        map[uint64]teams.Member
	result         Report
	transactions   int
	reportCalls    int
	lastAsOf       time.Time
	active         bool
	before         func()
	transactionErr error
	memberErr      error
	reportErr      error
	commitErr      error
}

func newRepositoryFake() *repositoryFake {
	return &repositoryFake{
		team: teams.Team{ID: 10, CreatedBy: 1},
		members: map[uint64]teams.Member{
			1: {TeamID: 10, UserID: 1, Role: teams.RoleOwner},
			2: {TeamID: 10, UserID: 2, Role: teams.RoleAdmin},
			3: {TeamID: 10, UserID: 3, Role: teams.RoleMember},
		},
	}
}

func (f *repositoryFake) WithTeamTransaction(ctx context.Context, teamID uint64, operation func(ReportTransaction) error) error {
	f.transactions++
	if err := ctx.Err(); err != nil {
		return err
	}

	if f.transactionErr != nil {
		return f.transactionErr
	}

	if teamID != f.team.ID {
		return teams.ErrTeamNotFound
	}

	if f.before != nil {
		f.before()
	}

	f.active = true
	defer func() { f.active = false }()
	if err := operation(f); err != nil {
		return err
	}

	return f.commitErr
}

func (f *repositoryFake) requireTransaction() {
	if !f.active {
		panic("stats operation outside transaction")
	}
}

func (f *repositoryFake) Team() teams.Team {
	f.requireTransaction()
	return f.team
}

func (f *repositoryFake) Member(_ context.Context, userID uint64) (teams.Member, error) {
	f.requireTransaction()
	if f.memberErr != nil {
		return teams.Member{}, f.memberErr
	}

	member, ok := f.members[userID]
	if !ok {
		return teams.Member{}, teams.ErrMemberNotFound
	}

	return member, nil
}

func (f *repositoryFake) Report(_ context.Context, asOf time.Time) (Report, error) {
	f.requireTransaction()
	f.reportCalls++
	f.lastAsOf = asOf
	return f.result, f.reportErr
}

func TestReportRoleModel(t *testing.T) {
	for _, test := range []struct {
		name  string
		actor uint64
		role  teams.Role
		want  error
	}{
		{"owner", 1, teams.RoleOwner, nil},
		{"admin", 2, teams.RoleAdmin, nil},
		{"member", 3, teams.RoleMember, teams.ErrForbidden},
		{"outsider", 99, "", teams.ErrTeamNotFound},
		{"owner identity mismatch", 2, teams.RoleOwner, teams.ErrForbidden},
		{"unknown role", 2, "manager", teams.ErrForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := newRepositoryFake()
			if member, ok := repository.members[test.actor]; ok {
				member.Role = test.role
				repository.members[test.actor] = member
			}

			repository.result = Report{TasksByStatus: TaskCounts{Todo: 5}, CommentsCount: 12}
			before := time.Now().UTC().Truncate(time.Microsecond)
			report, err := NewService(repository).Report(context.Background(), test.actor, 10)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v; want %v", err, test.want)
			}

			if test.want != nil {
				if repository.reportCalls != 0 || !reflect.DeepEqual(report, Report{}) {
					t.Fatal("denied request queried or returned report data")
				}

				return
			}

			if report.TeamID != 10 || report.TasksByStatus.Todo != 5 || report.CommentsCount != 12 || report.TopAssignees == nil || repository.reportCalls != 1 {
				t.Fatalf("unexpected report: %+v", report)
			}

			if repository.lastAsOf.Location() != time.UTC || repository.lastAsOf.Before(before) || repository.lastAsOf.After(time.Now().UTC()) {
				t.Fatal("report did not use a current UTC reference time")
			}
		})
	}
}

func TestReportReadsCurrentPermissions(t *testing.T) {
	for _, removed := range []bool{false, true} {
		repository := newRepositoryFake()
		service := NewService(repository)
		if _, err := service.Report(context.Background(), 2, 10); err != nil {
			t.Fatal(err)
		}

		repository.before = func() {
			if removed {
				delete(repository.members, 2)
				return
			}

			member := repository.members[2]
			member.Role = teams.RoleMember
			repository.members[2] = member
		}
		want := teams.ErrForbidden
		if removed {
			want = teams.ErrTeamNotFound
		}

		if _, err := service.Report(context.Background(), 2, 10); !errors.Is(err, want) || repository.reportCalls != 1 {
			t.Fatalf("stale admin permissions were used: %v", err)
		}
	}
}

func TestReportValidationAndTeamIsolation(t *testing.T) {
	for _, test := range []struct {
		actor, team  uint64
		want         error
		transactions int
	}{
		{0, 10, teams.ErrUnauthenticated, 0},
		{1, 0, teams.ErrInvalidInput, 0},
		{1, 99, teams.ErrTeamNotFound, 1},
	} {
		repository := newRepositoryFake()
		if _, err := NewService(repository).Report(context.Background(), test.actor, test.team); !errors.Is(err, test.want) || repository.transactions != test.transactions || repository.reportCalls != 0 {
			t.Fatalf("invalid or cross-team request reached report query: %v", err)
		}
	}
}

func TestReportPropagatesErrorsWithoutPartialData(t *testing.T) {
	for _, failure := range []string{"transaction", "member", "report", "commit"} {
		t.Run(failure, func(t *testing.T) {
			repository := newRepositoryFake()
			repository.result = Report{TasksByStatus: TaskCounts{Done: 10}, CommentsCount: 20}
			want := errors.New("storage failure")
			switch failure {
			case "transaction":
				repository.transactionErr = want
			case "member":
				repository.memberErr = want
			case "report":
				repository.reportErr = want
			case "commit":
				repository.commitErr = want
			}

			report, err := NewService(repository).Report(context.Background(), 1, 10)
			if !errors.Is(err, want) || !reflect.DeepEqual(report, Report{}) {
				t.Fatalf("storage failure leaked partial report or was hidden: %+v, %v", report, err)
			}
		})
	}

	repository := newRepositoryFake()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewService(repository).Report(ctx, 1, 10); !errors.Is(err, context.Canceled) || repository.reportCalls != 0 {
		t.Fatalf("request cancellation was ignored: %v", err)
	}
}
