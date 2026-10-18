package unshorten

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

type nopRoundTripper struct{}

func (nopRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("nopRoundTripper")
}

// lockedBuffer is a bytes.Buffer safe for a slog handler written from the
// flight group's goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// TestNewState checks the shared state newState builds: the client, one
// slot channel per allowlisted host, the global slots, and the flight
// group's bounds, abnormal value and panic log.
func TestNewState(t *testing.T) {
	rc := resolvedConfig{maxReq: 7, maxReqHost: 3, maxFlight: 3, maxFlightHost: 2, maxHops: 4}
	reg := &Registry{entries: []*compiledEntry{
		{Entry: Entry{Name: "a"}, hosts: []string{"a.example", "shared.example"}},
		{Entry: Entry{Name: "b"}, hosts: []string{"b.example", "shared.example"}},
	}}
	var logs lockedBuffer
	rt := nopRoundTripper{}
	st := newState(rc, reg, NoCache(), NoLimit(), rt, slog.New(slog.NewTextHandler(&logs, nil)))

	if st.client.Transport != rt || st.client.Jar != nil || st.client.Timeout != 0 {
		t.Errorf("client: Transport %v, Jar %v, Timeout %v; want rt, nil, 0", st.client.Transport, st.client.Jar, st.client.Timeout)
	}
	if err := st.client.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("CheckRedirect = %v, want http.ErrUseLastResponse", err)
	}
	var hosts []string
	for h, ch := range st.hostSlots {
		hosts = append(hosts, h)
		if cap(ch) != rc.maxReqHost {
			t.Errorf("hostSlots[%q] has cap %d, want %d", h, cap(ch), rc.maxReqHost)
		}
	}
	slices.Sort(hosts)
	if want := []string{"a.example", "b.example", "shared.example"}; !slices.Equal(hosts, want) {
		t.Errorf("hostSlots hosts %q, want %q", hosts, want)
	}
	if cap(st.global) != rc.maxReq {
		t.Errorf("global slots cap %d, want %d", cap(st.global), rc.maxReq)
	}

	// The group's bounds: per host, then overall.
	release := make(chan struct{})
	hold := func() (hopOutcome, func()) { <-release; return hopOutcome{}, nil }
	for _, k := range []struct{ key, host string }{{"1", "h"}, {"2", "h"}, {"3", "g"}} {
		if _, ok := st.group.DoChan(k.key, k.host, hold); !ok {
			t.Fatalf("key %s for %s refused", k.key, k.host)
		}
	}
	if _, ok := st.group.DoChan("4", "g", hold); ok {
		t.Error("a key past maxFlight started")
	}
	if total, forHost := st.group.InFlight("h"); total != 3 || forHost != 2 {
		t.Errorf("InFlight(h) = %d, %d; want 3, 2", total, forHost)
	}
	close(release)
	st.group.Wait()

	// A panicking call delivers the transient outcome and logs the entry,
	// host and panic type, never the value or the rest of the key.
	const token = "Zq7TOKENx9"
	key := "t.co\x00Redirect\x00https://t.co/" + token
	ch, ok := st.group.DoChan(key, "t.co", func() (hopOutcome, func()) {
		panic(errors.New("bad https://t.co/" + token))
	})
	if !ok {
		t.Fatal("panicking key refused")
	}
	st.group.Wait()
	got := <-ch
	if got.e != (CacheEntry{Failure: FailTransient}) || got.class != classOther || got.fromCache || got.infos != nil {
		t.Errorf("panic delivered %+v, want the transient outcome with classOther", got)
	}
	log := logs.String()
	for _, want := range []string{"level=ERROR", "entry=t.co", "host=t.co", "panic_type=*errors.errorString", "stack="} {
		if !strings.Contains(log, want) {
			t.Errorf("panic log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, token) {
		t.Errorf("panic log holds the URL token:\n%s", log)
	}
}
