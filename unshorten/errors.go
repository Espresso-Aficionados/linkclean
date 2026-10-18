package unshorten

import (
	"errors"
	"strconv"

	"code.dny.dev/ssrf"
)

// Errors. Every error Resolve and Process return wraps ErrUnresolved and the
// specific sentinel, and errors.Is keeps working after a cache hit.
var (
	ErrUnresolved         = errors.New("unshorten: not resolved") // umbrella; every failure wraps it
	ErrHopLimit           = errors.New("unshorten: hop limit reached")
	ErrLoop               = errors.New("unshorten: redirect loop")
	ErrFallback           = errors.New("unshorten: shortener returned its fallback page")
	ErrNoRedirect         = errors.New("unshorten: expected a redirect")
	ErrNoMatch            = errors.New("unshorten: extractor found no URL")
	ErrBadTarget          = errors.New("unshorten: target failed scheme/port/userinfo/IP checks")
	ErrFetchDenied        = errors.New("unshorten: strategy tried to fetch a host outside its entry")
	ErrBudget             = errors.New("unshorten: request budget exhausted")
	ErrNotShortLink       = errors.New("unshorten: not a short link") // a strategy may return it: stop, no failure
	ErrTransient          = errors.New("unshorten: transient failure (timeout, cancellation, rate limit or overload); not cached")
	ErrUnguardedTransport = errors.New("unshorten: transport is not verifiably SSRF-guarded")
)

// StatusError reports an unexpected HTTP status; it wraps ErrUnresolved.
type StatusError struct {
	Host   string
	Status int
}

func (e *StatusError) Error() string {
	return "unshorten: unexpected HTTP status " + strconv.Itoa(e.Status) + " from " + e.Host
}

func (e *StatusError) Unwrap() error { return ErrUnresolved }

// transientStatus reports whether an HTTP status is rate limiting or gateway
// overload (429, 502, 503, 504): a transient failure, never cached.
func transientStatus(status int) bool {
	switch status {
	case 429, 502, 503, 504:
		return true
	}
	return false
}

// resolveError is every error Resolve, Process and RequestInfo.Err carry.
// msg names the entry, the kind and at most a class: never a URL, path,
// query, IP, port or the text of an underlying error. errs holds only
// package-level sentinels and *StatusError values, never a *url.Error,
// *net.OpError or ssrf-formatted error, which embed the URL or address.
type resolveError struct {
	msg  string
	errs []error
}

func (e *resolveError) Error() string   { return e.msg }
func (e *resolveError) Unwrap() []error { return e.errs }

// errClass is the detail of a failure below its FailureKind. It is never
// cached (CacheEntry has no field for it), so it appears only in the text of
// an error built on the fresh path and decides RequestInfo.Err's extra
// sentinels.
type errClass uint8

const (
	classNone         errClass = iota
	classTimeout               // a transport timeout, or the resolve-level deadline
	classDeadline              // the detached hop deadline, or the caller's own deadline
	classCanceled              // the caller's cancellation
	classDNSNotFound           // DNS NXDOMAIN
	classDNSTemporary          // DNS timeout or temporary failure
	classRefused               // connection refused
	classTLSVerify             // TLS certificate verification failed
	classGuardDenied           // the dial guard refused the address dialed
	classLimiter               // Limiter.Wait failed
	classSlot                  // a request-slot wait ended
	classInFlight              // MaxInFlightPerHost or MaxInFlight reached
	classStatus                // an HTTP status
	classBudget                // the per-hop request budget
	classFetchDenied           // a host outside the entry
	classBadTarget             // scheme, port, userinfo, IP or form checks
	classOther                 // anything else
)

var classNames = [...]string{
	classNone:         "none",
	classTimeout:      "timeout",
	classDeadline:     "deadline exceeded",
	classCanceled:     "canceled",
	classDNSNotFound:  "DNS name not found",
	classDNSTemporary: "temporary DNS failure",
	classRefused:      "connection refused",
	classTLSVerify:    "TLS certificate verification failed",
	classGuardDenied:  "dial guard denied the address",
	classLimiter:      "rate limited",
	classSlot:         "no request slot before the deadline",
	classInFlight:     "too many lookups in flight",
	classStatus:       "HTTP status",
	classBudget:       "request budget exhausted",
	classFetchDenied:  "host outside the entry",
	classBadTarget:    "bad target",
	classOther:        "other",
}

func (c errClass) String() string {
	if int(c) < len(classNames) {
		return classNames[c]
	}
	return "errClass(" + strconv.Itoa(int(c)) + ")"
}

// detail is the class text an error message appends: "" for classNone and
// for the classes that only restate their kind.
func (c errClass) detail() string {
	switch c {
	case classNone, classStatus, classBudget, classFetchDenied, classBadTarget:
		return ""
	}
	return c.String()
}

// kindSentinel returns the sentinel a kind wraps besides ErrUnresolved, or
// nil for FailNone, FailStatus and FailTransport.
func kindSentinel(k FailureKind) error {
	switch k {
	case FailHopLimit:
		return ErrHopLimit
	case FailLoop:
		return ErrLoop
	case FailFallback:
		return ErrFallback
	case FailNoRedirect:
		return ErrNoRedirect
	case FailNoMatch:
		return ErrNoMatch
	case FailBadTarget:
		return ErrBadTarget
	case FailFetchDenied:
		return ErrFetchDenied
	case FailBudget:
		return ErrBudget
	case FailTransient:
		return ErrTransient
	}
	return nil
}

// kindText is the message text of a kind.
func kindText(k FailureKind, status int) string {
	switch k {
	case FailNone:
		return "not a short link"
	case FailHopLimit:
		return "hop limit reached"
	case FailLoop:
		return "redirect loop"
	case FailFallback:
		return "shortener returned its fallback page"
	case FailNoRedirect:
		return "expected a redirect"
	case FailNoMatch:
		return "extractor found no URL"
	case FailBadTarget:
		return "target failed scheme/port/userinfo/IP checks"
	case FailStatus:
		return "unexpected HTTP status " + strconv.Itoa(status)
	case FailFetchDenied:
		return "strategy tried to fetch a host outside its entry"
	case FailTransport:
		return "transport error"
	case FailBudget:
		return "request budget exhausted"
	case FailTransient:
		if transientStatus(status) {
			return "transient failure: HTTP " + strconv.Itoa(status)
		}
		return "transient failure"
	}
	return k.String()
}

// fetchError is the error Fetcher.Get returns for a failure past its checks
// (a slot wait, the Limiter, the transport). Its text names the class only.
// classifyStrategy recovers kind and class with errors.As, so a strategy that
// returns it (wrapped or not) keeps the classification. It unwraps to
// ErrUnresolved and its kind's sentinel (ErrTransient for a transient one).
type fetchError struct {
	kind   FailureKind
	class  errClass
	status int
}

func (e *fetchError) Error() string {
	return "unshorten: fetch failed: " + e.class.String()
}

func (e *fetchError) Unwrap() []error {
	if s := kindSentinel(e.kind); s != nil {
		return []error{ErrUnresolved, s}
	}
	return []error{ErrUnresolved}
}

// rehydrate builds the error Resolve and Process return for a failed hop or
// chain, from the outcome alone, so it is the same whether e came from the
// network or the Cache; class adds text only. It is
// "unshorten: <entry>: <kind>[: <class>]" wrapping ErrUnresolved and:
//
//   - FailHopLimit … FailBudget other than the two below: the kind's sentinel;
//   - FailStatus: &StatusError{host, e.Status};
//   - FailTransport: nothing more, fresh or cached;
//   - FailTransient: ErrTransient, ctxErr when non-nil (pass the caller's
//     ctx.Err() only when the caller's own ctx ended), and &StatusError for
//     429, 502, 503 and 504;
//   - FailNone: ErrNotShortLink (a stop on an Intermediate entry, which must
//     fail open rather than return the redirector).
//
// host is the host of the hop URL being resolved.
func rehydrate(e CacheEntry, entry, host string, class errClass, ctxErr error) error {
	errs := []error{ErrUnresolved}
	switch e.Failure {
	case FailNone:
		errs = append(errs, ErrNotShortLink)
	case FailStatus:
		errs = append(errs, &StatusError{Host: host, Status: e.Status})
	case FailTransport:
	case FailTransient:
		errs = append(errs, ErrTransient)
		if ctxErr != nil {
			errs = append(errs, ctxErr)
		}
		if transientStatus(e.Status) {
			errs = append(errs, &StatusError{Host: host, Status: e.Status})
		}
	default:
		if s := kindSentinel(e.Failure); s != nil {
			errs = append(errs, s)
		}
	}
	return &resolveError{msg: message(entry, kindText(e.Failure, e.Status), class), errs: errs}
}

// requestError builds RequestInfo.Err for one request: nil for FailNone,
// else rehydrate's error (without a ctx error: a request runs on the
// detached hop context) plus, per request, the class's sentinel: the
// matching ssrf.ErrProhibitedIP, ErrProhibitedPort or ErrProhibitedNetwork
// found in cause for a dial-guard denial, and ErrTransient for a timeout or
// deadline class. Only those package-level values are taken from cause.
func requestError(entry, host string, kind FailureKind, class errClass, status int, cause error) error {
	if kind == FailNone {
		return nil
	}
	re := rehydrate(CacheEntry{Failure: kind, Status: status}, entry, host, class, nil).(*resolveError)
	switch class {
	case classGuardDenied:
		for _, s := range []error{ssrf.ErrProhibitedIP, ssrf.ErrProhibitedPort, ssrf.ErrProhibitedNetwork} {
			if errors.Is(cause, s) {
				re.errs = append(re.errs, s)
			}
		}
	case classTimeout, classDeadline:
		if kind != FailTransient {
			re.errs = append(re.errs, ErrTransient)
		}
	}
	return re
}

func message(entry, kind string, class errClass) string {
	msg := "unshorten: "
	if entry != "" {
		msg += entry + ": "
	}
	msg += kind
	if d := class.detail(); d != "" {
		msg += ": " + d
	}
	return msg
}
