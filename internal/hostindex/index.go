package hostindex

import (
	"slices"
	"strings"

	"github.com/Espresso-Aficionados/linkclean/internal/matchpattern"
)

// Bucket holds the rules indexed under one host key: All of them, and the
// subset that was added as special. Both are ascending and deduplicated.
type Bucket struct {
	All, Special []int32
}

// Builder collects Include patterns. The zero value is ready to use.
type Builder struct {
	exact, suffix, label map[string]*Bucket
}

// Add registers rule ordinal ord under the host part of Include pattern p,
// in the Special lists too when special is set. Calls may come in any order
// and repeat. Patterns with host "*" are not indexed: a rule with one is a
// candidate for every host, which the caller knows without a lookup. Add
// panics on such a pattern.
func (b *Builder) Add(ord int32, p *matchpattern.Pattern, special bool) {
	var m *map[string]*Bucket
	kind, host := p.Host()
	switch kind {
	case matchpattern.HostExact:
		m = &b.exact
	case matchpattern.HostSuffix:
		m = &b.suffix
	case matchpattern.HostLabel:
		m = &b.label
	default:
		panic("hostindex: global pattern " + p.String())
	}
	if *m == nil {
		*m = map[string]*Bucket{}
	}
	bk := (*m)[host]
	if bk == nil {
		bk = &Bucket{}
		(*m)[host] = bk
	}
	bk.All = append(bk.All, ord)
	if special {
		bk.Special = append(bk.Special, ord)
	}
}

// Build freezes the index. The Builder must not be used afterwards.
func (b *Builder) Build() *Index {
	ix := &Index{exact: b.exact, suffix: b.suffix, label: b.label}
	for _, m := range []map[string]*Bucket{b.exact, b.suffix, b.label} {
		for _, bk := range m {
			bk.All, bk.Special = sortedSet(bk.All), sortedSet(bk.Special)
		}
	}
	for k := range b.suffix {
		ix.suffixLabels = max(ix.suffixLabels, strings.Count(k, ".")+1)
	}
	return ix
}

func sortedSet(l []int32) []int32 {
	slices.Sort(l)
	return slices.Clip(slices.Compact(l))
}

// Index maps a host view to the buckets of the rules whose Include host part
// could match it. Immutable; safe for concurrent use.
type Index struct {
	exact  map[string]*Bucket // "d" patterns, by d
	suffix map[string]*Bucket // "*.d" patterns, by d
	label  map[string]*Bucket // "label.*" and "*.label.*" patterns, by label
	// suffixLabels is the most labels of any suffix key. A parent domain
	// with more labels cannot be a key, so Lookup does not hash it.
	suffixLabels int
}

// Lookup appends to dst every distinct bucket whose rules could match host:
// the exact node, the "*." suffix nodes of host and of each parent domain
// (for an IP, host itself only), and the label map looked up by every label
// of host (a superset: no PSL needed; skipped for IPs, which have no site).
// Its cost is linear in len(host). Lookup does not allocate when dst has
// room.
func (ix *Index) Lookup(dst []*Bucket, host string, isIP bool) []*Bucket {
	start := len(dst)
	add := func(b *Bucket) {
		if b != nil && !slices.Contains(dst[start:], b) {
			dst = append(dst, b)
		}
	}
	add(ix.exact[host])
	add(ix.suffix[host])
	if isIP {
		return dst
	}
	end := len(host)
	for range ix.suffixLabels {
		i := strings.LastIndexByte(host[:end], '.')
		if i < 0 {
			break
		}
		add(ix.suffix[host[i+1:]])
		end = i
	}
	if len(ix.label) > 0 {
		for s := host; s != ""; {
			l, rest, _ := strings.Cut(s, ".")
			add(ix.label[l])
			s = rest
		}
	}
	return dst
}

// Merge appends to dst the ordinals in bs, ascending and deduplicated: the
// Special lists if special is set, else All. dst[:len(dst)] is left as is.
func Merge(dst []int32, bs []*Bucket, special bool) []int32 {
	start := len(dst)
	for _, b := range bs {
		if special {
			dst = append(dst, b.Special...)
		} else {
			dst = append(dst, b.All...)
		}
	}
	if len(bs) > 1 {
		out := dst[start:]
		slices.Sort(out)
		dst = dst[:start+len(slices.Compact(out))]
	}
	return dst
}

// Contains reports whether ord is in the All list of one of bs.
func Contains(bs []*Bucket, ord int32) bool {
	for _, b := range bs {
		if _, ok := slices.BinarySearch(b.All, ord); ok {
			return true
		}
	}
	return false
}
