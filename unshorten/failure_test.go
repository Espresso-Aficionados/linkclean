package unshorten

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"code.dny.dev/ssrf"
)

func TestFailureKindString(t *testing.T) {
	want := []string{"none", "hop_limit", "loop", "fallback", "no_redirect", "no_match", "bad_target",
		"status", "fetch_denied", "transport", "budget", "transient"}
	for k := FailNone; k <= FailTransient; k++ {
		if got := k.String(); got != want[k] {
			t.Errorf("FailureKind(%d).String() = %q, want %q", k, got, want[k])
		}
	}
	if got := FailureKind(200).String(); got != "FailureKind(200)" {
		t.Errorf("unknown kind: %q", got)
	}
	for c := classNone; c <= classOther; c++ {
		if s := c.String(); s == "" || strings.HasPrefix(s, "errClass(") {
			t.Errorf("errClass(%d) has no name", c)
		}
	}
}

func TestStatusError(t *testing.T) {
	err := error(&StatusError{Host: "lnkd.in", Status: 999})
	if !errors.Is(err, ErrUnresolved) {
		t.Error("StatusError does not wrap ErrUnresolved")
	}
	if got := err.Error(); got != "unshorten: unexpected HTTP status 999 from lnkd.in" {
		t.Errorf("Error() = %q", got)
	}
}

func TestRehydrate(t *testing.T) {
	const host = "vm.tiktok.com"
	all := []error{ErrUnresolved, ErrHopLimit, ErrLoop, ErrFallback, ErrNoRedirect, ErrNoMatch, ErrBadTarget,
		ErrFetchDenied, ErrBudget, ErrNotShortLink, ErrTransient, ErrUnguardedTransport,
		context.Canceled, context.DeadlineExceeded}
	tests := []struct {
		e      CacheEntry
		class  errClass
		ctxErr error
		want   []error // besides ErrUnresolved
		status int     // wrapped *StatusError, 0 for none
		msg    string
	}{
		{CacheEntry{Failure: FailHopLimit}, classNone, nil, []error{ErrHopLimit}, 0, "unshorten: t.co: hop limit reached"},
		{CacheEntry{Failure: FailLoop}, classNone, nil, []error{ErrLoop}, 0, "unshorten: t.co: redirect loop"},
		{CacheEntry{Failure: FailFallback}, classNone, nil, []error{ErrFallback}, 0, "unshorten: t.co: shortener returned its fallback page"},
		{CacheEntry{Failure: FailNoRedirect, Status: 404}, classNone, nil, []error{ErrNoRedirect}, 0, "unshorten: t.co: expected a redirect"},
		{CacheEntry{Failure: FailNoMatch}, classNone, nil, []error{ErrNoMatch}, 0, "unshorten: t.co: extractor found no URL"},
		{CacheEntry{Failure: FailBadTarget}, classBadTarget, nil, []error{ErrBadTarget}, 0, "unshorten: t.co: target failed scheme/port/userinfo/IP checks"},
		{CacheEntry{Failure: FailFetchDenied}, classFetchDenied, nil, []error{ErrFetchDenied}, 0, "unshorten: t.co: strategy tried to fetch a host outside its entry"},
		{CacheEntry{Failure: FailBudget}, classBudget, nil, []error{ErrBudget}, 0, "unshorten: t.co: request budget exhausted"},
		{CacheEntry{Failure: FailStatus, Status: 999}, classStatus, nil, nil, 999, "unshorten: t.co: unexpected HTTP status 999"},
		{CacheEntry{Failure: FailTransport}, classGuardDenied, nil, nil, 0, "unshorten: t.co: transport error: dial guard denied the address"},
		{CacheEntry{Failure: FailTransport}, classNone, nil, nil, 0, "unshorten: t.co: transport error"},
		{CacheEntry{Failure: FailTransient}, classTimeout, nil, []error{ErrTransient}, 0, "unshorten: t.co: transient failure: timeout"},
		{CacheEntry{Failure: FailTransient}, classCanceled, context.Canceled, []error{ErrTransient, context.Canceled}, 0, "unshorten: t.co: transient failure: canceled"},
		{CacheEntry{Failure: FailTransient}, classDeadline, context.DeadlineExceeded, []error{ErrTransient, context.DeadlineExceeded}, 0, "unshorten: t.co: transient failure: deadline exceeded"},
		{CacheEntry{Failure: FailTransient, Status: 503}, classNone, nil, []error{ErrTransient}, 503, "unshorten: t.co: transient failure: HTTP 503"},
		{CacheEntry{Failure: FailTransient, Status: 500}, classOther, nil, []error{ErrTransient}, 0, "unshorten: t.co: transient failure: other"},
		{CacheEntry{Failure: FailNone}, classNone, nil, []error{ErrNotShortLink}, 0, "unshorten: t.co: not a short link"},
	}
	for _, tt := range tests {
		err := rehydrate(tt.e, "t.co", host, tt.class, tt.ctxErr)
		if got := err.Error(); got != tt.msg {
			t.Errorf("%v: Error() = %q, want %q", tt.e, got, tt.msg)
		}
		if !errors.Is(err, ErrUnresolved) {
			t.Errorf("%v: does not wrap ErrUnresolved", tt.e)
		}
		for _, s := range all[1:] {
			want := false
			for _, w := range tt.want {
				want = want || w == s
			}
			if errors.Is(err, s) != want {
				t.Errorf("%v: errors.Is(%v) = %v, want %v", tt.e, s, !want, want)
			}
		}
		var se *StatusError
		switch ok := errors.As(err, &se); {
		case tt.status == 0 && ok:
			t.Errorf("%v: wraps %v", tt.e, se)
		case tt.status != 0 && (!ok || se.Status != tt.status || se.Host != host):
			t.Errorf("%v: StatusError = %v, want %d from %s", tt.e, se, tt.status, host)
		}
		if fmt.Sprintf("%+v", err) != tt.msg {
			t.Errorf("%v: %%+v differs from Error()", tt.e)
		}
	}
	if err := rehydrate(CacheEntry{Failure: FailLoop}, "", host, classNone, nil); err.Error() != "unshorten: redirect loop" {
		t.Errorf("no entry: %q", err)
	}
}

// TestRequestError covers RequestInfo.Err's per-request sentinels: the ssrf
// sentinel for a guard denial, ErrTransient for a timeout class, and nothing
// from the cause's text.
func TestRequestError(t *testing.T) {
	if err := requestError("t.co", "t.co", FailNone, classNone, 301, nil); err != nil {
		t.Errorf("FailNone: %v", err)
	}
	cause := fmt.Errorf("dial tcp 10.0.0.1:443: %w: 10.0.0.1 is not a permitted destination", ssrf.ErrProhibitedIP)
	err := requestError("t.co", "t.co", FailTransport, classGuardDenied, 0, cause)
	if !errors.Is(err, ErrUnresolved) || !errors.Is(err, ssrf.ErrProhibitedIP) || errors.Is(err, ssrf.ErrProhibitedPort) || errors.Is(err, ErrTransient) {
		t.Errorf("guard denial: wrong sentinels: %v", err)
	}
	if strings.Contains(err.Error(), "10.0.0.1") {
		t.Errorf("guard denial text has the address: %q", err)
	}
	err = requestError("t.co", "t.co", FailTransient, classTimeout, 0, context.DeadlineExceeded)
	if !errors.Is(err, ErrTransient) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout: wrong sentinels: %v", err)
	}
	err = requestError("t.co", "t.co", FailTransient, classNone, 429, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 429 {
		t.Errorf("429: %v", err)
	}
}

func TestFetchError(t *testing.T) {
	err := fmt.Errorf("strategy: %w", &fetchError{kind: FailTransient, class: classSlot})
	var fe *fetchError
	if !errors.As(err, &fe) || fe.class != classSlot {
		t.Fatal("errors.As lost the fetchError")
	}
	if !errors.Is(err, ErrTransient) || !errors.Is(err, ErrUnresolved) {
		t.Error("transient fetchError does not wrap ErrTransient and ErrUnresolved")
	}
	if got := fe.Error(); got != "unshorten: fetch failed: no request slot before the deadline" {
		t.Errorf("Error() = %q", got)
	}
	tr := &fetchError{kind: FailTransport, class: classRefused}
	if errors.Is(tr, ErrTransient) || !errors.Is(tr, ErrUnresolved) {
		t.Error("transport fetchError sentinels")
	}
}

func TestTransientStatus(t *testing.T) {
	for s := 100; s < 1000; s++ {
		want := s == 429 || s == 502 || s == 503 || s == 504
		if transientStatus(s) != want {
			t.Errorf("transientStatus(%d) = %v", s, !want)
		}
	}
}
