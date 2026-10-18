package unshorten

import "context"

// classifyTransport classifies an error from client.Do by type only, never
// by message text. hopCtx tells a hop-deadline timeout (classDeadline) from
// a transport timeout (classTimeout). Definitive (FailTransport): a
// dial-guard denial, DNS not found, connection refused, a TLS certificate
// verification failure. Everything else is FailTransient.
func classifyTransport(err error, hopCtx context.Context) (FailureKind, errClass) {
	panic("unshorten: not implemented")
}

// classifyStrategy classifies the error a Strategy's Next returned: a
// *fetchError keeps its kind and class; the sentinels map to their kinds; a
// *StatusError is transient for 429, 502, 503 and 504, else FailStatus with
// its status; a context error and any other error are transient.
func classifyStrategy(err error) (FailureKind, errClass, int) {
	panic("unshorten: not implemented")
}
