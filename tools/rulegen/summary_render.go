package main

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
)

// sumMaxRows caps every list, so a large upstream change still fits in a
// PR comment.
const sumMaxRows = 150

// sumWriter accumulates Markdown lines.
type sumWriter struct{ b strings.Builder }

func (w *sumWriter) f(format string, args ...any) {
	fmt.Fprintf(&w.b, format, args...)
	w.b.WriteByte('\n')
}

func (w *sumWriter) blank() { w.b.WriteByte('\n') }

func (w *sumWriter) h(title string) { w.f("\n### %s\n", title) }

// list writes items as a bulleted list, capped at sumMaxRows, or none when
// there are no items.
func (w *sumWriter) list(items []string, none string) {
	if len(items) == 0 {
		if none != "" {
			w.f("%s", none)
		}
		return
	}
	for i, it := range items {
		if i == sumMaxRows {
			w.f("- … and %d more", len(items)-i)
			break
		}
		w.f("- %s", it)
	}
}

// sumCode renders s as inline code, safe inside a table cell.
func sumCode(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	if strings.Contains(s, "`") {
		return "`` " + s + " ``"
	}
	return "`" + s + "`"
}

func (s *summary) render(ctx context.Context) []byte {
	w := &sumWriter{}
	w.f("%s", data.SummaryMarker)
	w.f("## Rule data summary")
	s.writeHeader(ctx, w)
	s.writeCounts(w)
	s.writePairs(w)
	s.writeRuleIDs(w)
	s.writeHygiene(w)
	s.writeAcks(w)
	w.h("Watch")
	for _, l := range s.watch {
		w.f("%s", l)
	}
	s.writeDebounce(w)
	s.writeBehaviour(w)
	if s.preflight != nil {
		w.h("Preflight")
		for _, l := range s.preflight {
			w.f("%s", l)
		}
	}
	return []byte(strings.TrimRight(w.b.String(), "\n") + "\n")
}

// sumDelta renders "a → b (±x.x%)", or "a" when unchanged.
func sumDelta(a, b int) string {
	switch {
	case a == b:
		return fmt.Sprint(a)
	case a == 0:
		return fmt.Sprintf("%d → %d (new)", a, b)
	}
	return fmt.Sprintf("%d → %d (%+.1f%%)", a, b, 100*float64(b-a)/float64(a))
}

func (s *summary) writeCounts(w *sumWriter) {
	w.h("Counts")
	w.f("| | rules | params | global params | scoped params |")
	w.f("|---|---|---|---|---|")
	names := [2]string{"clean-urls", "query-filter"}
	for i := range 2 {
		b, h := s.base.strip[i].counts, s.head.strip[i].counts
		w.f("| %s | %s | %s | %s | %s |", names[i], sumDelta(b.Rules, h.Rules), sumDelta(b.Params, h.Params),
			sumDelta(b.GlobalParams, h.GlobalParams), sumDelta(b.Params-b.GlobalParams, h.Params-h.GlobalParams))
	}
	split := func(u map[sumPair]bool) (all, global int) {
		for p := range u {
			all++
			if p.scope == "global" {
				global++
			}
		}
		return all, global
	}
	ba, bg := split(s.base.union())
	ha, hg := split(s.head.union())
	w.f("| union (distinct pairs) | | %s | %s | %s |", sumDelta(ba, ha), sumDelta(bg, hg), sumDelta(ba-bg, ha-hg))
	inc := func(rs []sumDebRule) (n int) {
		for _, r := range rs {
			n += len(r.include)
		}
		return n
	}
	w.blank()
	w.f("Debounce: rules %s, include instances %s.", sumDelta(len(s.base.debounce), len(s.head.debounce)), sumDelta(inc(s.base.debounce), inc(s.head.debounce)))

	var moved []string
	for _, m := range s.moves() {
		moved = append(moved, fmt.Sprintf("(%s, %s): %s → %s", sumCode(m.pair.scope), sumCode(m.pair.param), m.from, m.to))
	}
	if len(moved) > 0 {
		w.blank()
		w.f("Params moved between the two files (not removals):")
		w.blank()
		w.list(moved, "")
	}
}

type sumMove struct {
	pair     sumPair
	from, to string
}

// moves lists the pairs that left one strip file and arrived in the other.
func (s *summary) moves() []sumMove {
	names := [2]string{"clean-urls", "query-filter"}
	var out []sumMove
	for i := range 2 {
		j := 1 - i
		for p := range s.base.strip[i].pairs {
			if !s.head.strip[i].pairs[p] && s.head.strip[j].pairs[p] && !s.base.strip[j].pairs[p] {
				out = append(out, sumMove{p, names[i], names[j]})
			}
		}
	}
	slices.SortFunc(out, func(a, b sumMove) int { return sumComparePairs(a.pair, b.pair) })
	return out
}

func (s *summary) writePairs(w *sumWriter) {
	w.h("Pairs")
	risky, notes := sumRiskyNames(s.head.fsys)
	for _, n := range notes {
		w.f("note: risky names: %s", n)
	}
	bu, hu := s.base.union(), s.head.union()
	diff := func(a, b map[sumPair]bool) []sumPair {
		var out []sumPair
		for p := range a {
			if !b[p] {
				out = append(out, p)
			}
		}
		slices.SortFunc(out, sumComparePairs)
		return out
	}
	row := func(p sumPair) string {
		var flags, why []string
		if src, ok := risky[p.param]; ok {
			flags = append(flags, data.FlagRisky)
			why = append(why, "risky: "+src)
		}
		if s.engine != nil && s.engine.CorpusScopes[p.scope] {
			flags = append(flags, data.FlagCorpus)
		}
		if sumAffiliateName(p.param) {
			flags = append(flags, data.FlagAffiliate)
			why = append(why, "affiliate-class candidate")
		}
		r := fmt.Sprintf("%s (%s, %s)", strings.Join(flags, ""), sumCode(p.scope), sumCode(p.param))
		if len(why) > 0 {
			r += " — " + strings.Join(why, "; ")
		}
		return strings.TrimSpace(r)
	}
	w.f("%s risky name, %s corpus host, %s affiliate-class candidate.", data.FlagRisky, data.FlagCorpus, data.FlagAffiliate)
	if s.engine == nil {
		w.f("note: %s needs the engine data, which is unavailable (see Behaviour diff).", data.FlagCorpus)
	}
	for _, part := range []struct {
		title string
		pairs []sumPair
	}{{"Added", diff(hu, bu)}, {"Removed", diff(bu, hu)}} {
		w.blank()
		w.f("%s (%d):", part.title, len(part.pairs))
		w.blank()
		var items []string
		for _, p := range part.pairs {
			items = append(items, row(p))
		}
		w.list(items, "none")
	}
}

func (s *summary) writeRuleIDs(w *sumWriter) {
	w.h("Rule IDs")
	d := s.engine
	if d == nil {
		w.f("note: unavailable without the engine data (see Behaviour diff).")
		return
	}
	if d.BaseError != "" {
		w.f("note: base data does not compile: %s", d.BaseError)
	}
	if d.HeadError != "" {
		w.f("note: head data does not compile: %s", d.HeadError)
	}
	if d.BaseError != "" || d.HeadError != "" {
		return
	}
	describe := func(ids []string, groups map[string][]string) []string {
		var out []string
		for _, id := range ids {
			if g, ok := groups[id]; ok {
				var ps []string
				for _, p := range g {
					ps = append(ps, sumCode(p))
				}
				out = append(out, sumCode(id)+" (patterns "+strings.Join(ps, ", ")+")")
			} else {
				out = append(out, sumCode(id))
			}
		}
		return out
	}
	added := sumMinus(d.HeadRuleIDs, d.BaseRuleIDs)
	removed := sumMinus(d.BaseRuleIDs, d.HeadRuleIDs)
	w.f("Added (%d):", len(added))
	w.blank()
	w.list(describe(added, d.HeadDebounceGroups), "none")
	w.blank()
	w.f("Removed (%d):", len(removed))
	w.blank()
	w.list(describe(removed, d.BaseDebounceGroups), "none")
	if len(removed) > 0 {
		w.blank()
		w.f("**%s**: a Config.DisableRules entry naming a removed ID no longer matches a rule.", data.SumDisableRules)
	}
}

// sumMinus is a − b, in a's order.
func sumMinus(a, b []string) []string {
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	var out []string
	for _, x := range a {
		if !in[x] {
			out = append(out, x)
		}
	}
	return out
}

func (s *summary) writeHygiene(w *sumWriter) {
	w.h("Overlay hygiene")
	if s.engine == nil {
		w.f("note: unavailable without the engine data (see Behaviour diff).")
		return
	}
	var items []string
	for _, n := range s.engine.Hygiene {
		var t string
		switch n.Kind {
		case data.HygieneStaleKeep:
			t = fmt.Sprintf("stale keep-override: %s keeps %s, which no Brave rule strips in its scope any more", sumCode(n.RuleID), sumCode(n.Param))
		case data.HygieneDuplicated:
			t = fmt.Sprintf("duplicated upstream: overlay strip %s of %s is now stripped in every scope by %s", sumCode(n.RuleID), sumCode(n.Param), sumCode(n.With))
		case data.HygieneOverlap:
			t = fmt.Sprintf("overlap: overlay unwrap %s scope %s is also covered by %s", sumCode(n.RuleID), sumCode(n.Scope), sumCode(n.With))
		default:
			t = fmt.Sprintf("%s: %s", n.Kind, sumCode(n.RuleID))
		}
		if n.AtBase {
			t += " (also at base)"
		} else {
			t = "**new** " + t
		}
		items = append(items, t)
	}
	w.list(items, "none")
}

func (s *summary) writeAcks(w *sumWriter) {
	w.h("Acks")
	b := s.head.read(data.AcksPath)
	if b == nil {
		w.f("note: %s is missing", data.AcksPath)
		return
	}
	a, errs := data.ParseAcks(b)
	for _, err := range errs {
		w.f("- note: %v", err)
	}
	heads := map[string]string{} // sha256 → upstream path
	for _, spec := range data.Files[:3] {
		if sum := s.head.sha256Of(spec.Upstream); sum != "" {
			heads[sum] = spec.Upstream
		}
	}
	var items []string
	for i, k := range a.Acks {
		subject := k.Metric
		if k.Check == "risky" {
			subject = k.Scope + " " + k.Param
		}
		what := fmt.Sprintf("acks[%d] %s %s", i, k.Check, sumCode(subject))
		bound := heads[k.VendoredSHA256] != ""
		if f, ok := sumAckFile(k.Metric); ok {
			bound = s.head.sha256Of(f) == k.VendoredSHA256
		}
		if !bound {
			items = append(items, fmt.Sprintf(data.SumAckLapsed+": %s (%s)", k.VendoredSHA256, what, k.Reason))
		}
		if data.Expired(k.Expires, s.now) {
			items = append(items, fmt.Sprintf("%s (%s): %s (%s)", data.SumAckExpired, k.Expires, what, k.Reason))
		}
	}
	for i, h := range a.Holds {
		if data.Expired(h.Expires, s.now) {
			items = append(items, fmt.Sprintf("hold expired (%s): holds[%d] %s %s (%s)", h.Expires, i, sumCode(h.File), sumCode(sumShort(h.SHA256)), h.Reason))
		}
	}
	w.list(items, "No lapsed or expired entries.")
}

// sumAckFile is the upstream file a per-file counts metric is bound to.
func sumAckFile(metric string) (string, bool) {
	prefix, _, _ := strings.Cut(metric, ".")
	switch prefix {
	case "clean-urls":
		return data.Files[0].Upstream, true
	case "query-filter":
		return data.Files[1].Upstream, true
	case "debounce":
		return data.Files[2].Upstream, true
	}
	return "", false
}

func (s *summary) writeBehaviour(w *sumWriter) {
	w.h("Behaviour diff")
	d := s.engine
	if d == nil {
		w.f("note: engine data unavailable; the corpus behaviour diff, %s, rule IDs and overlay hygiene are omitted.", data.FlagCorpus)
		w.blank()
		w.f("<details><summary>why</summary>")
		w.blank()
		w.f("```")
		w.f("%s", s.engineNote)
		w.f("```")
		w.blank()
		w.f("</details>")
		return
	}
	if d.BaseError != "" || d.HeadError != "" {
		w.f("note: a side does not compile (see Rule IDs); outputs on that side are empty.")
	}
	exact := func(c data.CorpusRow, out string) bool {
		return out == c.Expected || slices.Contains(c.Alt, out)
	}
	var rows []string
	for _, c := range d.Corpus {
		if c.Base == c.Head {
			continue
		}
		mark := "changed"
		switch {
		case c.BrokenHead && !c.BrokenBase:
			mark = "**broke**"
		case c.BrokenBase && !c.BrokenHead, exact(c, c.Head) && !exact(c, c.Base):
			mark = "improved"
		}
		rows = append(rows, fmt.Sprintf("| %s | %s | %s | %s | %s |", sumCode(c.ID), mark, sumCode(c.Input), sumCode(c.Base), sumCode(c.Head)))
	}
	if len(rows) == 0 {
		w.f("No corpus output changed (%d items).", len(d.Corpus))
		return
	}
	w.f("%d of %d corpus items changed:", len(rows), len(d.Corpus))
	w.blank()
	w.f("| Item | | Input | Base | Head |")
	w.f("|---|---|---|---|---|")
	for i, r := range rows {
		if i == sumMaxRows {
			w.f("| … | %d more | | | |", len(rows)-i)
			break
		}
		w.f("%s", r)
	}
}

// sumSortedKeys returns m's keys in order.
func sumSortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
