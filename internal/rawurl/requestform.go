package rawurl

import "strings"

// RequestForm returns the form in which package unshorten keys and requests
// u:
//
//	Scheme "://" [userinfo "@"] Host [":" Port] Path ["?" Query]
//
// Scheme is lowercase and Host is the host view; userinfo and the port
// (with its ':', even when the port is empty), Path and Query are as
// written. The fragment is dropped. A bare '?' is kept and an empty Path
// stays empty: nothing else is changed. Unlike MatchString it keeps userinfo
// and a default port, and adds no '/'. When Raw is already in that form,
// RequestForm returns Raw and does not allocate.
func (u *URL) RequestForm() string {
	ui := u.Authority[:strings.LastIndexByte(u.Authority, '@')+1]
	hp := u.Authority[len(ui):]
	// splitHostPort cuts the port at the last ':' (after the ']' of an IPv6
	// literal), so a ':' was written iff it precedes Port.
	colon := len(hp) > len(u.Port) && hp[len(hp)-len(u.Port)-1] == ':'
	n := len(u.Scheme) + 3 + len(ui) + len(u.Host)
	if colon {
		n += 1 + len(u.Port)
	}
	p := u.Prefix
	if !u.HasFragment && len(p) == n && p[:len(u.Scheme)] == u.Scheme &&
		p[len(u.Scheme)+3+len(ui):len(u.Scheme)+3+len(ui)+len(u.Host)] == u.Host {
		return u.Raw
	}
	var b strings.Builder
	b.Grow(n + len(u.Path) + 1 + len(u.Query))
	b.WriteString(u.Scheme)
	b.WriteString("://")
	b.WriteString(ui)
	b.WriteString(u.Host)
	if colon {
		b.WriteByte(':')
		b.WriteString(u.Port)
	}
	b.WriteString(u.Path)
	if u.HasQuery {
		b.WriteByte('?')
		b.WriteString(u.Query)
	}
	return b.String()
}
