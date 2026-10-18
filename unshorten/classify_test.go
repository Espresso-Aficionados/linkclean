package unshorten

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"testing"
	"time"

	"code.dny.dev/ssrf"

	"github.com/Espresso-Aficionados/linkclean/unshorten/internal/dialhook"
)

// fakeTimeout is a net.Error that timed out, like net/http's unexported
// header and handshake timeout errors.
type fakeTimeout struct{}

func (fakeTimeout) Error() string   { return "timeout" }
func (fakeTimeout) Timeout() bool   { return true }
func (fakeTimeout) Temporary() bool { return true }

// dialFailure wraps err as client.Do returns a dial failure.
func dialFailure(err error) error {
	return &url.Error{Op: "Get", URL: "https://t.co/TOKEN", Err: &net.OpError{Op: "dial", Net: "tcp", Err: err}}
}

func TestClassifyTransport(t *testing.T) {
	live := context.Background()
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancel2 := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel2()

	for _, c := range []struct {
		name   string
		err    error
		hopCtx context.Context
		kind   FailureKind
		class  errClass
	}{
		{"nil", nil, live, FailNone, classNone},
		{"canceled", &url.Error{Op: "Get", URL: "u", Err: context.Canceled}, live, FailTransient, classCanceled},
		{"canceled beats a guard denial", errors.Join(context.Canceled, dialFailure(ssrf.ErrProhibitedIP)), live, FailTransient, classCanceled},
		{"guard IP", dialFailure(fmt.Errorf("%w: 10.0.0.1 is not a permitted destination", ssrf.ErrProhibitedIP)), live, FailTransport, classGuardDenied},
		{"guard port", dialFailure(fmt.Errorf("%w: 22", ssrf.ErrProhibitedPort)), live, FailTransport, classGuardDenied},
		{"guard network", dialFailure(fmt.Errorf("%w: tcp", ssrf.ErrProhibitedNetwork)), live, FailTransport, classGuardDenied},
		{"guard denial beats a hop deadline", dialFailure(ssrf.ErrProhibitedIP), expired, FailTransport, classGuardDenied},
		{"invalid host:port", dialFailure(fmt.Errorf("%w: x", ssrf.ErrInvalidHostPort)), live, FailTransient, classOther},
		{"unmapped", dialFailure(dialhook.ErrUnmapped), live, FailTransient, classOther},
		{"deadline", &url.Error{Op: "Get", URL: "u", Err: context.DeadlineExceeded}, live, FailTransient, classTimeout},
		{"deadline of the hop", &url.Error{Op: "Get", URL: "u", Err: context.DeadlineExceeded}, expired, FailTransient, classDeadline},
		{"canceled hop ctx", &url.Error{Op: "Get", URL: "u", Err: fakeTimeout{}}, ended, FailTransient, classDeadline},
		{"net timeout", &url.Error{Op: "Get", URL: "u", Err: fakeTimeout{}}, live, FailTransient, classTimeout},
		{"dial i/o timeout", dialFailure(fakeTimeout{}), live, FailTransient, classTimeout},
		{"nil hop ctx", fakeTimeout{}, nil, FailTransient, classTimeout},
		{"DNS not found", dialFailure(&net.DNSError{Err: "no such host", Name: "t.co", IsNotFound: true}), live, FailTransport, classDNSNotFound},
		{"DNS timeout", dialFailure(&net.DNSError{Err: "timeout", Name: "t.co", IsTimeout: true}), live, FailTransient, classTimeout},
		{"DNS temporary", dialFailure(&net.DNSError{Err: "server misbehaving", Name: "t.co", IsTemporary: true}), live, FailTransient, classDNSTemporary},
		{"DNS other", dialFailure(&net.DNSError{Err: "x", Name: "t.co"}), live, FailTransient, classDNSTemporary},
		{"refused", dialFailure(&os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}), live, FailTransport, classRefused},
		{"reset", dialFailure(&os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}), live, FailTransient, classOther},
		{"cert verify", &url.Error{Op: "Get", URL: "u", Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}, live, FailTransport, classTLSVerify},
		{"cert hostname", &url.Error{Op: "Get", URL: "u", Err: &tls.CertificateVerificationError{Err: x509.HostnameError{Host: "t.co"}}}, live, FailTransport, classTLSVerify},
		{"tls alert", &url.Error{Op: "Get", URL: "u", Err: &net.OpError{Op: "remote error", Err: tls.AlertError(40)}}, live, FailTransient, classOther},
		{"CONNECT refused", &url.Error{Op: "Get", URL: "u", Err: &net.OpError{Op: "proxyconnect", Err: errors.New("Forbidden")}}, live, FailTransient, classOther},
		{"other", errors.New("something"), live, FailTransient, classOther},
		{"message text is not read", errors.New("connection refused: no such host: prohibited IP address"), live, FailTransient, classOther},
	} {
		k, cl := classifyTransport(c.err, c.hopCtx)
		if k != c.kind || cl != c.class {
			t.Errorf("%s: %v/%v, want %v/%v", c.name, k, cl, c.kind, c.class)
		}
	}
}

// TestClassifyTransportReal classifies the errors net/http really returns
// through NewTransport and the guard, over loopback.
func TestClassifyTransportReal(t *testing.T) {
	n := installNet(t)
	stall := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-n.Done():
		case <-r.Context().Done():
		}
	})
	n.HandleTLS("h2-stall.test", stall)
	n.HandleTLSH1("h1-stall.test", stall)
	n.StallTLS("tls-stall.test")
	n.HandleTLS("ok.test", handlerOK)
	n.Refuse("refused.test")
	n.WrongCert("wrong-cert.test", handlerOK)
	n.MapRaw("rebound.test", "443", "10.0.0.1:443")
	if err := guardVerdict("10.0.0.1:443"); !isDenial(err) {
		t.Fatal("the guard allows 10.0.0.1:443")
	}

	short := TransportOptions{ResponseHeaderTimeout: 100 * time.Millisecond, TLSHandshakeTimeout: 100 * time.Millisecond}
	for _, c := range []struct {
		name  string
		opts  TransportOptions
		url   string
		kind  FailureKind
		class errClass
	}{
		{"h2 header stall", short, "https://h2-stall.test/x", FailTransient, classTimeout},
		{"h1 header stall", short, "https://h1-stall.test/x", FailTransient, classTimeout},
		{"TLS stall", short, "https://tls-stall.test/x", FailTransient, classTimeout},
		{"dial timeout", TransportOptions{DialTimeout: time.Nanosecond}, "https://ok.test/x", FailTransient, classTimeout},
		{"refused", short, "https://refused.test/x", FailTransport, classRefused},
		{"wrong certificate", short, "https://wrong-cert.test/x", FailTransport, classTLSVerify},
		{"guard denial", short, "https://rebound.test/x", FailTransport, classGuardDenied},
		{"unmapped", short, "https://unmapped.test/x", FailTransient, classOther},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr := guardedTransport(t, n, c.opts)
			_, err := fetchVia(t, tr, c.url)
			if err == nil {
				t.Fatal("no error")
			}
			if k, cl := classifyTransport(err, context.Background()); k != c.kind || cl != c.class {
				t.Errorf("%v/%v, want %v/%v (%v)", k, cl, c.kind, c.class, err)
			}
			if errors.Is(err, dialhook.ErrUnmapped) {
				n.Unmapped()
			}
		})
	}

	// The hop deadline ending during a header stall is classDeadline,
	// though net/http's error also matches context.DeadlineExceeded.
	tr := guardedTransport(t, n, TransportOptions{})
	hopCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := fetchViaCtx(hopCtx, tr, "https://h2-stall.test/x")
	if k, cl := classifyTransport(err, hopCtx); k != FailTransient || cl != classDeadline {
		t.Errorf("hop deadline: %v/%v (%v)", k, cl, err)
	}
}

func TestClassifyStrategy(t *testing.T) {
	for _, c := range []struct {
		name   string
		err    error
		kind   FailureKind
		class  errClass
		status int
	}{
		{"nil", nil, FailNone, classNone, 0},
		{"not a short link", ErrNotShortLink, FailNone, classNone, 0},
		{"wrapped stop", fmt.Errorf("x: %w", ErrNotShortLink), FailNone, classNone, 0},
		{"fetchError", &fetchError{kind: FailTransport, class: classRefused}, FailTransport, classRefused, 0},
		{"wrapped fetchError", fmt.Errorf("strategy: %w", &fetchError{kind: FailTransient, class: classSlot}), FailTransient, classSlot, 0},
		{"fetchError status", &fetchError{kind: FailTransient, class: classStatus, status: 503}, FailTransient, classStatus, 503},
		{"no redirect", ErrNoRedirect, FailNoRedirect, classNone, 0},
		{"no match", fmt.Errorf("x: %w", ErrNoMatch), FailNoMatch, classNone, 0},
		{"bad target", ErrBadTarget, FailBadTarget, classBadTarget, 0},
		{"fetch denied", ErrFetchDenied, FailFetchDenied, classFetchDenied, 0},
		{"budget", ErrBudget, FailBudget, classBudget, 0},
		{"status 404", &StatusError{Host: "t.co", Status: 404}, FailStatus, classStatus, 404},
		{"status 999", &StatusError{Host: "lnkd.in", Status: 999}, FailStatus, classStatus, 999},
		{"status 500", &StatusError{Host: "t.co", Status: 500}, FailStatus, classStatus, 500},
		{"status 429", &StatusError{Host: "t.co", Status: 429}, FailTransient, classStatus, 429},
		{"status 502", fmt.Errorf("x: %w", &StatusError{Host: "t.co", Status: 502}), FailTransient, classStatus, 502},
		{"status 503", &StatusError{Host: "t.co", Status: 503}, FailTransient, classStatus, 503},
		{"status 504", &StatusError{Host: "t.co", Status: 504}, FailTransient, classStatus, 504},
		{"canceled", context.Canceled, FailTransient, classCanceled, 0},
		{"deadline", fmt.Errorf("x: %w", context.DeadlineExceeded), FailTransient, classDeadline, 0},
		{"chain kind loop", ErrLoop, FailTransient, classOther, 0},
		{"chain kind fallback", ErrFallback, FailTransient, classOther, 0},
		{"chain kind hop limit", ErrHopLimit, FailTransient, classOther, 0},
		{"umbrella only", ErrUnresolved, FailTransient, classOther, 0},
		{"transient", ErrTransient, FailTransient, classOther, 0},
		{"unknown", errors.New("strategy bug"), FailTransient, classOther, 0},
	} {
		k, cl, st := classifyStrategy(c.err)
		if k != c.kind || cl != c.class || st != c.status {
			t.Errorf("%s: %v/%v/%d, want %v/%v/%d", c.name, k, cl, st, c.kind, c.class, c.status)
		}
	}
}
