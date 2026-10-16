package engine

import (
	"slices"
	"strings"

	"github.com/Espresso-Aficionados/linkclean/internal/matchpattern"
	"github.com/Espresso-Aficionados/linkclean/internal/query"
	"github.com/Espresso-Aficionados/linkclean/internal/rule"
)

// Options are a Cleaner's knobs, already defaulted and validated by the
// caller (linkclean.New).
type Options struct {
	AffiliateStrip bool

	DisableRules        []string // full rule IDs, exact
	DisableRulePrefixes []string
	DisableTags         []string

	DisableUnwrap       bool
	DisableNested       bool
	DisableSignedGuard  bool
	DisableArchiveGuard bool

	MaxUnwrap    int // > 0
	MaxURLLength int // > 0

	ExtraSignedParams []string // compared canonically
	ExtraArchive      []*matchpattern.Pattern
}

// Unmatched names a Disable* entry that matches no rule: Field is
// "DisableRules", "DisableRulePrefixes" or "DisableTags", Index its position.
type Unmatched struct {
	Field string
	Index int
}

// Cleaner cleans URLs with one or more Programs under fixed Options. Global
// ordinals run through the programs in order, so the first program's rules
// have the highest priority. Immutable; safe for concurrent use.
type Cleaner struct {
	progs   []*Program
	offs    []int32  // global ordinal of each program's first rule
	active  []uint64 // bitset: the rule is enabled and its affiliate gate is open
	opt     Options
	signed  map[string]struct{}
	archive []*matchpattern.Pattern // ExtraArchive; the built-ins are in builtinArchive
}

// NewCleaner builds a Cleaner over progs, which it shares, not copies. It
// reports every Disable* entry that matches no rule.
func NewCleaner(progs []*Program, opt Options) (*Cleaner, []Unmatched) {
	c := &Cleaner{progs: slices.Clone(progs), opt: opt, signed: builtinSigned, archive: opt.ExtraArchive}
	n := int32(0)
	for _, p := range progs {
		c.offs = append(c.offs, n)
		n += int32(len(p.rules))
	}
	c.active = make([]uint64, (n+63)/64)

	if len(opt.ExtraSignedParams) > 0 {
		c.signed = make(map[string]struct{}, len(builtinSigned)+len(opt.ExtraSignedParams))
		for k := range builtinSigned {
			c.signed[k] = struct{}{}
		}
		for _, s := range opt.ExtraSignedParams {
			c.signed[query.Canon(s)] = struct{}{}
		}
	}

	exact := make(map[string]int, len(opt.DisableRules))
	for i, id := range opt.DisableRules {
		if _, dup := exact[id]; !dup {
			exact[id] = i
		}
	}
	hitRule := make([]bool, len(opt.DisableRules))
	hitPrefix := make([]bool, len(opt.DisableRulePrefixes))
	hitTag := make([]bool, len(opt.DisableTags))
	ord := int32(0)
	for _, p := range progs {
		for i := range p.rules {
			r := &p.rules[i]
			on := true
			if r.affiliate {
				on = (r.action == rule.Keep) != opt.AffiliateStrip
			}
			if _, ok := exact[r.id]; ok {
				for j, id := range opt.DisableRules {
					if id == r.id {
						hitRule[j] = true
					}
				}
				on = false
			}
			for j, pre := range opt.DisableRulePrefixes {
				if strings.HasPrefix(r.id, pre) {
					hitPrefix[j] = true
					on = false
				}
			}
			for j, tag := range opt.DisableTags {
				if slices.Contains(r.tags, tag) {
					hitTag[j] = true
					on = false
				}
			}
			if on {
				c.active[ord/64] |= 1 << (ord % 64)
			}
			ord++
		}
	}
	var un []Unmatched
	for _, f := range [...]struct {
		name string
		hit  []bool
	}{{"DisableRules", hitRule}, {"DisableRulePrefixes", hitPrefix}, {"DisableTags", hitTag}} {
		for i, ok := range f.hit {
			if !ok {
				un = append(un, Unmatched{Field: f.name, Index: i})
			}
		}
	}
	return c, un
}

func (c *Cleaner) isActive(ord int32) bool { return c.active[ord/64]&(1<<(ord%64)) != 0 }

// rule returns the rule with global ordinal ord.
func (c *Cleaner) rule(ord int32) *crule {
	i := len(c.offs) - 1
	for c.offs[i] > ord {
		i--
	}
	return &c.progs[i].rules[ord-c.offs[i]]
}

// builtinSigned is the signature parameter list of §5.5, canonical.
var builtinSigned = func() map[string]struct{} {
	m := map[string]struct{}{}
	for _, s := range strings.Fields(`usg sdata sig signature hmac hm x-amz-signature x-amz-credential
		x-amz-security-token x-goog-signature x-goog-credential googleaccessid key-pair-id policy`) {
		m[s] = struct{}{}
	}
	return m
}()

// builtinArchive is the archive guard of §5.5, keyed by pattern host (every
// built-in pattern has an exact host).
var builtinArchive = func() map[string][]*matchpattern.Pattern {
	m := map[string][]*matchpattern.Pattern{}
	for _, s := range strings.Fields(`
		*://web.archive.org/web/*  *://wayback.archive-it.org/*  *://arquivo.pt/wayback/*
		*://archive.today/*  *://archive.ph/*  *://archive.is/*  *://archive.md/*  *://archive.li/*
		*://archive.vn/*  *://archive.fo/*  *://ghostarchive.org/*  *://webcache.googleusercontent.com/*
		*://urldefense.com/v3/*  *://urldefense.proofpoint.com/v2/*`) {
		p, err := matchpattern.Parse(s, false)
		if err != nil {
			panic(err)
		}
		_, h := p.Host()
		m[h] = append(m[h], p)
	}
	return m
}()
