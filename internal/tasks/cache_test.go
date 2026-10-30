package tasks

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
)

type listCacheFake struct {
	repository    *repositoryFake
	entry         ListCacheEntry
	loadErr       error
	storeErr      error
	invalidateErr error
	loads         int
	stores        int
	invalidated   []uint64
	lastFilter    ListFilter
	onLoad        func()
	onInvalidate  func(context.Context)
}

func (c *listCacheFake) checkOutsideTransaction(ctx context.Context) {
	if c.repository.active {
		panic("Redis operation inside MySQL transaction")
	}

	if _, ok := ctx.Deadline(); !ok {
		panic("cache operation has no deadline")
	}
}

func (c *listCacheFake) Load(ctx context.Context, filter ListFilter) (ListCacheEntry, error) {
	c.checkOutsideTransaction(ctx)
	c.loads++
	c.lastFilter = filter
	if c.onLoad != nil {
		c.onLoad()
	}

	return c.entry, c.loadErr
}

func (c *listCacheFake) Store(ctx context.Context, entry ListCacheEntry) error {
	c.checkOutsideTransaction(ctx)
	c.stores++
	if c.storeErr != nil {
		return c.storeErr
	}

	c.entry = entry
	c.entry.Tasks = slices.Clone(entry.Tasks)
	c.entry.Found = true
	return nil
}

func (c *listCacheFake) Invalidate(ctx context.Context, teamID uint64) error {
	c.checkOutsideTransaction(ctx)
	c.invalidated = append(c.invalidated, teamID)
	if c.onInvalidate != nil {
		c.onInvalidate(ctx)
	}

	return c.invalidateErr
}

func TestCachedListMissHitAndEmpty(t *testing.T) {
	for _, empty := range []bool{false, true} {
		repository := newRepositoryFake()
		if !empty {
			repository.listResult = []Task{{ID: 1, TeamID: 10, Status: StatusTodo, Version: 1}}
		}

		cache := &listCacheFake{repository: repository, entry: ListCacheEntry{key: "ticket"}}
		service := NewServiceWithCache(repository, cache)
		first, err := service.List(context.Background(), 1, ListFilter{TeamID: 10})
		if err != nil || first == nil || repository.listCalls != 1 || cache.stores != 1 || cache.lastFilter.Limit != DefaultLimit {
			t.Fatalf("unexpected cache miss: %+v, %v", first, err)
		}

		second, err := service.List(context.Background(), 3, ListFilter{TeamID: 10})
		if err != nil || !reflect.DeepEqual(first, second) || repository.listCalls != 1 || cache.loads != 2 || cache.stores != 1 {
			t.Fatalf("cache hit did not avoid list SELECT: %+v, %v", second, err)
		}
	}
}

func TestCachedListAlwaysChecksCurrentAccess(t *testing.T) {
	for _, hit := range []bool{false, true} {
		repository := newRepositoryFake()
		cache := &listCacheFake{
			repository: repository,
			entry:      ListCacheEntry{Found: hit, key: "ticket", Tasks: []Task{{ID: 1, TeamID: 10}}},
		}
		cache.onLoad = func() { delete(repository.members, 3) }
		result, err := NewServiceWithCache(repository, cache).List(context.Background(), 3, ListFilter{TeamID: 10})
		if !errors.Is(err, ErrTeamNotFound) || result != nil || repository.listCalls != 0 || cache.stores != 0 {
			t.Fatalf("removed member received cached or uncached tasks: %+v, %v", result, err)
		}
	}

	repository := newRepositoryFake()
	cache := &listCacheFake{repository: repository, entry: ListCacheEntry{Found: true}}
	if _, err := NewServiceWithCache(repository, cache).List(context.Background(), 99, ListFilter{TeamID: 10}); !errors.Is(err, ErrTeamNotFound) || cache.loads != 0 {
		t.Fatal("unauthorized request reached cache")
	}

	databaseErr := errors.New("database unavailable")
	unavailable := failingTransactionRepository{TaskRepository: repository, err: databaseErr}
	if _, err := NewServiceWithCache(unavailable, cache).List(context.Background(), 1, ListFilter{TeamID: 10}); !errors.Is(err, databaseErr) || cache.loads != 0 {
		t.Fatal("cache bypassed unavailable MySQL authorization")
	}
}

type failingTransactionRepository struct {
	TaskRepository
	err error
}

func (r failingTransactionRepository) WithTeamTransaction(context.Context, uint64, func(TaskTransaction) error) error {
	return r.err
}

func TestCacheReadAndStoreFailuresUseMySQL(t *testing.T) {
	for _, failure := range []string{"load", "store"} {
		repository := newRepositoryFake()
		repository.listResult = []Task{{ID: 1, TeamID: 10}}
		cache := &listCacheFake{repository: repository, entry: ListCacheEntry{key: "ticket"}}
		if failure == "load" {
			cache.loadErr = errors.New("Redis unavailable")
		} else {
			cache.storeErr = errors.New("Redis write failed")
		}

		result, err := NewServiceWithCache(repository, cache).List(context.Background(), 1, ListFilter{TeamID: 10})
		if err != nil || !reflect.DeepEqual(result, repository.listResult) || repository.listCalls != 1 {
			t.Fatalf("cache %s failure affected API result: %v", failure, err)
		}
	}
}

func TestCacheInvalidationAfterSuccessfulWrites(t *testing.T) {
	repository, task := updateFixture(t)
	cache := &listCacheFake{repository: repository, invalidateErr: errors.New("Redis unavailable")}
	service := NewServiceWithCache(repository, cache)
	input := inputForTask(task)
	input.Title = "Updated"
	updated, err := service.Update(context.Background(), 1, task.ID, input)
	if err != nil || updated.Version != 2 || !reflect.DeepEqual(cache.invalidated, []uint64{10}) || len(repository.history) != 2 {
		t.Fatalf("failed Redis invalidation affected committed update: %+v, %v", updated, err)
	}

	created, err := service.Create(context.Background(), 1, CreateInput{TeamID: 10, Title: "Created"})
	if err != nil || created.ID == 0 || len(cache.invalidated) != 2 || cache.invalidated[1] != 10 {
		t.Fatalf("create did not invalidate after commit: %+v, %v", created, err)
	}
}

func TestCacheNotInvalidatedForNoOpConflictCommentsOrRollback(t *testing.T) {
	repository, task := updateFixture(t)
	cache := &listCacheFake{repository: repository}
	service := NewServiceWithCache(repository, cache)
	ctx := context.Background()
	if _, err := service.Update(ctx, 1, task.ID, inputForTask(task)); err != nil {
		t.Fatal(err)
	}

	input := inputForTask(task)
	input.Version++
	if _, err := service.Update(ctx, 1, task.ID, input); !errors.Is(err, ErrVersionConflict) {
		t.Fatal("expected version conflict")
	}

	if _, err := service.CreateComment(ctx, 5, task.ID, CreateCommentInput{Content: "Comment"}); err != nil {
		t.Fatal(err)
	}

	repository.historyErr = errors.New("history failed")
	input = inputForTask(task)
	input.Title = "Must roll back"
	if _, err := service.Update(ctx, 1, task.ID, input); err == nil {
		t.Fatal("expected history error")
	}

	if _, err := service.Create(ctx, 1, CreateInput{TeamID: 10, Title: "Must roll back"}); err == nil {
		t.Fatal("expected creation history error")
	}

	if len(cache.invalidated) != 0 {
		t.Fatal("unchanged, commented or rolled-back task invalidated cache")
	}
}

type afterTransactionRepository struct {
	TaskRepository
	after func()
	err   error
}

func (r afterTransactionRepository) WithTeamTransaction(ctx context.Context, teamID uint64, operation func(TaskTransaction) error) error {
	if err := r.TaskRepository.WithTeamTransaction(ctx, teamID, operation); err != nil {
		return err
	}

	if r.after != nil {
		r.after()
	}

	return r.err
}

func TestInvalidationSurvivesCanceledRequestAndSkipsCommitFailure(t *testing.T) {
	repository := newRepositoryFake()
	cache := &listCacheFake{repository: repository}
	cache.onInvalidate = func(ctx context.Context) {
		if ctx.Err() != nil {
			t.Fatal("request cancellation prevented invalidation")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wrapped := afterTransactionRepository{TaskRepository: repository, after: cancel}
	if _, err := NewServiceWithCache(wrapped, cache).Create(ctx, 1, CreateInput{TeamID: 10, Title: "Task"}); err != nil || len(cache.invalidated) != 1 {
		t.Fatalf("committed creation failed to invalidate: %v", err)
	}

	cache.invalidated = nil
	commitErr := errors.New("commit failed")
	wrapped = afterTransactionRepository{TaskRepository: repository, err: commitErr}
	if _, err := NewServiceWithCache(wrapped, cache).Create(context.Background(), 1, CreateInput{TeamID: 10, Title: "Task"}); !errors.Is(err, commitErr) || len(cache.invalidated) != 0 {
		t.Fatal("failed commit invalidated cache")
	}
}

func TestCacheKeySeparatesFiltersAndGenerations(t *testing.T) {
	cache := NewRedisListCache(nil, "test-prefix")
	assignee := uint64(4)
	filters := []ListFilter{
		{TeamID: 10, Limit: DefaultLimit},
		{TeamID: 11, Limit: DefaultLimit},
		{TeamID: 10, Status: StatusTodo, Limit: DefaultLimit},
		{TeamID: 10, AssigneeID: &assignee, Limit: DefaultLimit},
		{TeamID: 10, Limit: 1},
		{TeamID: 10, Limit: DefaultLimit, Offset: 1},
	}
	keys := map[string]bool{}
	for _, filter := range filters {
		key := cache.listKey("namespace", "generation", filter)
		if keys[key] {
			t.Fatal("different task filters share a cache key")
		}

		keys[key] = true
	}

	if keys[cache.listKey("new-namespace", "generation", filters[0])] || keys[cache.listKey("namespace", "new-generation", filters[0])] {
		t.Fatal("namespace or generation is missing from the cache key")
	}
}
