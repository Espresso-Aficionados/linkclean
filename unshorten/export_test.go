package unshorten

import "testing"

// newTestResolver calls New and registers a cleanup that drains the
// Resolver's detached hops (and their OnRequest calls), then closes its idle
// connections. Call installNet first: cleanups run LIFO, so the drain then
// finishes before the test network is torn down. Every test that sends a
// request builds its Resolver this way.
func newTestResolver(tb testing.TB, cfg Config) *Resolver {
	tb.Helper()
	r, err := New(cfg)
	if err != nil {
		tb.Fatalf("New: %v", err)
	}
	tb.Cleanup(func() {
		r.st.group.Wait()
		r.st.client.CloseIdleConnections()
	})
	return r
}

// attestationCount returns the number of transports in the attestation
// registry.
func attestationCount() int {
	n := 0
	attestations.Range(func(any, any) bool { n++; return true })
	return n
}
