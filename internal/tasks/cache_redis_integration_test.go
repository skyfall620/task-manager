package tasks

import (
	"context"
	"crypto/rand"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func redisCacheFixture(t *testing.T) (context.Context, *RedisListCache) {
	t.Helper()
	addr := os.Getenv("TASKS_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set TASKS_TEST_REDIS_ADDR to run Redis integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	client := redis.NewClient(&redis.Options{
		Addr: addr, MaxRetries: -1, ContextTimeoutEnabled: true,
		DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second,
	})
	prefix := "task-manager:tests:" + rand.Text()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		// Only this fixture's random prefix is inspected or removed.
		iterator := client.Scan(cleanupCtx, 0, prefix+":*", 100).Iterator()
		for iterator.Next(cleanupCtx) {
			if err := client.Del(cleanupCtx, iterator.Val()).Err(); err != nil {
				t.Error(err)
			}
		}

		if err := iterator.Err(); err != nil {
			t.Error(err)
		}

		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}

	return ctx, NewRedisListCache(client, prefix)
}

func cacheMiss(t *testing.T, ctx context.Context, cache *RedisListCache, filter ListFilter) ListCacheEntry {
	t.Helper()
	entry, err := cache.Load(ctx, filter)
	if err != nil || entry.Found || entry.key == "" {
		t.Fatalf("expected cache miss with a fill ticket: %+v, %v", entry, err)
	}

	return entry
}

func storeCacheEntry(t *testing.T, ctx context.Context, cache *RedisListCache, entry ListCacheEntry, tasks []Task) {
	t.Helper()
	entry.Tasks = tasks
	if err := cache.Store(ctx, entry); err != nil {
		t.Fatal(err)
	}
}

func cachedTask(teamID uint64) Task {
	return Task{ID: 1, TeamID: teamID, Title: "Cached", Status: StatusTodo, Version: 1}
}

// Pause one reader after Redis has returned its generation, before Load reads the list.
type generationReadPauseHook struct {
	paused   atomic.Bool
	observed chan struct{}
	resume   <-chan struct{}
}

func (h *generationReadPauseHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (h *generationReadPauseHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *generationReadPauseHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err := next(ctx, cmd); err != nil {
			return err
		}

		args := cmd.Args()
		if cmd.Name() != "eval" || len(args) < 2 || args[1] != loadGenerationScript || !h.paused.CompareAndSwap(false, true) {
			return nil
		}

		close(h.observed)
		select {
		case <-h.resume:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func TestRedisListCacheIntegration(t *testing.T) {
	t.Run("ttl-empty-list-and-filters", func(t *testing.T) {
		ctx, cache := redisCacheFixture(t)
		filter := ListFilter{TeamID: 10, Limit: DefaultLimit}
		entry := cacheMiss(t, ctx, cache, filter)
		storeCacheEntry(t, ctx, cache, entry, []Task{})
		hit, err := cache.Load(ctx, filter)
		if err != nil || !hit.Found || hit.Tasks == nil || len(hit.Tasks) != 0 {
			t.Fatalf("empty list was not cached: %+v, %v", hit, err)
		}

		ttl, err := cache.client.PTTL(ctx, entry.key).Result()
		if err != nil || ttl <= 4*time.Minute || ttl > TaskListCacheTTL {
			t.Fatalf("unexpected list TTL: %v, %v", ttl, err)
		}

		assignee := uint64(4)
		for _, other := range []ListFilter{
			{TeamID: 11}, {TeamID: 10, Status: StatusDone},
			{TeamID: 10, AssigneeID: &assignee}, {TeamID: 10, Limit: 1},
			{TeamID: 10, Offset: 1},
		} {
			cacheMiss(t, ctx, cache, other)
		}

		// Expire our own key immediately instead of waiting five minutes.
		if err := cache.client.PExpire(ctx, entry.key, 0).Err(); err != nil {
			t.Fatal(err)
		}

		cacheMiss(t, ctx, cache, filter)
	})
	t.Run("team-invalidation-rejects-late-fill", func(t *testing.T) {
		ctx, cache := redisCacheFixture(t)
		filter := ListFilter{TeamID: 10}
		old := cacheMiss(t, ctx, cache, filter)
		storeCacheEntry(t, ctx, cache, old, []Task{cachedTask(10)})
		page := ListFilter{TeamID: 10, Limit: 1, Offset: 1}
		pending := cacheMiss(t, ctx, cache, page)
		other := cacheMiss(t, ctx, cache, ListFilter{TeamID: 11})
		storeCacheEntry(t, ctx, cache, other, []Task{cachedTask(11)})
		if err := cache.Invalidate(ctx, 10); err != nil {
			t.Fatal(err)
		}

		storeCacheEntry(t, ctx, cache, pending, []Task{})
		fresh := cacheMiss(t, ctx, cache, filter)
		cacheMiss(t, ctx, cache, page)
		if fresh.generation == old.generation {
			t.Fatal("invalidation reused the old team generation")
		}

		if exists, err := cache.client.Exists(ctx, pending.key).Result(); err != nil || exists != 0 {
			t.Fatalf("late reader stored an obsolete page: %d, %v", exists, err)
		}

		hit, err := cache.Load(ctx, ListFilter{TeamID: 11})
		if err != nil || !hit.Found {
			t.Fatalf("another team's cache was invalidated: %v", err)
		}
	})
	t.Run("team-invalidation-rejects-stale-hit", func(t *testing.T) {
		ctx, cache := redisCacheFixture(t)
		filter := ListFilter{TeamID: 10}
		old := cacheMiss(t, ctx, cache, filter)
		storeCacheEntry(t, ctx, cache, old, []Task{cachedTask(10)})

		resume := make(chan struct{})
		hook := &generationReadPauseHook{observed: make(chan struct{}), resume: resume}
		cache.client.AddHook(hook)
		loadCtx, cancelLoad := context.WithCancel(ctx)
		type loadResult struct {
			entry ListCacheEntry
			err   error
		}
		results := make(chan loadResult, 1)
		loadDone := make(chan struct{})
		go func() {
			defer close(loadDone)
			entry, err := cache.Load(loadCtx, filter)
			results <- loadResult{entry: entry, err: err}
		}()
		defer func() {
			cancelLoad()
			<-loadDone
		}()

		select {
		case <-hook.observed:
		case result := <-results:
			t.Fatalf("Load completed before the generation pause: %v", result.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}

		if err := cache.Invalidate(ctx, filter.TeamID); err != nil {
			t.Fatal(err)
		}

		if exists, err := cache.client.Exists(ctx, old.key).Result(); err != nil || exists != 1 {
			t.Fatalf("the old list must remain present during the race: %d, %v", exists, err)
		}

		close(resume)
		select {
		case result := <-results:
			if result.err != nil || result.entry.Found || result.entry.Tasks != nil {
				t.Fatalf("invalidated generation did not produce a cache miss: %+v, %v", result.entry, result.err)
			}

			if result.entry.generation != old.generation {
				t.Fatal("the paused reader did not observe the old generation")
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}

		fresh := cacheMiss(t, ctx, cache, filter)
		if fresh.generation == old.generation {
			t.Fatal("invalidation reused the old team generation")
		}

		updated := cachedTask(10)
		updated.Version = 2
		storeCacheEntry(t, ctx, cache, fresh, []Task{updated})
		hit, err := cache.Load(ctx, filter)
		if err != nil || !hit.Found || len(hit.Tasks) != 1 || hit.Tasks[0].Version != 2 {
			t.Fatalf("current generation did not serve the updated list: %+v, %v", hit, err)
		}
	})
	t.Run("restart-never-reuses-old-namespace", func(t *testing.T) {
		ctx, cache := redisCacheFixture(t)
		filter := ListFilter{TeamID: 10}
		old := cacheMiss(t, ctx, cache, filter)
		storeCacheEntry(t, ctx, cache, old, []Task{cachedTask(10)})
		restarted := NewRedisListCache(cache.client, cache.prefix)
		fresh := cacheMiss(t, ctx, restarted, filter)
		if fresh.namespace == old.namespace {
			t.Fatal("application restart reused a cache namespace")
		}

		if exists, err := cache.client.Exists(ctx, old.key).Result(); err != nil || exists != 1 {
			t.Fatalf("restart test must leave old entries present: %d, %v", exists, err)
		}
	})
	t.Run("failed-invalidation-requires-new-namespace", func(t *testing.T) {
		ctx, cache := redisCacheFixture(t)
		filter := ListFilter{TeamID: 10}
		old := cacheMiss(t, ctx, cache, filter)
		storeCacheEntry(t, ctx, cache, old, []Task{cachedTask(10)})
		client := cache.client
		closed := redis.NewClient(&redis.Options{Addr: "unused", MaxRetries: -1})
		if err := closed.Close(); err != nil {
			t.Fatal(err)
		}

		cache.client = closed
		defer func() { cache.client = client }()
		if err := cache.Invalidate(ctx, 10); err == nil {
			t.Fatal("expected unavailable Redis invalidation to fail")
		}

		if entry, err := cache.Load(ctx, filter); err == nil || entry.Found {
			t.Fatal("cache remained usable before namespace recovery")
		}

		cache.client = client
		fresh := cacheMiss(t, ctx, cache, filter)
		if fresh.namespace == old.namespace {
			t.Fatal("recovery reused stale namespace")
		}

		if exists, err := client.Exists(ctx, old.key).Result(); err != nil || exists != 1 {
			t.Fatalf("stale entry must remain unreachable without deleting other keys: %d, %v", exists, err)
		}

		updated := cachedTask(10)
		updated.Version = 2
		storeCacheEntry(t, ctx, cache, fresh, []Task{updated})
		hit, err := cache.Load(ctx, filter)
		if err != nil || !hit.Found || len(hit.Tasks) != 1 || hit.Tasks[0].Version != 2 {
			t.Fatalf("recovered cache did not serve new data: %+v, %v", hit, err)
		}
	})
	t.Run("expired-generation-and-namespace", func(t *testing.T) {
		ctx, cache := redisCacheFixture(t)
		filter := ListFilter{TeamID: 10}
		old := cacheMiss(t, ctx, cache, filter)
		storeCacheEntry(t, ctx, cache, old, []Task{cachedTask(10)})
		if err := cache.client.Del(ctx, cache.generationKey(old.namespace, 10)).Err(); err != nil {
			t.Fatal(err)
		}

		fresh := cacheMiss(t, ctx, cache, filter)
		if fresh.generation == old.generation {
			t.Fatal("missing generation revived an old cache key")
		}

		if err := cache.client.PExpire(ctx, cache.namespaceKey(), 0).Err(); err != nil {
			t.Fatal(err)
		}

		if entry, err := cache.Load(ctx, filter); err == nil || entry.Found {
			t.Fatal("missing namespace was accepted")
		}

		if recovered := cacheMiss(t, ctx, cache, filter); recovered.namespace == old.namespace {
			t.Fatal("missing namespace was not replaced")
		}
	})
	t.Run("invalidation-timeout-disables-cache", func(t *testing.T) {
		ctx, cache := redisCacheFixture(t)
		filter := ListFilter{TeamID: 10}
		old := cacheMiss(t, ctx, cache, filter)
		storeCacheEntry(t, ctx, cache, old, []Task{cachedTask(10)})
		if err := cache.lockMutation(ctx); err != nil {
			t.Fatal(err)
		}

		canceled, cancel := context.WithCancel(ctx)
		cancel()
		err := cache.Invalidate(canceled, 10)
		cache.unlockMutation()
		if err == nil {
			t.Fatal("expected invalidation waiting for another mutation to fail")
		}

		if fresh := cacheMiss(t, ctx, cache, filter); fresh.namespace == old.namespace {
			t.Fatal("failed invalidation reused old keys despite healthy Redis")
		}
	})
	t.Run("invalid-payload-falls-back", func(t *testing.T) {
		for _, payload := range []string{"{", "null", `[{"id":1,"team_id":999,"status":"todo","version":1}]`} {
			ctx, cache := redisCacheFixture(t)
			filter := ListFilter{TeamID: 10}
			entry := cacheMiss(t, ctx, cache, filter)
			if err := cache.client.Set(ctx, entry.key, payload, TaskListCacheTTL).Err(); err != nil {
				t.Fatal(err)
			}

			if result, err := cache.Load(ctx, filter); err == nil || result.Found {
				t.Fatal("invalid or cross-team cache payload was accepted")
			}

			if fresh := cacheMiss(t, ctx, cache, filter); fresh.namespace == entry.namespace {
				t.Fatal("corrupt payload remained reachable after fallback")
			}
		}
	})
}
