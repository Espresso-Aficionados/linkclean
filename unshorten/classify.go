package unshorten

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"syscall"

	"code.dny.dev/ssrf"

	"github.com/Espresso-Aficionados/linkclean/unshorten/internal/dialhook"
)

// classifyTransport classifies an error from client.Do by type only, never
// by message text. hopCtx tells a hop-deadline timeout (classDeadline) from
// a transport timeout (classTimeout). Definitive (FailTransport): a
// dial-guard denial, DNS not found, connection refused, a TLS certificate
// verification failure. Everything else is FailTransient.
//
// The order matters. A cancellation wins over everything; a guard denial
// over a timeout (a denial is definitive and must be cached); any timeout,
// a DNS timeout included, over the DNS and dial classes. A tls.AlertError,
// ssrf.ErrInvalidHostPort, a proxy's refusal of CONNECT and every other
// error are transient.
func classifyTransport(err error, hopCtx context.Context) (FailureKind, errClass) {
	var (
		netErr  net.Error
		dnsErr  *net.DNSError
		certErr *tls.CertificateVerificationError
	)
	switch {
	case err == nil:
		return FailNone, classNone
	case errors.Is(err, context.Canceled):
		return FailTransient, classCanceled
	case errors.Is(err, ssrf.ErrProhibitedIP), errors.Is(err, ssrf.ErrProhibitedPort), errors.Is(err, ssrf.ErrProhibitedNetwork):
		return FailTransport, classGuardDenied
	case errors.Is(err, dialhook.ErrUnmapped):
		return FailTransient, classOther
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		if hopCtx != nil && hopCtx.Err() != nil {
			return FailTransient, classDeadline
		}
		return FailTransient, classTimeout
	case errors.As(err, &dnsErr):
		if dnsErr.IsNotFound {
			return FailTransport, classDNSNotFound
		}
		return FailTransient, classDNSTemporary
	case errors.Is(err, syscall.ECONNREFUSED):
		return FailTransport, classRefused
	case errors.As(err, &certErr):
		return FailTransport, classTLSVerify
	}
	return FailTransient, classOther
}

// classifyStrategy classifies the error a Strategy's Next returned: a
// *fetchError keeps its kind and class; the sentinels map to their kinds; a
// *StatusError is transient for 429, 502, 503 and 504, else FailStatus with
// its status; a context error and any other error are transient.
//
// The sentinels are ErrNoRedirect, ErrNoMatch, ErrBadTarget, ErrFetchDenied
// and ErrBudget. nil and ErrNotShortLink (a stop, not a failure) are
// FailNone. Any other sentinel a strategy returns, a chain-level one
// (ErrHopLimit, ErrLoop, ErrFallback) included, is transient: a strategy
// cannot make the Resolver cache a chain-level kind.
func classifyStrategy(err error) (FailureKind, errClass, int) {
	var (
		fe *fetchError
		se *StatusError
	)
	switch {
	case err == nil, errors.Is(err, ErrNotShortLink):
		return FailNone, classNone, 0
	case errors.As(err, &fe):
		return fe.kind, fe.class, fe.status
	case errors.Is(err, ErrNoRedirect):
		return FailNoRedirect, classNone, 0
	case errors.Is(err, ErrNoMatch):
		return FailNoMatch, classNone, 0
	case errors.Is(err, ErrBadTarget):
		return FailBadTarget, classBadTarget, 0
	case errors.Is(err, ErrFetchDenied):
		return FailFetchDenied, classFetchDenied, 0
	case errors.Is(err, ErrBudget):
		return FailBudget, classBudget, 0
	case errors.As(err, &se):
		if transientStatus(se.Status) {
			return FailTransient, classStatus, se.Status
		}
		return FailStatus, classStatus, se.Status
	case errors.Is(err, context.Canceled):
		return FailTransient, classCanceled, 0
	case errors.Is(err, context.DeadlineExceeded):
		return FailTransient, classDeadline, 0
	}
	return FailTransient, classOther, 0
}
