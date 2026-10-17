package linkclean

import (
	"cmp"
	"encoding/json"
	"io/fs"
	"maps"
	"slices"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/internal/query"
)

// The base-relative gates of §11.3 (UM-R4-2): the Counts relative floor and
// debounce include-drop limit, Risky and Golden (c). Base data is decoded
// leniently at the raw-JSON level, as `rulegen summary` does.

// scopePair is a (scope, canon(param)) pair of a Brave strip file: scope is
// "global" for an include whose host is "*", else the include pattern.
type scopePair struct{ scope, param string }

func pairScope(include string) string {
	if isGlobalPattern(include) {
		return "global"
	}
	return include
}

// fileCounts are a strip file's counts as the lock defines them: params is
// the sum of len(params) over all elements, global the same sum over
// elements with an include whose host is "*".
type fileCounts struct{ rules, params, global int }

// dataCounts are the counts of one tree's Brave data.
type dataCounts struct {
	files [2]fileCounts // clean-urls, query-filter
	pairs map[scopePair]bool
	union struct{ params, global, scoped int } // distinct pairs of both files
	// debounce: elements and include instances.
	debounce struct{ rules, includes int }
}

// lenientElems decodes a Brave file as an array of objects, keeping what has
// the expected shape. Anything else counts as nothing: rules-schema reports
// it.
func lenientElems(data []byte) []map[string]any {
	var raw []any
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	var out []map[string]any
	for _, v := range raw {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func lenientStrings(v any) []string {
	a, _ := v.([]any)
	var out []string
	for _, e := range a {
		if s, ok := e.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// countData counts the Brave data of fsys (rules/brave/*.json).
func countData(fsys fs.FS) dataCounts {
	var c dataCounts
	c.pairs = map[scopePair]bool{}
	for i, p := range lockedPaths[:2] {
		data, _ := fs.ReadFile(fsys, p.vendored)
		for _, m := range lenientElems(data) {
			inc, params := lenientStrings(m["include"]), lenientStrings(m["params"])
			c.files[i].rules++
			c.files[i].params += len(params)
			if slices.ContainsFunc(inc, isGlobalPattern) {
				c.files[i].global += len(params)
			}
			for _, in := range inc {
				for _, pr := range params {
					c.pairs[scopePair{pairScope(in), query.Canon(pr)}] = true
				}
			}
		}
	}
	for p := range c.pairs {
		c.union.params++
		if p.scope == "global" {
			c.union.global++
		} else {
			c.union.scoped++
		}
	}
	data, _ := fs.ReadFile(fsys, lockedPaths[2].vendored)
	for _, m := range lenientElems(data) {
		c.debounce.rules++
		c.debounce.includes += len(lenientStrings(m["include"]))
	}
	return c
}

// dropsMore reports whether head is more than pct percent below base.
func dropsMore(base, head, pct int) bool { return head*100 < base*(100-pct) }

func dropPct(base, head int) float64 { return 100 * float64(base-head) / float64(max(base, 1)) }

// metricAcked reports whether a counts or golden ack accepts value for
// metric: unexpired, bound to bindFile's sha256 (or, with "", to any Brave
// file the PR changes), and value >= its min.
func (e *gateEnv) metricAcked(t testing.TB, check, metric string, value int, bindFile string) bool {
	t.Helper()
	_, ok := e.ackFor(t, func(a *ackEntry) bool {
		return a.Check == check && a.Metric == metric && a.Min != nil && value >= *a.Min
	}, bindFile)
	return ok
}

// checkCountsRelative: the union params, global and scoped counted
// separately, and the debounce include instances, must not drop by more than
// 15% vs base unless acked (§11.3, §4.5.3). Params that moved between the
// two files are not removals: the union counts distinct pairs.
func checkCountsRelative(t testing.TB, e *gateEnv, h dataCounts) {
	t.Helper()
	if skipNoBase(t, e, partCountsRelative) {
		skipNoBase(t, e, partDebounceDrop)
		return
	}
	b := countData(e.base)
	for _, m := range []struct {
		metric     string
		base, head int
	}{
		{"union.params", b.union.params, h.union.params},
		{"union.global_params", b.union.global, h.union.global},
		{"union.scoped_params", b.union.scoped, h.union.scoped},
	} {
		if dropsMore(m.base, m.head, 15) && !e.metricAcked(t, "counts", m.metric, m.head, "") {
			t.Errorf("%s: %s %d → %d (-%.1f%%), more than 15%% below base; needs an unexpired counts ack bound to the new bytes",
				partCountsRelative, m.metric, m.base, m.head, dropPct(m.base, m.head))
		}
	}
	if bi, hi := b.debounce.includes, h.debounce.includes; dropsMore(bi, hi, 15) &&
		!e.metricAcked(t, "counts", "debounce.includes", hi, lockedPaths[2].vendored) {
		t.Errorf("%s: debounce.includes %d → %d (-%.1f%%), more than 15%% below base; needs an unexpired counts ack bound to debounce.json",
			partDebounceDrop, bi, hi, dropPct(bi, hi))
	}
}

// riskyNames maps every risky name (§11.3) to where it comes from: the
// built-in never-strip list, testdata/golden/risky-params.txt, and every
// functional_params name of the eval corpus.
func riskyNames(t testing.TB, e *gateEnv) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, it := range readCorpusFS(t, e.tree) {
		for _, n := range it.Functional {
			out[query.Canon(n)] = "corpus functional_params"
		}
	}
	for _, l := range readLinesFS(t, e.tree, "testdata/golden/risky-params.txt") {
		out[query.Canon(l)] = "risky-params.txt"
	}
	for _, n := range neverStrip {
		out[n] = "never-strip"
	}
	return out
}

// checkRisky is rules-risky (§11.3): a (scope, canon(param)) pair new
// relative to base whose param is a risky name and whose scope is global
// fails unless acked. A scoped risky pair passes, with a note (⚠ in the
// summary; the routine escalates it).
func checkRisky(t testing.TB, e *gateEnv) {
	t.Helper()
	if skipNoBase(t, e, partRisky) {
		return
	}
	head, base := countData(e.data).pairs, countData(e.base).pairs
	names := riskyNames(t, e)
	var added []scopePair
	for p := range head {
		if !base[p] {
			added = append(added, p)
		}
	}
	slices.SortFunc(added, func(a, b scopePair) int { return cmp.Or(cmp.Compare(a.scope, b.scope), cmp.Compare(a.param, b.param)) })
	for _, p := range added {
		src, risky := names[p.param]
		switch {
		case !risky:
		case p.scope != "global":
			t.Logf("note: ⚠ new scoped pair (%s, %s): %s is a risky name (%s)", p.scope, p.param, p.param, src)
		default:
			if a, ok := e.ackFor(t, func(a *ackEntry) bool {
				return a.Check == "risky" && a.Scope == p.scope && query.Canon(a.Param) == p.param
			}, ""); ok {
				t.Logf("note: new global pair (global, %s) acked by %s: %s", p.param, a.By, a.Reason)
				continue
			}
			t.Errorf("Risky: new global pair (global, %s): %s is a risky name (%s); needs an unexpired risky ack bound to the new bytes", p.param, p.param, src)
		}
	}
}

// checkCorpusDelta is rules-golden (c) (§11.3): the corpus's exact-or-alt
// count must not drop below base, nor tracker recall by more than 1 point,
// unless acked. Both sides run the head engine, over base's and head's data.
func checkCorpusDelta(t testing.TB, e *gateEnv) {
	t.Helper()
	if skipNoBase(t, e, partGoldenC) {
		return
	}
	baseSrcs, err := sourcesFrom(e.base)
	if err != nil {
		t.Fatalf("%s: base rule data does not compile: %v", partGoldenC, err)
	}
	baseC, err := New(Config{Sources: baseSrcs})
	if err != nil {
		t.Fatalf("%s: base rule data does not compile: %v", partGoldenC, err)
	}
	srcs, key := e.cleanerSources(t)
	items := readCorpusFS(t, e.tree)
	b := evalCorpus(baseC, items)
	h := evalCorpus(cachedCleaner(t, key+"|corpus", Config{Sources: srcs}), items)
	if h.Exact < b.Exact && !e.metricAcked(t, "golden", "corpus.exact", h.Exact, "") {
		t.Errorf("%s: corpus exact-or-alt %d → %d, below base; needs an unexpired golden ack (corpus.exact)", partGoldenC, b.Exact, h.Exact)
		for _, r := range h.Results {
			if i := slices.IndexFunc(b.Results, func(x corpusResult) bool { return x.Item.ID == r.Item.ID }); i >= 0 && b.Results[i].Exact && !r.Exact {
				t.Logf("  %s: %s\n    base %s\n    head %s", r.Item.ID, r.Item.Input, b.Results[i].Out, r.Out)
			}
		}
	}
	if b.recall()-h.recall() > 1 && !e.metricAcked(t, "golden", "corpus.recall_removed", h.Removed, "") {
		t.Errorf("%s: tracker recall %.1f%% → %.1f%%, down more than 1 point; needs an unexpired golden ack (corpus.recall_removed)", partGoldenC, b.recall(), h.recall())
	}
	t.Logf("corpus: exact-or-alt %d → %d; tracker recall %.1f%% → %.1f%%", b.Exact, h.Exact, b.recall(), h.recall())
}

// sortedPairs is for messages and the summary data.
func sortedPairs(m map[scopePair]bool) []scopePair {
	return slices.SortedFunc(maps.Keys(m), func(a, b scopePair) int {
		return cmp.Or(cmp.Compare(a.scope, b.scope), cmp.Compare(a.param, b.param))
	})
}
