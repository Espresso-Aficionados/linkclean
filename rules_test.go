package linkclean

import (
	"bufio"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/internal/brave"
	"github.com/Espresso-Aficionados/linkclean/internal/matchpattern"
	"github.com/Espresso-Aficionados/linkclean/internal/query"
	"github.com/Espresso-Aficionados/linkclean/internal/rawurl"
)

// TestRules holds the M1 part of the §11.3 validation gates. Every
// base-relative part (the relative Counts floor, Risky, Golden (c) and the
// Provenance acks, holds and Upstream checks) needs LINKCLEAN_BASE_DIR and
// lands with the M2 tooling; it is skipped here with a log line.
func TestRules(t *testing.T) {
	if os.Getenv("LINKCLEAN_BASE_DIR") == "" {
		t.Log("skipped (no LINKCLEAN_BASE_DIR): Counts relative floor, Risky, Golden (c), Provenance acks/holds/Upstream")
	}
	t.Run("Provenance", testProvenance)
	t.Run("Schema", testSchema)
	t.Run("Compile", testCompile)
	t.Run("Counts", testCounts)
	t.Run("Golden", func(t *testing.T) {
		t.Run("a", testGoldenRows)
		t.Run("b", testCorpusGate)
	})
}

// gitBlobSHA1 is the git object ID of data as a blob.
func gitBlobSHA1(data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func testProvenance(t *testing.T) {
	lock, err := readLock(rulesFS)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(lock.Upstream.Commit) {
		t.Errorf("lock commit %q is not 40-hex", lock.Upstream.Commit)
	}
	for up, lf := range lock.Upstream.Files {
		var data []byte
		if up == "LICENSE" {
			// Not embedded: read from the working tree.
			data, err = os.ReadFile(lf.Vendored)
			if lf.SPDX != "MPL-2.0" {
				t.Errorf("LICENSE spdx %q, want MPL-2.0", lf.SPDX)
			}
		} else {
			data, err = fs.ReadFile(rulesFS, lf.Vendored)
		}
		if err != nil {
			t.Errorf("%s: %v", up, err)
			continue
		}
		if got := gitBlobSHA1(data); got != lf.GitBlobSHA1 {
			t.Errorf("%s: git blob %s, lock %s", lf.Vendored, got, lf.GitBlobSHA1)
		}
		if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != lf.SHA256 {
			t.Errorf("%s: sha256 %x, lock %s", lf.Vendored, sum, lf.SHA256)
		}
	}
	notice, err := os.ReadFile("rules/brave/NOTICE")
	if err != nil {
		t.Fatal(err)
	}
	want40 := map[string]bool{lock.Upstream.Commit: true}
	want64 := map[string]bool{}
	for _, lf := range lock.Upstream.Files {
		want40[lf.GitBlobSHA1] = true
		want64[lf.SHA256] = true
	}
	got40, got64 := map[string]bool{}, map[string]bool{}
	for _, tok := range regexp.MustCompile(`\b[0-9a-f]{40,64}\b`).FindAllString(string(notice), -1) {
		switch len(tok) {
		case 40:
			got40[tok] = true
		case 64:
			got64[tok] = true
		}
	}
	if fmt.Sprint(got40) != fmt.Sprint(want40) || fmt.Sprint(got64) != fmt.Sprint(want64) {
		t.Errorf("rules/brave/NOTICE names other hashes than the lock:\n  40-hex %v, want %v\n  64-hex %v, want %v", got40, want40, got64, want64)
	}
}

func testSchema(t *testing.T) {
	// The strict decoders of internal/brave and decodeOverlay run inside
	// readDefaults; any schema problem fails here.
	if _, err := readDefaults(rulesFS); err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct {
		path string
		re   *regexp.Regexp
	}{
		{"testdata/keep-coverage-exempt.txt", regexp.MustCompile(`^\S+ \S+ # \S.*$`)},
		{"testdata/corpus/eval-2026-10/known-broken.txt", regexp.MustCompile(`^[a-z]+-\d+ # \S.*$`)},
	} {
		for i, line := range readLines(t, f.path) {
			if !f.re.MatchString(line) {
				t.Errorf("%s:%d: %q does not match %s", f.path, i+1, line, f.re)
			}
		}
	}
}

// readLines returns the non-comment, non-blank lines of a human-owned list.
func readLines(t testing.TB, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// neverStrip is the built-in never-strip list of §11.3.
var neverStrip = strings.Fields(`id q query s t p page v u url lang hl start sort list index size type format ref tab
	token k variant sku language code state st key nonce redirect next return i f c g in`)

func testCompile(t *testing.T) {
	srcs := DefaultSources()
	c, err := New(Config{Sources: srcs})
	if err != nil {
		t.Fatal(err)
	}
	// Nothing dropped: every rule in is a rule out.
	for i, si := range c.Sources() {
		if si.Rules != len(srcs[i].Rules) {
			t.Errorf("%s: %d rules compiled, %d in", si.Name, si.Rules, len(srcs[i].Rules))
		}
	}
	// Debounce accounting (§4.5.3): compiled + skipped instances = upstream.
	deb, _ := fs.ReadFile(rulesFS, "rules/brave/debounce.json")
	cls, _ := fs.ReadFile(rulesFS, classesPath)
	_, st, err := brave.CompileDebounce(deb, cls)
	if err != nil {
		t.Fatal(err)
	}
	if st.CompiledInstances+st.SkippedInstances != st.Instances || st.CompiledRules != len(srcs[2].Rules) {
		t.Errorf("debounce accounting: %+v, %d rules", st, len(srcs[2].Rules))
	}
	if len(st.Stale) > 0 {
		t.Logf("note: stale classifications %v", st.Stale)
	}

	functional := corpusFunctionalNames(t)
	golden := goldenRuleNames(t)
	for _, s := range srcs {
		overlay := s.Name == overlaySet
		for _, r := range s.Rules {
			id := s.Name + ":" + r.ID
			// Affiliate rule data (MA-3).
			if r.Affiliate && r.Network == "" {
				t.Errorf("%s: built-in affiliate rule without network", id)
			}
			if !overlay {
				continue
			}
			for _, p := range slices.Concat(r.Include, r.Exclude) {
				_, rest, _ := strings.Cut(p, "://")
				if i := strings.IndexByte(rest, '/'); i >= 0 && strings.Contains(rest[i:len(rest)-1], "*") {
					t.Errorf("%s: path glob of %q has a '*' before its last character (§4.2)", id, p)
				}
			}
			for _, re := range r.URLRegexps {
				if strings.Contains(re, "://") {
					t.Errorf("%s: URLRegexps %q mentions a host", id, re)
				}
			}
			if !golden[id] {
				t.Errorf("%s: no golden row names it", id)
			}
			if !isGlobal(r) || r.Action != Strip {
				continue
			}
			for _, pr := range r.ParamRegexps {
				re := regexp.MustCompile("^(?:" + pr + ")$")
				for _, n := range neverStrip {
					if re.MatchString(n) {
						t.Errorf("%s: global paramRegexp %q matches never-strip name %q", id, pr, n)
					}
				}
				for n := range functional {
					if re.MatchString(n) {
						t.Errorf("%s: global paramRegexp %q matches corpus functional name %q", id, pr, n)
					}
				}
			}
			for _, p := range r.Params {
				if slices.Contains(neverStrip, query.Canon(p)) {
					t.Errorf("%s: global param %q is a never-strip name", id, p)
				}
			}
		}
	}
	testKeepCoverage(t, srcs)
}

func isGlobal(r Rule) bool {
	for _, p := range r.Include {
		if _, rest, _ := strings.Cut(p, "://"); strings.HasPrefix(rest, "*/") {
			return true
		}
	}
	return false
}

// corpusFunctionalNames returns the canonical functional_params names of the
// corpus, except those of items the signed or archive guard protects: there
// the guard, not a functional meaning, keeps the name (negatives-004's S3
// SigV4 and negatives-046's Wayback utm_source). Probing them would fail M-9
// and F-1, which §11.3 says the probe covers.
func corpusFunctionalNames(t testing.TB) map[string]bool {
	out := map[string]bool{}
	guarded := MustNew(Config{Sources: []RuleSet{}})
	for _, it := range readCorpus(t) {
		if r := guarded.Explain(it.Input); r.Signed || r.Skipped == SkipArchive {
			continue
		}
		for _, n := range it.Functional {
			out[query.Canon(n)] = true
		}
	}
	return out
}

// goldenRuleNames returns every rule ID named by a rules: line.
func goldenRuleNames(t testing.TB) map[string]bool {
	out := map[string]bool{}
	for _, c := range readCases(t, "testdata/golden/positive.txtar") {
		for _, id := range c.Rules {
			out[id] = true
		}
	}
	return out
}

// testKeepCoverage is the §11.3 keep-coverage probe: every Brave include that
// strips a name an A- or K-entry Keep protects must be covered by such a Keep,
// or exempted in testdata/keep-coverage-exempt.txt.
func testKeepCoverage(t *testing.T, srcs []RuleSet) {
	exempt := map[string]bool{}
	for _, l := range readLines(t, "testdata/keep-coverage-exempt.txt") {
		f := strings.Fields(l)
		exempt[f[0]+" "+f[1]] = true
	}
	used := map[string]bool{}
	var keeps []Rule
	for _, s := range srcs {
		if s.Name != overlaySet {
			continue
		}
		for _, r := range s.Rules {
			if r.Action == Keep && len(r.URLRegexps) == 0 && !isGlobal(r) || r.Action == Keep && r.Affiliate {
				keeps = append(keeps, r)
			}
		}
	}
	protected := map[string][]Rule{}
	for _, k := range keeps {
		for _, p := range k.Params {
			protected[query.Canon(p)] = append(protected[query.Canon(p)], k)
		}
	}
	for _, s := range srcs[:2] {
		for _, r := range s.Rules {
			for _, p := range r.Params {
				ks := protected[query.Canon(p)]
				if ks == nil {
					continue
				}
				for _, inc := range r.Include {
					probe := probeURL(inc)
					if matchesAny(t, r.Exclude, probe, false) {
						continue
					}
					covered := false
					for _, k := range ks {
						// URLRegexps are deliberate narrowings and not probed (§11.3).
						if matchesAny(t, k.Include, probe, true) && !matchesAny(t, k.Exclude, probe, true) {
							covered = true
						}
					}
					key := p + " " + inc
					switch {
					case covered:
					case exempt[key]:
						used[key] = true
					default:
						t.Errorf("keep coverage: %s strips %q at %s (probe %s) and no overlay Keep covers it", s.Name, p, inc, probe)
					}
				}
			}
		}
	}
	for k := range exempt {
		if !used[k] {
			t.Logf("note: keep-coverage exemption %q is unused", k)
		}
	}
}

// probeURL synthesizes a URL from an include pattern (§11.3).
func probeURL(p string) string {
	scheme, rest, _ := strings.Cut(p, "://")
	if scheme == "*" {
		scheme = "https"
	}
	host, path, _ := strings.Cut(rest, "/")
	switch {
	case host == "*":
		host = "www.example.com"
	case strings.HasPrefix(host, "*."):
		host = "www." + host[2:]
	}
	return scheme + "://" + host + "/" + strings.ReplaceAll(path, "*", "")
}

func matchesAny(t testing.TB, patterns []string, raw string, allowLabel bool) bool {
	u, sk := rawurl.Parse(raw)
	if sk != rawurl.OK {
		t.Fatalf("probe %q: %v", raw, sk)
	}
	tg := matchpattern.Target{Scheme: u.Scheme, Host: u.Host, IsIP: u.IsIP, Port: u.EffectivePort(), Site: rawurl.Site(u.Host)}
	tg.SetPath(u.PathForRequest())
	for _, s := range patterns {
		p, err := matchpattern.Parse(s, allowLabel)
		if err != nil {
			t.Fatal(err)
		}
		if p.Match(&tg) {
			return true
		}
	}
	return false
}

func testCounts(t *testing.T) {
	srcs := DefaultSources()
	type cnt struct{ rules, params int }
	count := func(s RuleSet) cnt {
		c := cnt{rules: len(s.Rules)}
		for _, r := range s.Rules {
			c.params += len(r.Params)
		}
		return c
	}
	cu, qf := count(srcs[0]), count(srcs[1])
	if cu.rules < 41 || cu.params < 340 {
		t.Errorf("clean-urls: %d rules / %d params, floors 41 / 340", cu.rules, cu.params)
	}
	if qf.rules < 3 || qf.params < 57 {
		t.Errorf("query-filter: %d rules / %d params, floors 3 / 57", qf.rules, qf.params)
	}
	global := map[string]bool{}
	for _, s := range srcs[:2] {
		for _, r := range s.Rules {
			if isGlobal(r) {
				for _, p := range r.Params {
					global[query.Canon(p)] = true
				}
			}
		}
	}
	if len(global) < 196 {
		t.Errorf("union global params: %d, floor 196", len(global))
	}
	deb, _ := fs.ReadFile(rulesFS, "rules/brave/debounce.json")
	cls, _ := fs.ReadFile(rulesFS, classesPath)
	_, st, err := brave.CompileDebounce(deb, cls)
	if err != nil {
		t.Fatal(err)
	}
	if st.Rules < 38 || st.Instances < 150 {
		t.Errorf("debounce: %d rules / %d include instances, floors 38 / 150", st.Rules, st.Instances)
	}
	t.Logf("clean-urls %d/%d, query-filter %d/%d, union global %d, debounce %d/%d", cu.rules, cu.params, qf.rules, qf.params, len(global), st.Rules, st.Instances)
}

// testGoldenRows is rules-golden (a): every Appendix C row, under its
// policies and flags, plus the invariants per case, and every rules: line is
// decisive (disabling the rule changes the result).
func testGoldenRows(t *testing.T) {
	cases := readCases(t, "testdata/golden/positive.txtar")
	for i := range cases {
		c := &cases[i]
		t.Run(c.ID, func(t *testing.T) {
			checkCase(t, c, nil, "default")
			for _, id := range c.Rules {
				if !decisive(t, c, id) {
					t.Errorf("rules: %s does not change the result when disabled", id)
				}
			}
		})
	}
	data, err := os.ReadFile("testdata/golden/negative.txt")
	if err != nil {
		t.Fatal(err)
	}
	negs, err := parseNegatives(data)
	if err != nil {
		t.Fatal(err)
	}
	keep, strip := Default(), MustNew(Config{Affiliate: AffiliateStrip})
	for _, n := range negs {
		for _, cl := range []*Cleaner{keep, strip} {
			if got := cl.Clean(n.URL); got != n.URL {
				t.Errorf("negative %s: %s\n  -> %s", n.ID, n.URL, got)
			}
			if r := cl.Explain(n.URL); r.URL != n.URL || r.Changed {
				t.Errorf("negative %s: Explain changed it", n.ID)
			}
		}
	}
}

// decisive reports whether disabling rule id changes c's outcome under some
// applicable policy: the URL, abandonment, or a hop or removal's category,
// network or ID, or the order of the removals.
func decisive(t *testing.T, c *goldenCase, id string) bool {
	sig := func(r Result) string {
		s := fmt.Sprint(r.URL, r.UnwrapAbandoned)
		for _, h := range r.Hops {
			s += fmt.Sprint(h.Category, h.Beneficiary)
		}
		for _, m := range r.Removed {
			s += fmt.Sprint(m.Part, m.Name, m.Category, m.Beneficiary)
		}
		return s
	}
	for _, pw := range c.policies() {
		on := caseCleaner(t, c, pw.p, nil, "default")
		cfg := c.Config
		cfg.Affiliate = pw.p
		cfg.DisableRules = append(slices.Clone(cfg.DisableRules), id)
		off, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if len(off.Warnings()) > 0 {
			t.Errorf("rules: %s: %v", id, off.Warnings())
			return true
		}
		if sig(on.Explain(c.In)) != sig(off.Explain(c.In)) {
			return true
		}
	}
	return false
}

// corpusItem is one item of testdata/corpus/eval-2026-10/*.json.
type corpusItem struct {
	ID         string   `json:"id"`
	Input      string   `json:"input"`
	Expected   string   `json:"expected"`
	Alt        []string `json:"alt_expected"`
	Tracking   []string `json:"tracking_params"`
	Functional []string `json:"functional_params"`
}

func readCorpus(t testing.TB) []corpusItem {
	t.Helper()
	files, err := filepath.Glob("testdata/corpus/eval-2026-10/*.json")
	if err != nil {
		t.Fatal(err)
	}
	var out []corpusItem
	for _, f := range files {
		if filepath.Base(f) == "adjudicated.json" {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var items []corpusItem
		if err := json.Unmarshal(data, &items); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out = append(out, items...)
	}
	return out
}

// paramNames returns the canonical names of u's query and fragment segments.
func paramNames(u string) map[string]bool {
	m := map[string]bool{}
	v, sk := rawurl.Parse(u)
	if sk != rawurl.OK {
		return m
	}
	for _, part := range []string{v.Query, v.Fragment} {
		for _, s := range query.Parse(nil, part) {
			if s.Name != "" {
				m[query.Canon(s.Name)] = true
			}
		}
	}
	return m
}

// Corpus exact-or-alt count with the capture-status overlay entries absent
// (design §4.7: 227 instead of 232).
const wantCorpusExact = 227

// testCorpusGate is rules-golden (b) (§11.3): no new broken links.
func testCorpusGate(t *testing.T) {
	known := map[string]bool{}
	for _, l := range readLines(t, "testdata/corpus/eval-2026-10/known-broken.txt") {
		id, _, _ := strings.Cut(l, " ")
		known[id] = true
	}
	c := Default()
	items := readCorpus(t)
	exact, tracked, removed := 0, 0, 0
	for _, it := range items {
		out := c.Clean(it.Input)
		if c.Clean(out) != out {
			t.Errorf("%s: not idempotent: %s", it.ID, out)
		}
		ok := out == it.Expected || slices.Contains(it.Alt, out)
		if ok {
			exact++
		}
		post := it.Input
		if r := c.Explain(it.Input); len(r.Hops) > 0 {
			post = r.Hops[len(r.Hops)-1].To
		}
		pn, on := paramNames(post), paramNames(out)
		broken := false
		if !ok {
			if it.Expected == it.Input && out != it.Input {
				broken = true
			}
			for _, f := range it.Functional {
				if cn := query.Canon(f); pn[cn] && !on[cn] {
					broken = true
				}
			}
		}
		switch {
		case broken && !known[it.ID]:
			t.Errorf("%s broken (not in known-broken.txt):\n  in   %s\n  out  %s\n  want %s", it.ID, it.Input, out, it.Expected)
		case !broken && known[it.ID]:
			t.Logf("stale known-broken entry %s", it.ID)
		}
		for _, n := range it.Tracking {
			if strings.HasPrefix(n, "/") {
				continue
			}
			if cn := query.Canon(n); pn[cn] {
				tracked++
				if !on[cn] {
					removed++
				}
			}
		}
	}
	t.Logf("corpus: exact-or-alt %d/%d; tracker recall %d/%d (%.1f%%)", exact, len(items), removed, tracked, 100*float64(removed)/float64(max(tracked, 1)))
	if exact != wantCorpusExact {
		t.Errorf("exact-or-alt = %d, want %d", exact, wantCorpusExact)
	}
}
