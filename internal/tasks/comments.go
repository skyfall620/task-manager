package tasks

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

func (s *Service) CreateComment(ctx context.Context, actorID, taskID uint64, input CreateCommentInput) (Comment, error) {
	if err := validateTaskActor(actorID, taskID); err != nil {
		return Comment{}, err
	}

	content := strings.TrimSpace(input.Content)
	if !utf8.ValidString(content) || content == "" || utf8.RuneCountInString(content) > MaxCommentLength {
		return Comment{}, fmt.Errorf("%w: content must contain 1 to 10000 Unicode characters after trimming whitespace", ErrInvalidInput)
	}

	var result Comment
	err := s.withTaskTeam(ctx, taskID, func(tx TaskTransaction) error {
		if err := authorizeMember(ctx, tx, actorID); err != nil {
			return err
		}

		var err error
		result, err = tx.CreateComment(ctx, Comment{
			TaskID: taskID, UserID: actorID, Content: content,
			CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
		})
		return err
	})
	if err != nil {
		return Comment{}, fmt.Errorf("create task comment: %w", err)
	}

	return result, nil
}

func (s *Service) Comments(ctx context.Context, actorID, taskID uint64, filter CommentsFilter) ([]Comment, error) {
	if err := validateTaskActor(actorID, taskID); err != nil {
		return nil, err
	}

	if filter.Limit == 0 {
		filter.Limit = DefaultLimit
	}

	if filter.Limit < 1 || filter.Limit > MaxLimit || filter.Offset < 0 {
		return nil, fmt.Errorf("%w: limit must be 1 to 100 and offset must be nonnegative", ErrInvalidInput)
	}

	var result []Comment
	err := s.withTaskTeam(ctx, taskID, func(tx TaskTransaction) error {
		if err := authorizeMember(ctx, tx, actorID); err != nil {
			return err
		}

		var err error
		result, err = tx.Comments(ctx, taskID, filter)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list task comments: %w", err)
	}

	if result == nil {
		result = []Comment{}
	}

	return result, nil
}
