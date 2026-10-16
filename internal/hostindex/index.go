package hostindex

import (
	"slices"
	"strings"

	"github.com/Espresso-Aficionados/linkclean/internal/matchpattern"
)

// Builder collects Include patterns. The zero value is ready to use.
type Builder struct {
	exact, suffix, label map[string][]int32
	global               []int32
}

// Add registers rule ordinal ord under the host part of Include pattern p.
// Calls may come in any order and repeat.
func (b *Builder) Add(ord int32, p *matchpattern.Pattern) {
	if b.exact == nil {
		b.exact = map[string][]int32{}
		b.suffix = map[string][]int32{}
		b.label = map[string][]int32{}
	}
	kind, host := p.Host()
	switch kind {
	case matchpattern.HostAny:
		b.global = append(b.global, ord)
	case matchpattern.HostExact:
		b.exact[host] = append(b.exact[host], ord)
	case matchpattern.HostSuffix:
		b.suffix[host] = append(b.suffix[host], ord)
	case matchpattern.HostLabel:
		b.label[host] = append(b.label[host], ord)
	}
}

// Build freezes the index. The Builder must not be used afterwards.
func (b *Builder) Build() *Index {
	for _, m := range []map[string][]int32{b.exact, b.suffix, b.label} {
		for k, l := range m {
			m[k] = sortedSet(l)
		}
	}
	return &Index{
		exact:  b.exact,
		suffix: b.suffix,
		label:  b.label,
		global: sortedSet(b.global),
	}
}

func sortedSet(l []int32) []int32 {
	slices.Sort(l)
	return slices.Clip(slices.Compact(l))
}

// Index maps a host view to candidate rule ordinals. Immutable; safe for
// concurrent use.
type Index struct {
	exact  map[string][]int32 // "d" patterns, by d
	suffix map[string][]int32 // "*.d" patterns, by d
	label  map[string][]int32 // "label.*" and "*.label.*" patterns, by label
	global []int32            // "*" patterns
}

// Candidates appends to dst the ordinals of every rule with an Include whose
// host part could match host: the exact node, the "*." suffix nodes of host
// and of each parent domain (for an IP, host itself only), the label map
// looked up by every label of host (a superset: no PSL needed; skipped for
// IPs, which have no site), and the global bucket. The appended ordinals are
// ascending and deduplicated; dst[:len(dst)] is left as is. Candidates does
// not allocate when dst has room.
func (ix *Index) Candidates(dst []int32, host string, isIP bool) []int32 {
	start := len(dst)
	sources := 0
	add := func(l []int32) {
		if len(l) > 0 {
			dst = append(dst, l...)
			sources++
		}
	}
	add(ix.exact[host])
	add(ix.suffix[host])
	if !isIP {
		for s := host; ; {
			i := strings.IndexByte(s, '.')
			if i < 0 {
				break
			}
			s = s[i+1:]
			add(ix.suffix[s])
		}
		if len(ix.label) > 0 {
			for s := host; s != ""; {
				l, rest, _ := strings.Cut(s, ".")
				add(ix.label[l])
				s = rest
			}
		}
	}
	add(ix.global)
	if sources > 1 {
		out := dst[start:]
		slices.Sort(out)
		dst = dst[:start+len(slices.Compact(out))]
	}
	return dst
}
