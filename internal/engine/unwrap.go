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
	if !cl.u.HasQuery {
		return "", -1, false
	}
	for it := (specIter{cl: cl}); ; {
		ord, r, ok := it.next()
		if !ok {
			break
		}
		if r.action != rule.Unwrap || !cl.c.isActive(ord) || !cl.inScope(ord, r) {
			continue
		}
		for _, qp := range r.unwrapParams {
			v, found := cl.firstValue(cl.u.Query, qp)
			if !found {
				continue
			}
			t := query.DecodeValue(v)
			if r.prependScheme != "" && !hasHTTPPrefix(t) {
				t = r.prependScheme + "://" + t
			}
			// VALID bounds the decoded target (§6). Reserialize can grow it
			// up to 3x, so the target may exceed MaxURLLength, which bounds
			// the input; Clean then returns that output unchanged.
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

// firstValue returns the value of the first segment of query q whose
// canonical name is cn.
func (cl *call) firstValue(q, cn string) (string, bool) {
	var nb [128]byte
	for {
		s, rest, ok := query.Next(q)
		if !ok {
			return "", false
		}
		if string(cl.canon(&nb, s.Name)) == cn {
			return s.Value, true
		}
		q = rest
	}
}

// hop describes the unwrap of cl's URL to to by rule ord (§3.1, §6).
func (cl *call) hop(to string, ord int32) Hop {
	r := cl.c.rule(ord)
	h := Hop{From: cl.u.Raw, To: to, Rule: r.id, Category: CatRedirectUnwrapped, FromHost: cl.u.Host}
	if !r.affiliate {
		return h
	}
	h.Category = CatAffiliate
	h.Beneficiary.Network = r.network
	switch {
	case r.idParam != "":
		if v, ok := cl.firstValue(cl.u.Query, r.idParam); ok {
			h.Beneficiary.ID = query.DecodeValue(v)
		}
	case r.idPath != nil:
		if m := r.idPath.FindStringSubmatch(cl.u.Path); m != nil {
			h.Beneficiary.ID = query.DecodeValue(m[1])
		}
	}
	return h
}
