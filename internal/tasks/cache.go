package tasks

import (
	"context"
	"log/slog"
	"time"
)

const (
	TaskListCacheTTL      = 5 * time.Minute
	cacheOperationTimeout = 200 * time.Millisecond
)

// ListCacheEntry contains a cached list or an opaque ticket for a later fill.
// Found distinguishes a cached empty list from a miss; the unexported fields
// bind Store to the namespace, team and generation observed by Load.
type ListCacheEntry struct {
	Tasks      []Task
	Found      bool
	key        string
	namespace  string
	generation string
	teamID     uint64
}

// ListCache provides optional task-list caching; callers may fall back to MySQL
// when cache operations fail.
type ListCache interface {
	// Load returns a hit or a fill ticket on a miss. A cached empty list is a hit.
	Load(context.Context, ListFilter) (ListCacheEntry, error)
	// Store may ignore a ticket invalidated since Load without returning an error.
	Store(context.Context, ListCacheEntry) error
	// Invalidate invalidates all filter and pagination variants for a team.
	Invalidate(context.Context, uint64) error
}

func NewServiceWithCache(repository TaskRepository, cache ListCache) *Service {
	return &Service{repository: repository, cache: cache}
}

func (s *Service) authorizeList(ctx context.Context, actorID, teamID uint64) error {
	return s.repository.WithTeamTransaction(ctx, teamID, func(tx TaskTransaction) error {
		return authorizeMember(ctx, tx, actorID)
	})
}

func (s *Service) loadCachedList(ctx context.Context, actorID uint64, filter ListFilter) (ListCacheEntry, error) {
	if s.cache == nil {
		return ListCacheEntry{}, nil
	}

	if err := s.authorizeList(ctx, actorID, filter.TeamID); err != nil {
		return ListCacheEntry{}, err
	}

	cacheCtx, cancel := context.WithTimeout(ctx, cacheOperationTimeout)
	entry, err := s.cache.Load(cacheCtx, filter)
	cancel()
	if err != nil {
		slog.WarnContext(ctx, "task list cache read failed", "team_id", filter.TeamID, "error", err)
		entry = ListCacheEntry{}
	}

	if entry.Found {
		// Redis is outside both transactions. Recheck membership before using a hit.
		if err := s.authorizeList(ctx, actorID, filter.TeamID); err != nil {
			return ListCacheEntry{}, err
		}

		if entry.Tasks == nil {
			entry.Tasks = []Task{}
		}
	}

	return entry, nil
}

func (s *Service) storeCachedList(ctx context.Context, teamID uint64, entry ListCacheEntry, tasks []Task) {
	if s.cache == nil || entry.key == "" {
		return
	}

	entry.Tasks = tasks
	cacheCtx, cancel := context.WithTimeout(ctx, cacheOperationTimeout)
	defer cancel()
	if err := s.cache.Store(cacheCtx, entry); err != nil {
		slog.WarnContext(ctx, "task list cache write failed", "team_id", teamID, "error", err)
	}
}

func (s *Service) invalidateTaskLists(ctx context.Context, teamID uint64) {
	if s.cache == nil {
		return
	}

	// A disconnected client must not cancel invalidation after a successful commit.
	cacheCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cacheOperationTimeout)
	defer cancel()
	if err := s.cache.Invalidate(cacheCtx, teamID); err != nil {
		slog.WarnContext(ctx, "task list cache invalidation failed", "team_id", teamID, "error", err)
	}
}
