package unshorten

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Cache stores hop outcomes by hop key. Keys are the requested URLs in
// request form (no fragment); values are the shortener's raw destination,
// which can carry tracking and affiliate parameters the caller's policy later
// removes.
//
// The Resolver bounds the ctx it passes by Config.Timeout: a Get on a
// caller's path has the resolve deadline, and the Get and Set of a hop
// being fetched have the hop's own. An implementation that waits on
// anything (a remote store) must honour ctx, or it holds the caller past
// Timeout.
type Cache interface {
	Get(ctx context.Context, key string) (CacheEntry, bool)
	Set(ctx context.Context, key string, e CacheEntry, ttl time.Duration) // may clamp ttl or decline to store
}

// defaultCacheEntries is NewMemoryCache's size for maxEntries ≤ 0.
const defaultCacheEntries = 10_000

// maxCacheEntryBytes bounds len(key) + len(Next) of a stored entry, so the
// default cache holds at most (10,000 + 2,500) × 2 KiB, about 25 MiB, of
// URL bytes.
const maxCacheEntryBytes = 2 << 10

// NewMemoryCache returns a bounded LRU with per-entry expiry; concurrency-safe;
// definitive failures, and hops the Resolver identifies as a shortener
// fallback or a stop, live in a separate bound of maxEntries/4, so failing
// keys never evict successes; an entry whose key plus Next exceeds 2 KiB is
// not stored, so the default holds at most (10,000 + 2,500) × 2 KiB ≈ 25 MiB
// of URL bytes. maxEntries ≤ 0 means 10,000.
func NewMemoryCache(maxEntries int) Cache {
	if maxEntries <= 0 {
		maxEntries = defaultCacheEntries
	}
	return &memoryCache{
		pos: newLRU(maxEntries),
		neg: newLRU(max(1, maxEntries/4)),
	}
}

// memoryCache is NewMemoryCache's Cache: two LRUs under one mutex. pos holds
// FailNone records stored through Set; neg holds definitive failures and the
// records stored through setNegative. A key lives in at most one of them.
type memoryCache struct {
	mu  sync.Mutex
	pos lru
	neg lru
}

var _ negativeSetter = (*memoryCache)(nil)

// Get returns key's entry unless it has expired, and marks it most recently
// used. An expired entry is removed. ctx is unused.
func (c *memoryCache) Get(_ context.Context, key string) (CacheEntry, bool) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range [...]*lru{&c.pos, &c.neg} {
		if e, ok := l.get(key, now); ok {
			return e, true
		}
	}
	return CacheEntry{}, false
}

// Set stores e for ttl: a FailNone record in the positive bound, a
// definitive failure in the negative one. It declines ttl ≤ 0, FailTransient,
// the chain-level kinds (FailHopLimit, FailLoop, FailFallback), any kind
// unknown to this version, and an entry whose key plus Next exceeds 2 KiB.
// ctx is unused.
func (c *memoryCache) Set(_ context.Context, key string, e CacheEntry, ttl time.Duration) {
	c.store(key, e, ttl, e.Failure != FailNone)
}

// setNegative stores e in the negative bound, whatever its kind, with the
// same declines as Set. The resolver uses it for FailNone records that are a
// stop or a shortener fallback, so a flood of them cannot evict successes.
func (c *memoryCache) setNegative(_ context.Context, key string, e CacheEntry, ttl time.Duration) {
	c.store(key, e, ttl, true)
}

func (c *memoryCache) store(key string, e CacheEntry, ttl time.Duration, negative bool) {
	if !cacheable(key, e, ttl) {
		return
	}
	exp := time.Now().Add(ttl)
	c.mu.Lock()
	defer c.mu.Unlock()
	to, other := &c.pos, &c.neg
	if negative {
		to, other = other, to
	}
	other.remove(key)
	to.put(key, e, exp)
}

// cacheable reports whether the memory cache stores e under key for ttl.
func cacheable(key string, e CacheEntry, ttl time.Duration) bool {
	if ttl <= 0 || len(key)+len(e.Next) > maxCacheEntryBytes {
		return false
	}
	switch e.Failure {
	case FailNone, FailNoRedirect, FailNoMatch, FailBadTarget, FailStatus, FailFetchDenied, FailTransport, FailBudget:
		return true
	default: // FailTransient, the chain-level kinds and unknown values
		return false
	}
}

// lru is one bounded list, most recently used at the front. Not safe for
// concurrent use: memoryCache.mu guards it.
type lru struct {
	max   int
	order *list.List // of *lruItem
	items map[string]*list.Element
}

type lruItem struct {
	key string
	e   CacheEntry
	exp time.Time
}

func newLRU(maxEntries int) lru {
	return lru{max: maxEntries, order: list.New(), items: map[string]*list.Element{}}
}

func (l *lru) get(key string, now time.Time) (CacheEntry, bool) {
	el, ok := l.items[key]
	if !ok {
		return CacheEntry{}, false
	}
	it := el.Value.(*lruItem)
	if !now.Before(it.exp) {
		l.order.Remove(el)
		delete(l.items, key)
		return CacheEntry{}, false
	}
	l.order.MoveToFront(el)
	return it.e, true
}

// put stores or replaces key, then evicts from the back past the bound.
func (l *lru) put(key string, e CacheEntry, exp time.Time) {
	if el, ok := l.items[key]; ok {
		it := el.Value.(*lruItem)
		it.e, it.exp = e, exp
		l.order.MoveToFront(el)
		return
	}
	l.items[key] = l.order.PushFront(&lruItem{key: key, e: e, exp: exp})
	for l.order.Len() > l.max {
		back := l.order.Back()
		l.order.Remove(back)
		delete(l.items, back.Value.(*lruItem).key)
	}
}

func (l *lru) remove(key string) {
	if el, ok := l.items[key]; ok {
		l.order.Remove(el)
		delete(l.items, key)
	}
}

// NoCache returns a Cache that stores nothing.
func NoCache() Cache { return noCache{} }

type noCache struct{}

func (noCache) Get(context.Context, string) (CacheEntry, bool)         { return CacheEntry{}, false }
func (noCache) Set(context.Context, string, CacheEntry, time.Duration) {}

// negativeSetter is implemented by NewMemoryCache's cache only: it stores a
// FailNone record in the negative bound, with the TTL given. The resolver
// uses it for stops and for hops IsFallback flags; any other Cache gets Set.
type negativeSetter interface {
	setNegative(ctx context.Context, key string, e CacheEntry, ttl time.Duration)
}
