package linkclean

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Espresso-Aficionados/linkclean/internal/brave"
	"github.com/Espresso-Aficionados/linkclean/internal/matchpattern"
	"github.com/Espresso-Aficionados/linkclean/internal/query"
	"github.com/Espresso-Aficionados/linkclean/internal/rawurl"
)

// TestRules runs the §11.3 validation gates over the embedded rule data, so
// what CI validates is what ships. Each subtest is one required check.
//
// The base-relative parts (the relative Counts floor and debounce drop limit,
// Risky, Golden (c), and in Provenance the acks-changed check, the hold match
// and the Upstream checks) compare against LINKCLEAN_BASE_DIR, a directory
// written by `rulegen basedir`. Without it each is skipped with one log line
// naming it, and everything else still runs. The Upstream checks also need
// LINKCLEAN_UPSTREAM_DIR, a clone made by `rulegen upstream`.
func TestRules(t *testing.T) {
	e := realGateEnv(t)
	for _, check := range gateChecks {
		t.Run(check, func(t *testing.T) { checkRules(t, e, check) })
	}
}

// gateChecks are TestRules' subtests, one per required check (§11.3).
var gateChecks = []string{"Provenance", "Schema", "Compile", "Counts", "Risky", "Golden"}

// gateEnv is everything a gate reads (§11.3).
type gateEnv struct {
	data        fs.FS     // the embedded set: rules/brave/*.json, rules/overlay/*.json, rules.lock.json
	tree        fs.FS     // the working tree: rules/acks.json, rules/brave/{LICENSE,NOTICE}, .gitmodules, testdata/**
	base        fs.FS     // LINKCLEAN_BASE_DIR (rules/**, rules.lock.json, .gitmodules?, GITLINK?), or nil
	headGitlink string    // gitlink of tools/upstream/adblock-lists in HEAD (mode 160000), or ""
	upstream    string    // LINKCLEAN_UPSTREAM_DIR, or ""
	now         time.Time // decides ack and hold expiry
	key         string    // cleaner cache key of data; "default" means data is the embedded set

	srcOnce sync.Once
	srcs    []RuleSet
	srcErr  error
}

// realGateEnv is the environment of TestRules.
func realGateEnv(t testing.TB) *gateEnv {
	e := &gateEnv{data: rulesFS, tree: os.DirFS("."), now: time.Now(), key: "default"}
	if dir := os.Getenv("LINKCLEAN_BASE_DIR"); dir != "" {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Fatalf("LINKCLEAN_BASE_DIR %q is not a directory (%v)", dir, err)
		}
		e.base = os.DirFS(dir)
		e.headGitlink = headGitlink(t)
	}
	e.upstream = os.Getenv("LINKCLEAN_UPSTREAM_DIR")
	return e
}

// headGitlink reads the gitlink of the submodule path from HEAD's tree, never
// from a submodule worktree (§11.1 sync step 1).
func headGitlink(t testing.TB) string {
	out, err := exec.Command("git", "ls-tree", "-z", "HEAD", "--", submodulePath).Output()
	if err != nil {
		t.Fatalf("git ls-tree HEAD -- %s: %v", submodulePath, err)
	}
	// "<mode> <type> <object>\t<path>\x00"
	meta, _, ok := strings.Cut(string(out), "\t")
	if f := strings.Fields(meta); ok && len(f) == 3 && f[0] == "160000" {
		return f[2]
	}
	return ""
}

// sources compiles e.data like DefaultSources does.
func (e *gateEnv) sources() ([]RuleSet, error) {
	e.srcOnce.Do(func() { e.srcs, e.srcErr = sourcesFrom(e.data) })
	return e.srcs, e.srcErr
}

// cleanerSources returns the Sources and cache key for Cleaners over e.data:
// nil (the defaults) for the embedded set, so the gates share Cleaners with
// the rest of the package's tests.
func (e *gateEnv) cleanerSources(t testing.TB) ([]RuleSet, string) {
	t.Helper()
	if e.key == "default" {
		return nil, e.key
	}
	srcs, err := e.sources()
	if err != nil {
		t.Fatalf("rule data does not compile: %v", err)
	}
	return srcs, e.key
}

func sourcesFrom(fsys fs.FS) ([]RuleSet, error) {
	sets, err := readDefaults(fsys)
	if err != nil {
		return nil, err
	}
	out := make([]RuleSet, len(sets))
	for i := range sets {
		out[i] = setFromInternal(&sets[i])
	}
	return out, nil
}

// checkRules runs one gate: a gateChecks name, or "Golden/a", "Golden/b" or
// "Golden/c" for one part of Golden. t is a *testing.T in TestRules and a
// recorder in the fixtures.
func checkRules(t testing.TB, e *gateEnv, check string) {
	t.Helper()
	switch check {
	case "Provenance":
		checkProvenance(t, e)
	case "Schema":
		checkSchema(t, e)
	case "Compile":
		checkCompile(t, e)
	case "Counts":
		checkCounts(t, e)
	case "Risky":
		checkRisky(t, e)
	case "Golden":
		gateRun(t, "a", func(t testing.TB) { checkGoldenRows(t, e) })
		gateRun(t, "b", func(t testing.TB) { checkCorpusGate(t, e) })
		gateRun(t, "c", func(t testing.TB) { checkCorpusDelta(t, e) })
	case "Golden/a":
		checkGoldenRows(t, e)
	case "Golden/b":
		checkCorpusGate(t, e)
	case "Golden/c":
		checkCorpusDelta(t, e)
	default:
		t.Fatalf("unknown check %q", check)
	}
}

// gateRun runs f as a subtest under a *testing.T, and inline otherwise.
func gateRun(t testing.TB, name string, f func(t testing.TB)) {
	if tt, ok := t.(*testing.T); ok {
		tt.Run(name, func(t *testing.T) { f(t) })
		return
	}
	f(t)
}

// skipNoBase logs the one line a base-relative part writes when it is skipped
// (§11.3, UM-R4-2). It reports whether the part must be skipped.
func skipNoBase(t testing.TB, e *gateEnv, part string) bool {
	t.Helper()
	if e.base != nil {
		return false
	}
	t.Logf(msgSkipNoBase, part)
	return true
}

// gitBlobSHA1 is the git object ID of data as a blob.
func gitBlobSHA1(data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// spdxOf identifies the upstream LICENSE text: MPL-2.0 iff its first line is
// the MPL 2.0 title, else NOASSERTION.
func spdxOf(license []byte) string {
	first, _, _ := bytes.Cut(license, []byte("\n"))
	if strings.TrimSpace(string(first)) == "Mozilla Public License Version 2.0" {
		return braveSPDX
	}
	return "NOASSERTION"
}

// lockedPaths are the upstream paths the lock must hold, with their vendored
// paths, in lock order.
var lockedPaths = []struct{ upstream, vendored string }{
	{"brave-lists/clean-urls.json", "rules/brave/clean-urls.json"},
	{"brave-lists/query-filter.json", "rules/brave/query-filter.json"},
	{"brave-lists/debounce.json", "rules/brave/debounce.json"},
	{"LICENSE", "rules/brave/LICENSE"},
}

// checkProvenance is rules-provenance (§11.3): the local checks, then the
// base-relative acks-changed check, hold match and Upstream checks.
func checkProvenance(t testing.TB, e *gateEnv) {
	t.Helper()
	lock, err := readLock(e.data)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(lock.Upstream.Commit) {
		t.Errorf("lock commit %q is not 40-hex", lock.Upstream.Commit)
	}
	if len(lock.Upstream.Files) != len(lockedPaths) {
		t.Errorf("lock holds %d files, want %d", len(lock.Upstream.Files), len(lockedPaths))
	}
	for _, p := range lockedPaths {
		lf, ok := lock.Upstream.Files[p.upstream]
		if !ok {
			t.Errorf("lock: %s is not locked", p.upstream)
			continue
		}
		if lf.Vendored != p.vendored {
			t.Errorf("lock: %s is vendored at %q, want %q", p.upstream, lf.Vendored, p.vendored)
			continue
		}
		var data []byte
		if p.upstream == "LICENSE" {
			// Not embedded: read from the working tree.
			data, err = fs.ReadFile(e.tree, lf.Vendored)
			if lf.SPDX != braveSPDX {
				t.Errorf("lock: LICENSE spdx %q, want %s", lf.SPDX, braveSPDX)
			}
			if err == nil && spdxOf(data) != braveSPDX {
				t.Errorf("%s: SPDX id %s, want %s", lf.Vendored, spdxOf(data), braveSPDX)
			}
		} else {
			data, err = fs.ReadFile(e.data, lf.Vendored)
		}
		if err != nil {
			t.Errorf("%s: %v", p.upstream, err)
			continue
		}
		if got := gitBlobSHA1(data); got != lf.GitBlobSHA1 {
			t.Errorf("%s: git blob %s, lock %s", lf.Vendored, got, lf.GitBlobSHA1)
		}
		if got := sha256Hex(data); got != lf.SHA256 {
			t.Errorf("%s: sha256 %s, lock %s", lf.Vendored, got, lf.SHA256)
		}
	}
	if notice, err := fs.ReadFile(e.tree, "rules/brave/NOTICE"); err != nil {
		t.Error(err)
	} else {
		checkNotice(t, &lock, notice)
	}
	checkAcksChanged(t, e)
	checkHolds(t, e, &lock)
	checkUpstream(t, e, &lock)
}

// checkNotice: rules/brave/NOTICE names the locked commit and exactly the
// locked blobs.
func checkNotice(t testing.TB, lock *lockFile, notice []byte) {
	t.Helper()
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

// checkSchema is rules-schema (§11.3).
func checkSchema(t testing.TB, e *gateEnv) {
	t.Helper()
	// The strict decoders of internal/brave and decodeOverlay run inside
	// readDefaults; any schema problem fails here.
	if _, err := readDefaults(e.data); err != nil {
		t.Error(err)
	}
	for _, f := range []struct {
		path string
		re   *regexp.Regexp
	}{
		{"testdata/keep-coverage-exempt.txt", regexp.MustCompile(`^\S+ \S+ # \S.*$`)},
		{"testdata/corpus/eval-2026-10/known-broken.txt", regexp.MustCompile(`^[a-z]+-\d+ # \S.*$`)},
	} {
		for i, line := range readLinesFS(t, e.tree, f.path) {
			if !f.re.MatchString(line) {
				t.Errorf("%s:%d: %q does not match %s", f.path, i+1, line, f.re)
			}
		}
	}
	data, err := fs.ReadFile(e.tree, acksPath)
	if err != nil {
		t.Errorf("%s: %v", acksPath, err)
		return
	}
	_, errs := parseAcks(data)
	for _, err := range errs {
		t.Errorf("%s: %v", acksPath, err)
	}
}

// readLines returns the non-comment, non-blank lines of a human-owned list.
func readLines(t testing.TB, path string) []string {
	t.Helper()
	return readLinesFS(t, os.DirFS("."), path)
}

func readLinesFS(t testing.TB, fsys fs.FS, path string) []string {
	t.Helper()
	data, err := fs.ReadFile(fsys, path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(data))
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

// checkCompile is rules-compile (§11.3).
func checkCompile(t testing.TB, e *gateEnv) {
	t.Helper()
	srcs, err := e.sources()
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{Sources: srcs})
	if err != nil {
		t.Fatal(err)
	}
	// Nothing dropped: every rule in is a rule out. SetInfo.Rules counts
	// the rules Compile emitted, not the input.
	for i, si := range c.Sources() {
		if si.Rules != len(srcs[i].Rules) {
			t.Errorf("%s: %d rules compiled, %d in", si.Name, si.Rules, len(srcs[i].Rules))
		}
	}
	// Debounce accounting (§4.5.3): compiled + skipped instances = upstream.
	deb, _ := fs.ReadFile(e.data, "rules/brave/debounce.json")
	cls, _ := fs.ReadFile(e.data, classesPath)
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

	functional := corpusFunctionalNamesFS(t, e.tree)
	golden := goldenRuleNamesFS(t, e.tree)
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
			if isGlobal(r) && r.Action == Strip {
				for _, p := range r.Params {
					if slices.Contains(neverStrip, query.Canon(p)) {
						t.Errorf("%s: global param %q is a never-strip name", id, p)
					}
				}
			}
		}
	}
	probeGlobalRegexps(t, srcs, functional)
	testKeepCoverage(t, e, srcs)
}

// probeGlobalRegexps is the §11.3 probe: no global-scope Strip ParamRegexps,
// in the overlay or a built-in set, matches a never-strip name or a corpus
// functional_params name.
func probeGlobalRegexps(t testing.TB, srcs []RuleSet, functional map[string]bool) {
	t.Helper()
	for _, s := range srcs {
		for _, r := range s.Rules {
			if !isGlobal(r) || r.Action != Strip {
				continue
			}
			id := s.Name + ":" + r.ID
			for _, pr := range r.ParamRegexps {
				re := regexp.MustCompile("^(?:" + pr + ")$")
				for _, n := range neverStrip {
					if re.MatchString(n) {
						t.Errorf("%s: global paramRegexp %q matches never-strip name %q", id, pr, n)
					}
				}
				for _, n := range slices.Sorted(maps.Keys(functional)) {
					if re.MatchString(n) {
						t.Errorf("%s: global paramRegexp %q matches corpus functional name %q", id, pr, n)
					}
				}
			}
		}
	}
}

func isGlobal(r Rule) bool {
	for _, p := range r.Include {
		if isGlobalPattern(p) {
			return true
		}
	}
	return false
}

// isGlobalPattern reports whether a pattern's host is "*".
func isGlobalPattern(p string) bool {
	_, rest, _ := strings.Cut(p, "://")
	return strings.HasPrefix(rest, "*/")
}

// corpusFunctionalNames returns the canonical functional_params names of the
// corpus, except those of items the signed or archive guard protects: there
// the guard, not a functional meaning, keeps the name (negatives-004's S3
// SigV4 and negatives-046's Wayback utm_source). Probing them would fail M-9
// and F-1, which §11.3 says the probe covers.
func corpusFunctionalNames(t testing.TB) map[string]bool {
	return corpusFunctionalNamesFS(t, os.DirFS("."))
}

func corpusFunctionalNamesFS(t testing.TB, fsys fs.FS) map[string]bool {
	out := map[string]bool{}
	guarded := MustNew(Config{Sources: []RuleSet{}})
	for _, it := range readCorpusFS(t, fsys) {
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
	return goldenRuleNamesFS(t, os.DirFS("."))
}

func goldenRuleNamesFS(t testing.TB, fsys fs.FS) map[string]bool {
	out := map[string]bool{}
	for _, c := range readCasesFS(t, fsys, "testdata/golden/positive.txtar") {
		for _, id := range c.Rules {
			out[id] = true
		}
	}
	return out
}

func readCasesFS(t testing.TB, fsys fs.FS, path string) []goldenCase {
	t.Helper()
	data, err := fs.ReadFile(fsys, path)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := parseCases(path, data)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// testKeepCoverage is the §11.3 keep-coverage probe: every Brave include that
// strips a name an A- or K-entry Keep protects must be covered by such a Keep,
// or exempted in testdata/keep-coverage-exempt.txt.
func testKeepCoverage(t testing.TB, e *gateEnv, srcs []RuleSet) {
	t.Helper()
	exempt := map[string]bool{}
	for _, l := range readLinesFS(t, e.tree, "testdata/keep-coverage-exempt.txt") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue // rules-schema reports the line
		}
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
	for _, k := range slices.Sorted(maps.Keys(exempt)) {
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

// checkCounts is rules-counts (§11.3, §4.5.3): the absolute floors, then the
// base-relative union floor and debounce include-drop limit.
func checkCounts(t testing.TB, e *gateEnv) {
	t.Helper()
	h := countData(e.data)
	cu, qf, deb := h.files[0], h.files[1], h.debounce
	if cu.rules < 41 || cu.params < 340 {
		t.Errorf("clean-urls: %d rules / %d params, floors 41 / 340", cu.rules, cu.params)
	}
	if qf.rules < 3 || qf.params < 57 {
		t.Errorf("query-filter: %d rules / %d params, floors 3 / 57", qf.rules, qf.params)
	}
	if g := h.union.global; g < 196 {
		t.Errorf("union global params: %d, floor 196", g)
	}
	if deb.rules < 38 || deb.includes < 150 {
		t.Errorf("debounce: %d rules / %d include instances, floors 38 / 150", deb.rules, deb.includes)
	}
	t.Logf("clean-urls %d/%d, query-filter %d/%d, union %d (global %d, scoped %d), debounce %d/%d",
		cu.rules, cu.params, qf.rules, qf.params, h.union.params, h.union.global, h.union.scoped, deb.rules, deb.includes)
	checkCountsRelative(t, e, h)
}

// checkGoldenRows is rules-golden (a): every Appendix C row, under its
// policies and flags, plus the invariants per case, and every rules: line is
// decisive (disabling the rule changes the result).
func checkGoldenRows(t testing.TB, e *gateEnv) {
	t.Helper()
	srcs, key := e.cleanerSources(t)
	cases := readCasesFS(t, e.tree, "testdata/golden/positive.txtar")
	for i := range cases {
		c := &cases[i]
		gateRun(t, c.ID, func(t testing.TB) {
			checkCase(t, c, srcs, key)
			for _, id := range c.Rules {
				if !decisive(t, c, id, srcs, key) {
					t.Errorf("%s: rules: %s does not change the result when disabled", c.ID, id)
				}
			}
		})
	}
	data, err := fs.ReadFile(e.tree, "testdata/golden/negative.txt")
	if err != nil {
		t.Fatal(err)
	}
	negs, err := parseNegatives(data)
	if err != nil {
		t.Fatal(err)
	}
	keep := cachedCleaner(t, key+"|negative-keep", Config{Sources: srcs})
	strip := cachedCleaner(t, key+"|negative-strip", Config{Sources: srcs, Affiliate: AffiliateStrip})
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
func decisive(t testing.TB, c *goldenCase, id string, sources []RuleSet, srcKey string) bool {
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
		on := caseCleaner(t, c, pw.p, sources, srcKey)
		cfg := c.Config
		cfg.Affiliate = pw.p
		cfg.Sources = sources
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

const corpusDir = "testdata/corpus/eval-2026-10"

func readCorpus(t testing.TB) []corpusItem {
	t.Helper()
	return readCorpusFS(t, os.DirFS("."))
}

func readCorpusFS(t testing.TB, fsys fs.FS) []corpusItem {
	t.Helper()
	files, err := fs.Glob(fsys, corpusDir+"/*.json")
	if err != nil {
		t.Fatal(err)
	}
	var out []corpusItem
	for _, f := range files {
		if path.Base(f) == "adjudicated.json" {
			continue
		}
		data, err := fs.ReadFile(fsys, f)
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
		for q := part; ; {
			s, rest, ok := query.Next(q)
			if !ok {
				break
			}
			if s.Name != "" {
				m[query.Canon(s.Name)] = true
			}
			q = rest
		}
	}
	return m
}

// corpusResult is one corpus item cleaned under AffiliateKeep.
type corpusResult struct {
	Item   corpusItem
	Out    string
	Exact  bool // the output is expected or an alt_expected
	Broken bool // §11.3 rules-golden (b)
}

// corpusStats is the corpus cleaned by one Cleaner.
type corpusStats struct {
	Results                 []corpusResult
	Exact, Tracked, Removed int // exact-or-alt items; tracker names present, and removed
}

// recall is tracker recall in percent.
func (s *corpusStats) recall() float64 {
	return 100 * float64(s.Removed) / float64(max(s.Tracked, 1))
}

// evalCorpus cleans every item with c. An item is broken iff its output is
// not in {expected} ∪ alt_expected and either expected == input and the
// output differs, or a functional_params name present in the post-unwrap URL
// (the last hop target, else the input) is absent from the output.
func evalCorpus(c *Cleaner, items []corpusItem) corpusStats {
	var st corpusStats
	for _, it := range items {
		out := c.Clean(it.Input)
		r := corpusResult{Item: it, Out: out, Exact: out == it.Expected || slices.Contains(it.Alt, out)}
		if r.Exact {
			st.Exact++
		}
		post := it.Input
		if x := c.Explain(it.Input); len(x.Hops) > 0 {
			post = x.Hops[len(x.Hops)-1].To
		}
		pn, on := paramNames(post), paramNames(out)
		if !r.Exact {
			if it.Expected == it.Input && out != it.Input {
				r.Broken = true
			}
			for _, f := range it.Functional {
				if cn := query.Canon(f); pn[cn] && !on[cn] {
					r.Broken = true
				}
			}
		}
		for _, n := range it.Tracking {
			if strings.HasPrefix(n, "/") {
				continue
			}
			if cn := query.Canon(n); pn[cn] {
				st.Tracked++
				if !on[cn] {
					st.Removed++
				}
			}
		}
		st.Results = append(st.Results, r)
	}
	return st
}

// checkCorpusGate is rules-golden (b) (§11.3): no new broken links.
func checkCorpusGate(t testing.TB, e *gateEnv) {
	t.Helper()
	known := map[string]bool{}
	for _, l := range readLinesFS(t, e.tree, corpusDir+"/known-broken.txt") {
		id, _, _ := strings.Cut(l, " ")
		known[id] = true
	}
	srcs, key := e.cleanerSources(t)
	c := cachedCleaner(t, key+"|corpus", Config{Sources: srcs})
	st := evalCorpus(c, readCorpusFS(t, e.tree))
	for _, r := range st.Results {
		if c.Clean(r.Out) != r.Out {
			t.Errorf("%s: not idempotent: %s", r.Item.ID, r.Out)
		}
		switch {
		case r.Broken && !known[r.Item.ID]:
			t.Errorf("%s broken (not in known-broken.txt):\n  in   %s\n  out  %s\n  want %s", r.Item.ID, r.Item.Input, r.Out, r.Item.Expected)
		case !r.Broken && known[r.Item.ID]:
			t.Logf("stale known-broken entry %s", r.Item.ID)
		}
	}
	// Today 227/247: the capture-status overlay entries are absent (§4.7).
	// A drop is Golden (c)'s to judge, against base.
	t.Logf("corpus: exact-or-alt %d/%d; tracker recall %d/%d (%.1f%%)", st.Exact, len(st.Results), st.Removed, st.Tracked, st.recall())
}
