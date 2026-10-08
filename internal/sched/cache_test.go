package sched

import (
	"fmt"
	"testing"
	"time"
)

func cached(feedURL string, at time.Time) *fetched {
	return &fetched{feedURL: feedURL, fetchedAt: at}
}

func TestPreviewCache_KeyedByRequestedAndFinalURL(t *testing.T) {
	c := newPreviewCache(previewCacheTTL, previewCacheMax)
	v := cached("https://new.example/feed", t0)
	c.put(v, "http://old.example/feed", v.feedURL)

	for _, key := range []string{"http://old.example/feed", "https://new.example/feed"} {
		if got := c.get(key, t0.Add(time.Minute)); got != v {
			t.Errorf("get(%s) = %v, want the cached fetch", key, got)
		}
	}
	c.drop("https://new.example/feed")
	if got := c.get("http://old.example/feed", t0); got != nil {
		t.Errorf("drop by one key left the other key cached")
	}
}

func TestPreviewCache_ExpiresAfterTenMinutes(t *testing.T) {
	c := newPreviewCache(previewCacheTTL, previewCacheMax)
	c.put(cached("u", t0), "u")
	if c.get("u", t0.Add(previewCacheTTL-time.Second)) == nil {
		t.Error("expired before the TTL")
	}
	if c.get("u", t0.Add(previewCacheTTL)) != nil {
		t.Error("still cached after the TTL")
	}
	if c.get("u", t0.Add(-time.Minute)) != nil {
		t.Error("a fetch from the future (clock went back) was trusted")
	}
}

func TestPreviewCache_EvictsOldestBeyondSixteenEntries(t *testing.T) {
	c := newPreviewCache(previewCacheTTL, previewCacheMax)
	n := previewCacheMax + 3
	for i := range n {
		u := fmt.Sprintf("https://f%d.example/feed", i)
		// Two keys per fetch must still count as one entry.
		c.put(cached(u, t0.Add(time.Duration(i)*time.Second)), u, u+"?alias")
	}
	if len(c.items) != previewCacheMax {
		t.Fatalf("cache holds %d entries, want %d", len(c.items), previewCacheMax)
	}
	now := t0.Add(time.Minute)
	for i := range n {
		u := fmt.Sprintf("https://f%d.example/feed", i)
		got := c.get(u, now)
		if evicted := i < n-previewCacheMax; evicted != (got == nil) {
			t.Errorf("%s: cached = %v, want evicted = %v", u, got != nil, evicted)
		}
	}
}

func TestPreviewCache_PutReplacesAnEntrySharingAKey(t *testing.T) {
	c := newPreviewCache(previewCacheTTL, previewCacheMax)
	first := cached("https://a.example/feed", t0)
	c.put(first, "https://a.example/feed")
	second := cached("https://a.example/feed", t0.Add(time.Minute))
	c.put(second, "https://a.example/feed", "https://a.example/feed")
	if got := c.get("https://a.example/feed", t0.Add(2*time.Minute)); got != second {
		t.Errorf("get = %v, want the newer fetch", got)
	}
	if len(c.items) != 1 {
		t.Errorf("cache holds %d entries, want 1", len(c.items))
	}
}

func TestPreviewCache_PutKeepsANewerFetchStoredBeforeIt(t *testing.T) {
	// Two previews finish out of order: B was fetched after A but is cached
	// first, because A took longer to parse.
	c := newPreviewCache(previewCacheTTL, previewCacheMax)
	b := cached("https://b.example/feed", t0.Add(2*time.Second))
	c.put(b, b.feedURL)
	a := cached("https://a.example/feed", t0.Add(time.Second))
	c.put(a, a.feedURL)
	now := t0.Add(3 * time.Second)
	if got := c.get(b.feedURL, now); got != b {
		t.Errorf("get(B) = %v after caching an older fetch, want B", got)
	}
	if got := c.get(a.feedURL, now); got != a {
		t.Errorf("get(A) = %v, want A", got)
	}

	// Items that are really expired are still evicted on put.
	c.put(cached("https://c.example/feed", t0.Add(previewCacheTTL+2*time.Second)), "https://c.example/feed")
	if len(c.items) != 1 {
		t.Errorf("cache holds %d items, want only the latest after the others expired", len(c.items))
	}
}
