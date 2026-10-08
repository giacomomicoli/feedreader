package sched

import (
	"slices"
	"sync"
	"time"
)

// Preview cache tuning. These are implementation details, not documented
// defaults, so they live here rather than in internal/config.
const (
	// previewCacheTTL is how long a Preview fetch stays usable by Subscribe.
	previewCacheTTL = 10 * time.Minute
	// previewCacheMax bounds the number of cached fetches (each holds a
	// parsed feed); the oldest is evicted first.
	previewCacheMax = 16
)

// previewCache keeps recent add-flow fetches so that confirming a preview
// does not fetch the feed a second time. A fetch is stored under several
// keys (the URL typed and the final feed URL) but counts once.
type previewCache struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	items []cacheItem // oldest first
}

type cacheItem struct {
	keys []string
	val  *fetched
}

func newPreviewCache(ttl time.Duration, maxItems int) *previewCache {
	return &previewCache{ttl: ttl, max: maxItems}
}

// put stores v under keys, replacing whatever any of those keys referred to,
// and evicts the items fetched at least ttl before v and then the oldest ones
// beyond capacity. An item fetched after v is kept: concurrent fetches can
// finish parsing out of order.
func (c *previewCache) put(v *fetched, keys ...string) {
	keys = slices.Compact(slices.Sorted(slices.Values(keys)))
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeLocked(keys)
	c.items = slices.DeleteFunc(c.items, func(it cacheItem) bool {
		return v.fetchedAt.Sub(it.val.fetchedAt) >= c.ttl
	})
	c.items = append(c.items, cacheItem{keys: keys, val: v})
	if n := len(c.items) - c.max; n > 0 {
		c.items = slices.Delete(c.items, 0, n)
	}
}

// get returns the fetch cached under key if it is still fresh at now, else
// nil.
func (c *previewCache) get(key string, now time.Time) *fetched {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.items) - 1; i >= 0; i-- {
		it := c.items[i]
		if slices.Contains(it.keys, key) {
			if c.fresh(it.val, now) {
				return it.val
			}
			return nil
		}
	}
	return nil
}

// drop removes every item stored under any of keys.
func (c *previewCache) drop(keys ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeLocked(keys)
}

func (c *previewCache) removeLocked(keys []string) {
	c.items = slices.DeleteFunc(c.items, func(it cacheItem) bool {
		return slices.ContainsFunc(keys, func(k string) bool { return slices.Contains(it.keys, k) })
	})
}

// fresh reports whether v, fetched at v.fetchedAt, may still be used at now.
// A fetch from the future (the clock went back) is not trusted.
func (c *previewCache) fresh(v *fetched, now time.Time) bool {
	age := now.Sub(v.fetchedAt)
	return age >= 0 && age < c.ttl
}
