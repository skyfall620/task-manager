package stats

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/skyfall620/task-manager/internal/teams"
)

// ReportTransaction is usable only during the WithTeamTransaction callback.
// Membership locks are held until the transaction commits or rolls back.
type ReportTransaction interface {
	Team() teams.Team
	Member(context.Context, uint64) (teams.Member, error)
	Report(context.Context, time.Time) (Report, error)
}

type ReportRepository interface {
	WithTeamTransaction(context.Context, uint64, func(ReportTransaction) error) error
}

type Service struct {
	repository ReportRepository
}

func NewService(repository ReportRepository) *Service {
	return &Service{repository: repository}
}

// Report returns team analytics to its owner and admins using current membership.
// Top assignees cover the last 30 days; task counts, comment counts and average
// closing time cover all time. The average is nil if there are no closed tasks.
func (s *Service) Report(ctx context.Context, actorID, teamID uint64) (Report, error) {
	if actorID == 0 {
		return Report{}, teams.ErrUnauthenticated
	}

	if teamID == 0 {
		return Report{}, fmt.Errorf("%w: team_id must be positive", teams.ErrInvalidInput)
	}

	var report Report
	err := s.repository.WithTeamTransaction(ctx, teamID, func(tx ReportTransaction) error {
		member, err := tx.Member(ctx, actorID)
		if errors.Is(err, teams.ErrMemberNotFound) {
			return teams.ErrTeamNotFound
		}

		if err != nil {
			return err
		}

		team := tx.Team()
		if member.TeamID != team.ID || member.UserID != actorID {
			return teams.ErrForbidden
		}

		if member.Role != teams.RoleAdmin && (member.Role != teams.RoleOwner || member.UserID != team.CreatedBy) {
			return teams.ErrForbidden
		}

		report, err = tx.Report(ctx, time.Now().UTC().Truncate(time.Microsecond))
		return err
	})
	if err != nil {
		return Report{}, fmt.Errorf("get team stats: %w", err)
	}

	report.TeamID = teamID
	if report.TopAssignees == nil {
		report.TopAssignees = []AssigneeStats{}
	}

	return report, nil
}
