package unshorten

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func newMem(t *testing.T, maxEntries int) *memoryCache {
	t.Helper()
	c, ok := NewMemoryCache(maxEntries).(*memoryCache)
	if !ok {
		t.Fatalf("NewMemoryCache returned %T, want *memoryCache", c)
	}
	return c
}

// lens returns the sizes of both LRUs and checks that each one's map and
// list agree and that no key is in both.
func (c *memoryCache) lens(t *testing.T) (pos, neg int) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range []*lru{&c.pos, &c.neg} {
		if l.order.Len() != len(l.items) {
			t.Fatalf("lru list has %d items, map %d", l.order.Len(), len(l.items))
		}
	}
	for k := range c.pos.items {
		if _, ok := c.neg.items[k]; ok {
			t.Fatalf("key %q is in both LRUs", k)
		}
	}
	return c.pos.order.Len(), c.neg.order.Len()
}

func (c *memoryCache) inNeg(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.neg.items[key]
	return ok
}

var (
	okEntry   = CacheEntry{Next: "https://example.net/p", Strategy: "Redirect", Status: 301, Requests: 1}
	failEntry = CacheEntry{Strategy: "Redirect", Status: 404, Requests: 1, Failure: FailNoRedirect}
)

func TestMemoryCacheGetSet(t *testing.T) {
	ctx := context.Background()
	c := NewMemoryCache(10)
	if _, ok := c.Get(ctx, "k"); ok {
		t.Fatal("hit on an empty cache")
	}
	c.Set(ctx, "k", okEntry, time.Hour)
	c.Set(ctx, "f", failEntry, time.Hour)
	for key, want := range map[string]CacheEntry{"k": okEntry, "f": failEntry} {
		if got, ok := c.Get(ctx, key); !ok || got != want {
			t.Errorf("Get(%q) = %+v, %v; want %+v, true", key, got, ok, want)
		}
	}
	replaced := okEntry
	replaced.Next = "https://example.net/q"
	c.Set(ctx, "k", replaced, time.Hour)
	if got, _ := c.Get(ctx, "k"); got != replaced {
		t.Errorf("after replace Get = %+v, want %+v", got, replaced)
	}
}

func TestMemoryCacheSizes(t *testing.T) {
	for _, tc := range []struct{ in, pos, neg int }{
		{0, 10_000, 2_500}, {-5, 10_000, 2_500}, {100, 100, 25}, {8, 8, 2}, {3, 3, 1}, {1, 1, 1},
	} {
		c := newMem(t, tc.in)
		if c.pos.max != tc.pos || c.neg.max != tc.neg {
			t.Errorf("NewMemoryCache(%d): bounds %d/%d, want %d/%d", tc.in, c.pos.max, c.neg.max, tc.pos, tc.neg)
		}
	}
}

func TestMemoryCacheExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := newMem(t, 10)
		c.Set(ctx, "pos", okEntry, 24*time.Hour)
		c.Set(ctx, "neg", failEntry, 10*time.Minute)
		c.setNegative(ctx, "stop", CacheEntry{Strategy: "Redirect"}, 24*time.Hour)

		time.Sleep(10*time.Minute - time.Nanosecond)
		for _, k := range []string{"pos", "neg", "stop"} {
			if _, ok := c.Get(ctx, k); !ok {
				t.Errorf("%s expired early", k)
			}
		}
		time.Sleep(time.Nanosecond)
		if _, ok := c.Get(ctx, "neg"); ok {
			t.Error("neg served at its expiry")
		}
		if _, neg := c.lens(t); neg != 1 {
			t.Errorf("expired entry not removed on read: neg holds %d, want 1", neg)
		}
		time.Sleep(24*time.Hour - 10*time.Minute)
		for _, k := range []string{"pos", "stop"} {
			if _, ok := c.Get(ctx, k); ok {
				t.Errorf("%s served at its expiry", k)
			}
		}
		if pos, neg := c.lens(t); pos != 0 || neg != 0 {
			t.Errorf("after expiry: %d/%d entries, want 0/0", pos, neg)
		}

		// Replacing an entry resets its expiry.
		c.Set(ctx, "k", okEntry, time.Minute)
		time.Sleep(30 * time.Second)
		c.Set(ctx, "k", okEntry, time.Minute)
		time.Sleep(45 * time.Second)
		if _, ok := c.Get(ctx, "k"); !ok {
			t.Error("replaced entry kept its old expiry")
		}
	})
}

func TestMemoryCacheLRUOrder(t *testing.T) {
	ctx := context.Background()
	c := newMem(t, 3)
	for _, k := range []string{"a", "b", "c"} {
		c.Set(ctx, k, okEntry, time.Hour)
	}
	c.Get(ctx, "a")                     // a is now the most recent
	c.Set(ctx, "b", okEntry, time.Hour) // a replace counts as a use: b is most recent
	c.Set(ctx, "d", okEntry, time.Hour) // evicts c, the least recent
	for k, want := range map[string]bool{"a": true, "b": true, "c": false, "d": true} {
		if _, ok := c.Get(ctx, k); ok != want {
			t.Errorf("Get(%q) hit %v, want %v", k, ok, want)
		}
	}
	if pos, _ := c.lens(t); pos != 3 {
		t.Errorf("positive bound holds %d, want 3", pos)
	}
}

// TestMemoryCacheNegativeBound: definitive failures live in their own
// bound, so a flood of unique failing keys never evicts a success.
func TestMemoryCacheNegativeBound(t *testing.T) {
	ctx := context.Background()
	c := newMem(t, 100)
	c.Set(ctx, "https://t.co/ok", okEntry, time.Hour)
	for i := range 10_000 {
		e := failEntry
		e.Failure = []FailureKind{FailNoRedirect, FailNoMatch, FailBadTarget, FailStatus, FailFetchDenied, FailTransport, FailBudget}[i%7]
		c.Set(ctx, fmt.Sprintf("https://t.co/bad%d", i), e, time.Hour)
	}
	if got, ok := c.Get(ctx, "https://t.co/ok"); !ok || got != okEntry {
		t.Errorf("success evicted by failures: %+v, %v", got, ok)
	}
	if pos, neg := c.lens(t); pos != 1 || neg != 25 {
		t.Errorf("bounds hold %d/%d, want 1/25", pos, neg)
	}
	if _, ok := c.Get(ctx, "https://t.co/bad9999"); !ok {
		t.Error("the most recent failure was not kept")
	}
}

// TestMemoryCacheSetNegative: setNegative places a FailNone record in the
// negative bound, so a flood of fallback or stop records cannot evict a
// success either.
func TestMemoryCacheSetNegative(t *testing.T) {
	ctx := context.Background()
	c := newMem(t, 100)
	var ns negativeSetter = c
	c.Set(ctx, "https://t.co/ok", okEntry, time.Hour)
	fallback := CacheEntry{Next: "https://www.amazon.com/", Strategy: "Redirect", Status: 301, Requests: 1}
	for i := range 1_000 {
		ns.setNegative(ctx, fmt.Sprintf("https://amzn.to/x%d", i), fallback, time.Hour)
	}
	if got, ok := c.Get(ctx, "https://t.co/ok"); !ok || got != okEntry {
		t.Errorf("success evicted by fallback records: %+v, %v", got, ok)
	}
	if got, ok := c.Get(ctx, "https://amzn.to/x999"); !ok || got != fallback {
		t.Errorf("fallback record: %+v, %v; want %+v", got, ok, fallback)
	}
	if !c.inNeg("https://amzn.to/x999") {
		t.Error("setNegative stored outside the negative bound")
	}
	if pos, neg := c.lens(t); pos != 1 || neg != 25 {
		t.Errorf("bounds hold %d/%d, want 1/25", pos, neg)
	}
	if _, ok := NoCache().(negativeSetter); ok {
		t.Error("NoCache implements negativeSetter")
	}
}

// TestMemoryCacheMove: a key moving between the bounds leaves the other
// one.
func TestMemoryCacheMove(t *testing.T) {
	ctx := context.Background()
	c := newMem(t, 8)
	c.Set(ctx, "k", okEntry, time.Hour)
	c.Set(ctx, "k", failEntry, time.Hour)
	if pos, neg := c.lens(t); pos != 0 || neg != 1 {
		t.Errorf("after failure: %d/%d, want 0/1", pos, neg)
	}
	c.Set(ctx, "k", okEntry, time.Hour)
	if pos, neg := c.lens(t); pos != 1 || neg != 0 {
		t.Errorf("after success: %d/%d, want 1/0", pos, neg)
	}
	c.setNegative(ctx, "k", okEntry, time.Hour)
	if pos, neg := c.lens(t); pos != 0 || neg != 1 {
		t.Errorf("after setNegative: %d/%d, want 0/1", pos, neg)
	}
	if got, ok := c.Get(ctx, "k"); !ok || got != okEntry {
		t.Errorf("Get = %+v, %v", got, ok)
	}
}

func TestMemoryCacheDeclines(t *testing.T) {
	ctx := context.Background()
	long := func(n int) string { return "https://t.co/" + strings.Repeat("a", n-len("https://t.co/")) }
	for _, tc := range []struct {
		name string
		key  string
		e    CacheEntry
		ttl  time.Duration
	}{
		{"zero ttl", "k", okEntry, 0},
		{"negative ttl", "k", okEntry, -time.Second},
		{"transient", "k", CacheEntry{Failure: FailTransient}, time.Hour},
		{"hop limit", "k", CacheEntry{Failure: FailHopLimit}, time.Hour},
		{"loop", "k", CacheEntry{Failure: FailLoop}, time.Hour},
		{"fallback", "k", CacheEntry{Failure: FailFallback}, time.Hour},
		{"unknown kind", "k", CacheEntry{Failure: FailTransient + 1}, time.Hour},
		{"key over 2 KiB", long(2049), CacheEntry{}, time.Hour},
		{"key plus Next over 2 KiB", long(1024), CacheEntry{Next: long(1025)}, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newMem(t, 8)
			c.Set(ctx, tc.key, tc.e, tc.ttl)
			c.setNegative(ctx, tc.key+"/neg", tc.e, tc.ttl)
			if pos, neg := c.lens(t); pos != 0 || neg != 0 {
				t.Errorf("stored %d/%d entries, want none", pos, neg)
			}
		})
	}
	t.Run("exactly 2 KiB", func(t *testing.T) {
		c := newMem(t, 8)
		e := CacheEntry{Next: long(1024)}
		c.Set(ctx, long(1024), e, time.Hour)
		if got, ok := c.Get(ctx, long(1024)); !ok || got != e {
			t.Error("an entry of exactly 2 KiB was not stored")
		}
	})
}

func TestNoCache(t *testing.T) {
	ctx := context.Background()
	c := NoCache()
	c.Set(ctx, "k", okEntry, time.Hour)
	if _, ok := c.Get(ctx, "k"); ok {
		t.Error("NoCache returned a hit")
	}
}

// TestMemoryCacheConcurrent hammers one cache from many goroutines; run it
// under -race. The bounds and the map/list agreement must hold throughout.
func TestMemoryCacheConcurrent(t *testing.T) {
	ctx := context.Background()
	c := newMem(t, 64)
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Go(func() {
			for i := range 2_000 {
				k := fmt.Sprintf("k%d", (g*7919+i)%300)
				switch i % 4 {
				case 0:
					c.Set(ctx, k, okEntry, time.Hour)
				case 1:
					c.Set(ctx, k, failEntry, time.Hour)
				case 2:
					c.setNegative(ctx, k, okEntry, time.Hour)
				default:
					if e, ok := c.Get(ctx, k); ok && e != okEntry && e != failEntry {
						t.Errorf("Get(%q) = %+v: torn entry", k, e)
					}
				}
			}
		})
	}
	wg.Wait()
	if pos, neg := c.lens(t); pos > 64 || neg > 16 {
		t.Errorf("bounds exceeded: %d/%d, max 64/16", pos, neg)
	}
}
