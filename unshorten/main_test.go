package unshorten

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/unshorten/internal/dialhook"
)

// TestMain keeps the test binary off the real network. http.DefaultTransport
// is replaced by one that fails every request, and a fail-closed Resolve
// seam is installed for the whole binary: the guarded dialer asks it before
// any DNS lookup, it maps nothing, and it records the address. installNet
// overrides it with the test network's map and restores it on cleanup, so a
// test that forgets installNet fails before any lookup or dial. The binary
// fails if anything was recorded.
func TestMain(m *testing.M) {
	origDefaultTransport = http.DefaultTransport
	http.DefaultTransport = failingTransport{}
	restore := dialhook.SetResolve(failClosedResolve)
	// Tests that set AllowUnguardedTransport without a Logger would print New's
	// warning on stderr; tests that check logging pass their own Logger.
	slog.SetDefault(slog.New(slog.DiscardHandler))
	code := m.Run()
	restore()
	if leaks := offNetwork.list(); len(leaks) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: unshorten tests tried to reach the real network: %q\n", leaks)
		code = 1
	}
	os.Exit(code)
}

// origDefaultTransport is net/http's DefaultTransport, kept so tests can pass it
// to New (which must refuse it: its Proxy is ProxyFromEnvironment). Never
// send a request through it.
var origDefaultTransport http.RoundTripper

// offNetwork records every attempt to leave the test network.
var offNetwork recorder

type recorder struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = map[string]bool{}
	}
	r.seen[s] = true
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for s := range r.seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// failClosedResolve is the binary-wide Resolve seam: it maps nothing.
func failClosedResolve(host, port string) (string, bool) {
	offNetwork.add("dial " + net.JoinHostPort(host, port))
	return "", false
}

// errDefaultTransport is the error of every request through
// http.DefaultTransport.
var errDefaultTransport = errors.New("unshorten tests: http.DefaultTransport is disabled")

type failingTransport struct{}

func (failingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	offNetwork.add("http.DefaultTransport " + req.URL.Host)
	if req.Body != nil {
		req.Body.Close()
	}
	return nil, errDefaultTransport
}
