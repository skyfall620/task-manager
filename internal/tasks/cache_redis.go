package tasks

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/redis/go-redis/v9"
)

var errCacheNotReady = errors.New("task list cache namespace is not ready")

const loadGenerationScript = `
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return false end
local generation = redis.call('GET', KEYS[2])
if not generation then
    generation = ARGV[2]
    redis.call('SET', KEYS[2], generation, 'PX', ARGV[3])
end
return generation`

const loadListScript = `
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return false end
if redis.call('GET', KEYS[2]) ~= ARGV[2] then return false end
return redis.call('GET', KEYS[3])`

const storeListScript = `
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
if redis.call('GET', KEYS[2]) ~= ARGV[2] then return 0 end
redis.call('SET', KEYS[3], ARGV[3], 'PX', ARGV[4])
return 1`

const invalidateListScript = `
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
redis.call('SET', KEYS[2], ARGV[2], 'PX', ARGV[3])
return 1`

// RedisListCache maintains task-list generations for one application instance.
// Its prefix must be dedicated to that instance. A fresh namespace is acknowledged
// by Redis on first use or recovery before cache reads can be served.
type RedisListCache struct {
	client    *redis.Client
	prefix    string
	mu        sync.Mutex
	namespace string
	ready     bool
	revision  uint64
	mutation  chan struct{}
}

func NewRedisListCache(client *redis.Client, prefix string) *RedisListCache {
	return &RedisListCache{client: client, prefix: prefix, mutation: make(chan struct{}, 1)}
}

// Load checks the namespace and team generation atomically with reading the list.
// A generation change before that read yields a miss; a list read before
// invalidation may still be returned after invalidation completes.
func (c *RedisListCache) Load(ctx context.Context, filter ListFilter) (ListCacheEntry, error) {
	if filter.Limit == 0 {
		filter.Limit = DefaultLimit
	}

	namespace, err := c.ensureReady(ctx)
	if err != nil {
		return ListCacheEntry{}, err
	}

	value, err := c.client.Eval(ctx, loadGenerationScript,
		[]string{c.namespaceKey(), c.generationKey(namespace, filter.TeamID)},
		namespace, rand.Text(), (2 * TaskListCacheTTL).Milliseconds()).Result()
	if err != nil {
		c.disable()
		return ListCacheEntry{}, fmt.Errorf("read task cache generation: %w", err)
	}

	generation, ok := value.(string)
	if !ok || generation == "" {
		c.disable()
		return ListCacheEntry{}, errCacheNotReady
	}

	entry := ListCacheEntry{
		namespace: namespace, generation: generation, teamID: filter.TeamID,
		key: c.listKey(namespace, generation, filter),
	}
	// Validate the generation and read its list atomically with respect to invalidation.
	data, err := c.client.Eval(ctx, loadListScript,
		[]string{c.namespaceKey(), c.generationKey(namespace, filter.TeamID), entry.key},
		namespace, generation).Text()
	if errors.Is(err, redis.Nil) {
		if !c.isCurrent(namespace) {
			return ListCacheEntry{}, errCacheNotReady
		}

		return entry, nil
	}

	if err != nil {
		c.disable()
		return ListCacheEntry{}, fmt.Errorf("read cached task list: %w", err)
	}

	if err := json.Unmarshal([]byte(data), &entry.Tasks); err != nil {
		c.disable()
		return ListCacheEntry{}, fmt.Errorf("decode cached task list: %w", err)
	}

	if !validCachedList(entry.Tasks, filter) {
		c.disable()
		return ListCacheEntry{}, errors.New("cached task list does not match its filter")
	}

	if !c.isCurrent(namespace) {
		return ListCacheEntry{}, errCacheNotReady
	}

	entry.Found = true
	return entry, nil
}

func (c *RedisListCache) Store(ctx context.Context, entry ListCacheEntry) error {
	if entry.key == "" || !c.isCurrent(entry.namespace) {
		return nil
	}

	if entry.Tasks == nil {
		entry.Tasks = []Task{}
	}

	data, err := json.Marshal(entry.Tasks)
	if err != nil {
		return fmt.Errorf("encode cached task list: %w", err)
	}

	// A late reader cannot fill an obsolete namespace or team generation.
	if err := c.client.Eval(ctx, storeListScript,
		[]string{c.namespaceKey(), c.generationKey(entry.namespace, entry.teamID), entry.key},
		entry.namespace, entry.generation, string(data), TaskListCacheTTL.Milliseconds()).Err(); err != nil {
		c.disable()
		return fmt.Errorf("store cached task list: %w", err)
	}

	return nil
}

// Invalidate advances the team's generation, or resets the namespace during recovery.
// A failure disables this instance's cache until recovery acknowledges a fresh namespace.
func (c *RedisListCache) Invalidate(ctx context.Context, teamID uint64) error {
	if err := c.lockMutation(ctx); err != nil {
		c.disable()
		return err
	}

	defer c.unlockMutation()
	namespace, ready, _ := c.state()
	if !ready {
		// Switching the whole namespace also covers writes during an outage.
		_, err := c.resetLocked(ctx)
		return err
	}

	changed, err := c.client.Eval(ctx, invalidateListScript,
		[]string{c.namespaceKey(), c.generationKey(namespace, teamID)},
		namespace, rand.Text(), (2 * TaskListCacheTTL).Milliseconds()).Int64()
	if err != nil {
		c.disable()
		return fmt.Errorf("invalidate task list cache: %w", err)
	}

	if changed != 1 {
		c.disable()
		return errCacheNotReady
	}

	if !c.isCurrent(namespace) {
		return errCacheNotReady
	}

	return nil
}

func (c *RedisListCache) ensureReady(ctx context.Context) (string, error) {
	namespace, ready, _ := c.state()
	if ready {
		return namespace, nil
	}

	if err := c.lockMutation(ctx); err != nil {
		return "", err
	}

	defer c.unlockMutation()
	namespace, ready, _ = c.state()
	if ready {
		return namespace, nil
	}

	return c.resetLocked(ctx)
}

func (c *RedisListCache) resetLocked(ctx context.Context) (string, error) {
	_, _, revision := c.state()
	namespace := rand.Text()
	// This single marker is overwritten on every restart or recovery; only
	// generation keys and cached lists need expiry to reclaim obsolete data.
	if err := c.client.Set(ctx, c.namespaceKey(), namespace, 0).Err(); err != nil {
		c.disable()
		return "", fmt.Errorf("initialize task cache namespace: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// A newer invalidation failure must not be cleared by an older reset.
	if c.revision != revision {
		return "", errCacheNotReady
	}

	c.namespace = namespace
	c.ready = true
	return namespace, nil
}

func (c *RedisListCache) state() (string, bool, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.namespace, c.ready, c.revision
}

func (c *RedisListCache) isCurrent(namespace string) bool {
	current, ready, _ := c.state()
	return ready && current == namespace
}

func (c *RedisListCache) disable() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready = false
	c.revision++
}

func (c *RedisListCache) lockMutation(ctx context.Context) error {
	select {
	case c.mutation <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *RedisListCache) unlockMutation() {
	<-c.mutation
}

func (c *RedisListCache) namespaceKey() string {
	return c.prefix + ":namespace"
}

func (c *RedisListCache) generationKey(namespace string, teamID uint64) string {
	return fmt.Sprintf("%s:generation:%s:%d", c.prefix, namespace, teamID)
}

func (c *RedisListCache) listKey(namespace, generation string, filter ListFilter) string {
	status, assignee := string(filter.Status), "-"
	if status == "" {
		status = "-"
	}

	if filter.AssigneeID != nil {
		assignee = strconv.FormatUint(*filter.AssigneeID, 10)
	}

	return fmt.Sprintf("%s:data:%s:%d:%s:%s:%s:%d:%d", c.prefix, namespace, filter.TeamID,
		generation, status, assignee, filter.Limit, filter.Offset)
}

func validCachedList(tasks []Task, filter ListFilter) bool {
	if tasks == nil || len(tasks) > filter.Limit {
		return false
	}

	for _, task := range tasks {
		if task.ID == 0 || task.TeamID != filter.TeamID || task.Version == 0 || !validStatus(task.Status) {
			return false
		}

		if filter.Status != "" && task.Status != filter.Status {
			return false
		}

		if filter.AssigneeID != nil && !sameAssignee(task.AssigneeID, filter.AssigneeID) {
			return false
		}
	}

	return true
}
