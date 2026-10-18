package unshorten

// Benchmarks. The ones that send requests do so over the test network
// (nettest_test.go) only, with NoLimit and large bounds so they never wait
// on the default 2 requests per second.

import (
	"context"
	"strconv"
	"sync"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/unshorten/internal/flight"
)

// benchResolver builds a Resolver over n with a single s.example.net
// Redirect entry answered by n.
func benchResolver(b *testing.B, n *testNet, cfg Config) *Resolver {
	b.Helper()
	reg, err := NewRegistry(rvEntry("s.example.net"))
	if err != nil {
		b.Fatal(err)
	}
	cfg.Registry = reg
	cfg.Transport = NewTransport(TransportOptions{TLSConfig: n.ClientTLS()})
	cfg.Limiter = NoLimit()
	return newTestResolver(b, cfg)
}

// BenchmarkResolveNoMatch is the cost every input pays: the offline clean
// and the registry lookup, with no request.
func BenchmarkResolveNoMatch(b *testing.B) {
	r := newTestResolver(b, Config{})
	ctx := context.Background()
	for b.Loop() {
		if _, err := r.Resolve(ctx, "https://example.org/x?utm_source=z&id=1"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkResolveCached is a short link whose hop is in the Cache.
func BenchmarkResolveCached(b *testing.B) {
	n := installNet(b)
	n.HandleTLS("s.example.net", rvRedirect(301, "https://example.net/p?utm_source=x&id=1"))
	r := benchResolver(b, n, Config{})
	ctx := context.Background()
	if _, err := r.Resolve(ctx, "https://s.example.net/x"); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		res, err := r.Resolve(ctx, "https://s.example.net/x")
		if err != nil || !res.Cached {
			b.Fatalf("%+v, %v", res, err)
		}
	}
}

// BenchmarkResolveMiss is one uncached hop per call: a request over
// loopback, with the dial guard, TLS session reuse and keep-alive.
func BenchmarkResolveMiss(b *testing.B) {
	n := installNet(b)
	n.HandleTLS("s.example.net", rvRedirect(301, "https://example.net/p?utm_source=x&id=1"))
	r := benchResolver(b, n, Config{Cache: NoCache(), MaxInFlight: 1024, MaxInFlightPerHost: 1024, MaxConcurrentRequests: 64, MaxConcurrentRequestsPerHost: 64})
	ctx := context.Background()
	for b.Loop() {
		if _, err := r.Resolve(ctx, "https://s.example.net/x"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFlight is the flood model: 300 unique keys for one stalled
// host against the default bounds (256 keys, 16 per host), so 16 start
// and 284 are refused at once, then the stall ends and the group drains.
func BenchmarkFlight(b *testing.B) {
	keys := make([]string, 300)
	for i := range keys {
		keys[i] = "t.co\x00Redirect\x00https://t.co/f" + strconv.Itoa(i)
	}
	for b.Loop() {
		g := flight.New(defaultMaxFlight, defaultMaxFlightHost, hopOutcome{}, nil)
		release := make(chan struct{})
		var wg sync.WaitGroup
		started := 0
		for _, k := range keys {
			wg.Add(1)
			_, ok := g.DoChan(k, "t.co", func() (hopOutcome, func()) {
				defer wg.Done()
				<-release
				return hopOutcome{}, nil
			})
			if ok {
				started++
			} else {
				wg.Done()
			}
		}
		if started != defaultMaxFlightHost {
			b.Fatalf("%d keys started, want %d", started, defaultMaxFlightHost)
		}
		close(release)
		wg.Wait()
		g.Wait()
	}
}
