package tasks

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/skyfall620/task-manager/internal/teams"
)

type countingListRepository struct {
	TaskRepository
	lists atomic.Int64
}

func (r *countingListRepository) WithTeamTransaction(ctx context.Context, teamID uint64, operation func(TaskTransaction) error) error {
	return r.TaskRepository.WithTeamTransaction(ctx, teamID, func(tx TaskTransaction) error {
		return operation(countingListTransaction{TaskTransaction: tx, lists: &r.lists})
	})
}

type countingListTransaction struct {
	TaskTransaction
	lists *atomic.Int64
}

func (tx countingListTransaction) List(ctx context.Context, filter ListFilter) ([]Task, error) {
	tx.lists.Add(1)
	return tx.TaskTransaction.List(ctx, filter)
}

func runCacheIntegrationTests(t *testing.T, db *sql.DB) {
	t.Helper()
	t.Run("cache-filters-pages-and-invalidation", func(t *testing.T) { testCachedLists(t, db) })
	t.Run("cache-current-membership-and-team-isolation", func(t *testing.T) { testCachedListAccess(t, db) })
	t.Run("cache-no-op-comments-and-rollback", func(t *testing.T) { testCacheUnchangedTasks(t, db) })
	t.Run("cache-outage-and-recovery", func(t *testing.T) { testCachedListRecovery(t, db) })
}

func testCachedLists(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, fixture := taskFixture(t, db)
	_, cache := redisCacheFixture(t)
	repository := &countingListRepository{TaskRepository: NewRepository(db)}
	service := NewServiceWithCache(repository, cache)
	uncached := NewService(NewRepository(db))
	task := createIntegrationTask(t, ctx, service, fixture)
	if _, err := service.Create(ctx, fixture.owner, CreateInput{TeamID: fixture.otherTeam, Title: "Other team"}); err != nil {
		t.Fatal(err)
	}

	filters := []ListFilter{
		{TeamID: fixture.team},
		{TeamID: fixture.team, Status: StatusTodo},
		{TeamID: fixture.team, Status: StatusDone},
		{TeamID: fixture.team, AssigneeID: &fixture.assignee},
		{TeamID: fixture.team, Limit: 1, Offset: 1},
	}
	check := func(filter ListFilter, wantMiss bool) {
		t.Helper()
		before := repository.lists.Load()
		want, err := uncached.List(ctx, fixture.owner, filter)
		if err != nil {
			t.Fatal(err)
		}

		got, err := service.List(ctx, fixture.owner, filter)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("cached and SQL lists differ: %+v, want %+v, error %v", got, want, err)
		}

		increment := int64(0)
		if wantMiss {
			increment = 1
		}

		if repository.lists.Load() != before+increment {
			t.Fatalf("unexpected list SELECT count for %+v; wantMiss=%v", filter, wantMiss)
		}
	}
	other := ListFilter{TeamID: fixture.otherTeam}
	check(other, true)
	for _, filter := range filters {
		check(filter, true)
		check(filter, false)
	}

	input := inputForTask(task)
	input.Status = StatusDone
	input.AssigneeID = nil
	if _, err := service.Update(ctx, fixture.owner, task.ID, input); err != nil {
		t.Fatal(err)
	}

	check(other, false)
	for _, filter := range filters {
		check(filter, true)
		check(filter, false)
	}

	if _, err := service.Create(ctx, fixture.owner, CreateInput{TeamID: fixture.team, Title: "Newest"}); err != nil {
		t.Fatal(err)
	}

	check(other, false)
	for _, filter := range filters {
		check(filter, true)
		check(filter, false)
	}
}

func testCachedListAccess(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, fixture, actors := commentFixture(t, db)
	_, cache := redisCacheFixture(t)
	repository := &countingListRepository{TaskRepository: NewRepository(db)}
	service := NewServiceWithCache(repository, cache)
	task := createIntegrationTask(t, ctx, service, fixture)
	filter := ListFilter{TeamID: fixture.team}
	if list, err := service.List(ctx, actors.member, filter); err != nil || len(list) != 1 || list[0].ID != task.ID {
		t.Fatalf("member could not warm cache: %+v, %v", list, err)
	}

	if list, err := service.List(ctx, actors.outsider, filter); !errors.Is(err, ErrTeamNotFound) || list != nil {
		t.Fatalf("other team member received cached tasks: %+v, %v", list, err)
	}

	if err := teams.NewService(teams.NewRepository(db)).Remove(ctx, fixture.owner, fixture.team, actors.member); err != nil {
		t.Fatal(err)
	}

	if list, err := service.List(ctx, actors.member, filter); !errors.Is(err, ErrTeamNotFound) || list != nil {
		t.Fatalf("removed member received cached tasks: %+v, %v", list, err)
	}

	if list, err := service.List(ctx, fixture.owner, filter); err != nil || len(list) != 1 || repository.lists.Load() != 1 {
		t.Fatalf("current member did not reuse authorized cache: %+v, %v", list, err)
	}
}

func testCacheUnchangedTasks(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, fixture := taskFixture(t, db)
	_, cache := redisCacheFixture(t)
	repository := &countingListRepository{TaskRepository: NewRepository(db)}
	service := NewServiceWithCache(repository, cache)
	task := createIntegrationTask(t, ctx, service, fixture)
	filter := ListFilter{TeamID: fixture.team}
	if _, err := service.List(ctx, fixture.owner, filter); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Update(ctx, fixture.owner, task.ID, inputForTask(task)); err != nil {
		t.Fatal(err)
	}

	stale := inputForTask(task)
	stale.Version++
	if _, err := service.Update(ctx, fixture.owner, task.ID, stale); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("expected version conflict: %v", err)
	}

	if _, err := service.CreateComment(ctx, fixture.assignee, task.ID, CreateCommentInput{Content: "Comment"}); err != nil {
		t.Fatal(err)
	}

	failing := NewServiceWithCache(failingHistoryRepository{
		TaskRepository: repository, changedBy: fixture.missingUser,
	}, cache)
	input := inputForTask(task)
	input.Title = "Must roll back"
	if _, err := failing.Update(ctx, fixture.owner, task.ID, input); err == nil {
		t.Fatal("expected update history failure")
	}

	if _, err := failing.Create(ctx, fixture.owner, CreateInput{TeamID: fixture.team, Title: "Must roll back"}); err == nil {
		t.Fatal("expected creation history failure")
	}

	if list, err := service.List(ctx, fixture.owner, filter); err != nil || len(list) != 1 || list[0].Version != 1 || repository.lists.Load() != 1 {
		t.Fatalf("unchanged task invalidated cache: %+v, %v", list, err)
	}

	if !reflect.DeepEqual(persistedTask(t, ctx, db, task.ID), task) {
		t.Fatal("rollback or comment changed persisted task")
	}

	if entries, err := service.History(ctx, fixture.owner, task.ID, HistoryFilter{}); err != nil || len(entries) != 1 {
		t.Fatalf("unexpected committed task history: %d, %v", len(entries), err)
	}
}

func testCachedListRecovery(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, fixture := taskFixture(t, db)
	_, cache := redisCacheFixture(t)
	repository := &countingListRepository{TaskRepository: NewRepository(db)}
	service := NewServiceWithCache(repository, cache)
	task := createIntegrationTask(t, ctx, service, fixture)
	filter := ListFilter{TeamID: fixture.team}
	if _, err := service.List(ctx, fixture.owner, filter); err != nil {
		t.Fatal(err)
	}

	old, err := cache.Load(ctx, filter)
	if err != nil || !old.Found {
		t.Fatalf("could not warm cache: %v", err)
	}

	client := cache.client
	closed := redis.NewClient(&redis.Options{Addr: "unused", MaxRetries: -1})
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}

	cache.client = closed
	defer func() { cache.client = client }()
	input := inputForTask(task)
	input.Title = "Changed while Redis unavailable"
	updated, err := service.Update(ctx, fixture.owner, task.ID, input)
	if err != nil || updated.Version != 2 {
		t.Fatalf("Redis outage failed committed update: %+v, %v", updated, err)
	}

	if list, err := service.List(ctx, fixture.owner, filter); err != nil || len(list) != 1 || list[0].Title != input.Title || repository.lists.Load() != 2 {
		t.Fatalf("Redis outage did not fall back to MySQL: %+v, %v", list, err)
	}

	cache.client = client
	for range 2 {
		if list, err := service.List(ctx, fixture.owner, filter); err != nil || len(list) != 1 || list[0].Version != 2 || list[0].Title != input.Title {
			t.Fatalf("recovery returned stale tasks: %+v, %v", list, err)
		}
	}

	if repository.lists.Load() != 3 {
		t.Fatal("recovery did not refill cache once and reuse it")
	}

	if exists, err := client.Exists(ctx, old.key).Result(); err != nil || exists != 1 {
		t.Fatalf("recovery test must retain the stale entry in its isolated namespace: %d, %v", exists, err)
	}
}
