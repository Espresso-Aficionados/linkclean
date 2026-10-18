package unshorten

import (
	"context"
	"time"
)

// Cache stores hop outcomes by hop key. Keys are the requested URLs in
// request form (no fragment); values are the shortener's raw destination,
// which can carry tracking and affiliate parameters the caller's policy later
// removes.
type Cache interface {
	Get(ctx context.Context, key string) (CacheEntry, bool)
	Set(ctx context.Context, key string, e CacheEntry, ttl time.Duration) // may clamp ttl or decline to store
}

// NewMemoryCache returns a bounded LRU with per-entry expiry; concurrency-safe;
// definitive failures, and hops the Resolver identifies as a shortener
// fallback or a stop, live in a separate bound of maxEntries/4, so failing
// keys never evict successes; an entry whose key plus Next exceeds 2 KiB is
// not stored, so the default holds at most (10,000 + 2,500) × 2 KiB ≈ 25 MiB
// of URL bytes. maxEntries ≤ 0 means 10,000.
func NewMemoryCache(maxEntries int) Cache {
	panic("unshorten: not implemented")
}

// NoCache returns a Cache that stores nothing.
func NoCache() Cache {
	panic("unshorten: not implemented")
}

// negativeSetter is implemented by NewMemoryCache's cache only: it stores a
// FailNone record in the negative bound, with the TTL given. The resolver
// uses it for stops and for hops IsFallback flags; any other Cache gets Set.
type negativeSetter interface {
	setNegative(ctx context.Context, key string, e CacheEntry, ttl time.Duration)
}
