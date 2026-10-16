package engine

import (
	"github.com/Espresso-Aficionados/linkclean/internal/query"
	"github.com/Espresso-Aficionados/linkclean/internal/rawurl"
	"github.com/Espresso-Aficionados/linkclean/internal/rule"
)

// unwrapOnce finds the first in-scope Unwrap rule, by ordinal, that extracts
// a VALID target from cl's URL (§6), and returns the target re-serialized
// (§5.4).
func (cl *call) unwrapOnce() (target string, ord int32, ok bool) {
	if !cl.hasQuery {
		return "", -1, false
	}
	var sb [32]query.Segment
	var segs []query.Segment
	for pos, ord := range cl.cands() {
		r := cl.c.rule(ord)
		if r.action != rule.Unwrap || !cl.c.isActive(ord) || !cl.inScope(pos, r) {
			continue
		}
		if segs == nil {
			segs = query.Parse(sb[:0], cl.query)
		}
		for _, qp := range r.unwrapParams {
			v, found := firstValue(segs, qp)
			if !found {
				continue
			}
			t := query.DecodeValue(v)
			if r.prependScheme != "" && !hasHTTPPrefix(t) {
				t = r.prependScheme + "://" + t
			}
			if len(t) > cl.c.opt.MaxURLLength {
				continue
			}
			tu, ok := rawurl.ParseTarget(t)
			if !ok || !r.allowSameSite && rawurl.Site(tu.Host) == cl.site() {
				continue
			}
			return rawurl.Reserialize(&tu), ord, true
		}
	}
	return "", -1, false
}

// firstValue returns the value of the first segment whose canonical name is
// cn.
func firstValue(segs []query.Segment, cn string) (string, bool) {
	var nb [64]byte
	for i := range segs {
		if string(query.AppendCanon(nb[:0], segs[i].Name)) == cn {
			return segs[i].Value, true
		}
	}
	return "", false
}

// hop describes the unwrap of cl's URL (from) to to by rule ord (§3.1, §6).
func (cl *call) hop(from, to string, ord int32) Hop {
	r := cl.c.rule(ord)
	h := Hop{From: from, To: to, Rule: r.id, Category: CatRedirectUnwrapped, FromHost: cl.u.Host}
	if !r.affiliate {
		return h
	}
	h.Category = CatAffiliate
	h.Beneficiary.Network = r.network
	switch {
	case r.idParam != "":
		var sb [32]query.Segment
		if v, ok := firstValue(query.Parse(sb[:0], cl.query), r.idParam); ok {
			h.Beneficiary.ID = query.DecodeValue(v)
		}
	case r.idPath != nil:
		if m := r.idPath.FindStringSubmatch(cl.u.Path); m != nil {
			h.Beneficiary.ID = query.DecodeValue(m[1])
		}
	}
	return h
}
