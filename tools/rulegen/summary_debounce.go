package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
)

// Debounce rules have no identity upstream, so base and head rules are
// paired: by index when they share an include, then by the most shared
// includes. An unpaired head rule is new; an unpaired base rule is gone.

// sumDebPair is a head rule and the base rule it continues (nil for a new
// rule), or a base rule with no head rule (head nil).
type sumDebPair struct{ base, head *sumDebRule }

func sumPairDebounce(base, head []sumDebRule) []sumDebPair {
	shared := func(a, b *sumDebRule) int {
		n := 0
		for _, p := range a.include {
			if slices.Contains(b.include, p) {
				n++
			}
		}
		return n
	}
	used := make([]bool, len(base))
	match := make([]int, len(head))
	for j := range head {
		match[j] = -1
		if j < len(base) && shared(&base[j], &head[j]) > 0 {
			match[j], used[j] = j, true
		}
	}
	for j := range head {
		if match[j] >= 0 {
			continue
		}
		best, bestN := -1, 0
		for i := range base {
			if n := shared(&base[i], &head[j]); !used[i] && n > bestN {
				best, bestN = i, n
			}
		}
		if best >= 0 {
			match[j], used[best] = best, true
		}
	}
	var out []sumDebPair
	for j := range head {
		p := sumDebPair{head: &head[j]}
		if match[j] >= 0 {
			p.base = &base[match[j]]
		}
		out = append(out, p)
	}
	for i := range base {
		if !used[i] {
			out = append(out, sumDebPair{base: &base[i]})
		}
	}
	return out
}

// classesOf is the set of classes a rule's includes have in classes.
func classesOf(r *sumDebRule, classes map[string]sumClass) []string {
	var out []string
	for _, p := range r.include {
		if c, ok := classes[p]; ok && c.Class != "" && !slices.Contains(out, c.Class) {
			out = append(out, c.Class)
		}
	}
	slices.Sort(out)
	return out
}

// hasCompiledPatterns reports whether the rule compiles and classes one of
// its includes affiliate or unwrap.
func hasCompiledPatterns(r *sumDebRule, classes map[string]sumClass) bool {
	return r != nil && r.compiled() && slices.ContainsFunc(r.include, func(p string) bool {
		c := classes[p].Class
		return c == "affiliate" || c == "unwrap"
	})
}

func sumRuleLabel(r *sumDebRule) string {
	return fmt.Sprintf("rule %d (%s, %s)", r.index, sumCode(r.action), sumCode(r.param))
}

func (s *summary) writeDebounce(w *sumWriter) {
	w.h("Debounce")
	pairs := sumPairDebounce(s.base.debounce, s.head.debounce)
	var changes, notes []string
	codes := func(ps []string) string {
		var out []string
		for _, p := range ps {
			out = append(out, sumCode(p))
		}
		return strings.Join(out, ", ")
	}
	for _, p := range pairs {
		switch {
		case p.base == nil:
			changes = append(changes, fmt.Sprintf("new %s: added %s", sumRuleLabel(p.head), codes(p.head.include)))
			continue
		case p.head == nil:
			changes = append(changes, fmt.Sprintf("removed base %s: removed %s", sumRuleLabel(p.base), codes(p.base.include)))
			continue
		}
		added := sumMinus(p.head.include, p.base.include)
		removed := sumMinus(p.base.include, p.head.include)
		var parts []string
		if len(added) > 0 {
			parts = append(parts, "added "+codes(added))
		}
		if len(removed) > 0 {
			parts = append(parts, "removed "+codes(removed))
		}
		label := sumRuleLabel(p.head)
		if p.base.index != p.head.index {
			label += fmt.Sprintf(" (base rule %d)", p.base.index)
		}
		if len(parts) > 0 {
			changes = append(changes, label+": "+strings.Join(parts, "; "))
		}
		var fields []string
		for _, f := range []struct{ name, b, h string }{
			{"action", p.base.action, p.head.action},
			{"param", p.base.param, p.head.param},
			{"prepend_scheme", p.base.scheme, p.head.scheme},
		} {
			if f.b != f.h {
				fields = append(fields, fmt.Sprintf("%s %s → %s", f.name, sumCode(f.b), sumCode(f.h)))
			}
		}
		if len(fields) > 0 && (hasCompiledPatterns(p.base, s.base.classes) || hasCompiledPatterns(p.head, s.head.classes)) {
			notes = append(notes, fmt.Sprintf("%s has compiled patterns and changed %s; check the golden rows", label, strings.Join(fields, ", ")))
		}
		if hc := classesOf(p.head, s.head.classes); len(hc) > 1 && len(classesOf(p.base, s.base.classes)) <= 1 {
			notes = append(notes, fmt.Sprintf("%s: %s (%s)", label, data.SumMixesClasses, strings.Join(hc, ", ")))
		}
	}
	w.f("Include changes:")
	w.blank()
	w.list(changes, "none")

	// Stubs, stale classifications and exact-path hygiene.
	inHead := map[string]bool{}
	for _, r := range s.head.debounce {
		for _, p := range r.include {
			inHead[p] = true
		}
	}
	var stubs, stubDescs []string
	seen := map[string]bool{}
	for i := range s.head.debounce {
		for _, p := range s.head.debounce[i].include {
			if seen[p] {
				continue
			}
			seen[p] = true
			c, classed := s.head.classes[p]
			switch {
			case !classed:
				e, d := s.stub(p, "unclassified")
				stubs, stubDescs = append(stubs, e), append(stubDescs, d)
			case c.Class != "off" && !s.compiledSomewhere(p):
				e, d := s.stub(p, fmt.Sprintf("classed %s, but no rule holding it compiles in v1", c.Class))
				stubs, stubDescs = append(stubs, e), append(stubDescs, d)
			}
		}
	}
	var stale []string
	for _, p := range sumSortedKeys(s.head.classes) {
		if !inHead[p] {
			stale = append(stale, fmt.Sprintf("stale classification: %s (%s) is no longer in any upstream rule", sumCode(p), s.head.classes[p].Class))
		}
	}
	notes = append(notes, stale...)
	baseIncludes := map[string]bool{}
	for _, r := range s.base.debounce {
		for _, p := range r.include {
			baseIncludes[p] = true
		}
	}
	seen = map[string]bool{}
	for _, r := range s.head.debounce {
		for _, p := range r.include {
			if seen[p] || strings.Contains(sumPatternPath(p), "*") {
				continue
			}
			seen[p] = true
			n := fmt.Sprintf("%s: %s (rule %d)", sumCode(p), data.SumExactPath, r.index)
			if !baseIncludes[p] {
				n = "**new** " + n
			}
			notes = append(notes, n)
		}
	}
	w.blank()
	w.f("Notes:")
	w.blank()
	w.list(notes, "none")
	if len(stubs) > 0 {
		w.blank()
		w.f("Classification stubs for %s (guessed; a human decides, and rules-compile stays red until an entry is committed):", data.ClassesPath)
		w.blank()
		w.list(stubDescs, "")
		w.blank()
		w.f("```json")
		w.f("%s", strings.Join(stubs, ",\n"))
		w.f("```")
	}
}

// compiledSomewhere reports whether a compiled head rule holds pattern p.
func (s *summary) compiledSomewhere(p string) bool {
	return slices.ContainsFunc(s.head.debounce, func(r sumDebRule) bool { return r.compiled() && slices.Contains(r.include, p) })
}

// stub returns a ready-to-paste debounce-classes.json entry for p with a
// guessed class, and a line explaining it. The guess is the first match: off
// when no rule holding p compiles; affiliate when the host is a known network
// domain, the host or the rule's param matches the affiliate heuristic, or
// the rule already holds affiliate-classed patterns; otherwise unwrap.
func (s *summary) stub(p, why string) (entry, desc string) {
	var rule *sumDebRule
	for i := range s.head.debounce {
		r := &s.head.debounce[i]
		if slices.Contains(r.include, p) && (rule == nil || r.compiled() && !rule.compiled()) {
			rule = r
		}
	}
	e := map[string]string{}
	host := sumPatternHost(p)
	network, known := s.knownNetwork(host)
	var reason string
	switch {
	case !rule.compiled():
		e["class"], e["why"] = "off", rule.uncompiledWhy()
		reason = "rule " + fmt.Sprint(rule.index) + " is not compiled in v1"
	case known:
		e["class"], e["network"] = "affiliate", network
		reason = host + " is a known " + network + " domain"
	case sumAffiliateHost(host) || sumAffiliateName(data.Canon(rule.param)):
		e["class"], e["network"] = "affiliate", "<network>"
		reason = "the host or the param matches the affiliate heuristic"
	case slices.Contains(classesOf(rule, s.head.classes), "affiliate"):
		e["class"], e["network"] = "affiliate", "<network>"
		reason = fmt.Sprintf("rule %d already holds affiliate-classed patterns", rule.index)
	default:
		e["class"] = "unwrap"
		reason = "no affiliate signal"
	}
	flag := ""
	if e["class"] == "affiliate" {
		flag = " " + data.FlagAffiliate
		// Needs a locator (idParam or idPath, with a ref) or a why.
		e["why"] = "<idParam/idPath with a ref, or why there is no single beneficiary ID>"
	}
	var fields []string
	for _, k := range []string{"class", "network", "why"} {
		if v, ok := e[k]; ok {
			fields = append(fields, sumJSON(k)+": "+sumJSON(v))
		}
	}
	entry = sumJSON(p) + ": {" + strings.Join(fields, ", ") + "}"
	desc = fmt.Sprintf("%s (%s, rule %d): guessed %s%s, because %s", sumCode(p), why, rule.index, e["class"], flag, reason)
	return entry, desc
}

// knownNetwork reports whether host equals, or is a subdomain of, the host
// of an affiliate-classed pattern, and that pattern's network.
func (s *summary) knownNetwork(host string) (string, bool) {
	hosts := map[string]string{}
	if s.engine != nil {
		hosts = s.engine.AffiliateHosts
	} else {
		for p, c := range s.head.classes {
			if c.Class == "affiliate" {
				hosts[sumPatternHost(p)] = c.Network
			}
		}
	}
	for _, h := range sumSortedKeys(hosts) {
		if h != "" && h != "*" && (host == h || strings.HasSuffix(host, "."+h)) {
			return hosts[h], true
		}
	}
	return "", false
}

// sumJSON encodes s as a JSON string without HTML escaping.
func sumJSON(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}
