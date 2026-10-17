package data

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
)

// root is the linkclean repository root, relative to this package.
const root = "../../../.."

func readRoot(t testing.TB, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func realLock(t testing.TB) Lock {
	t.Helper()
	l, err := ParseLock(readRoot(t, LockPath))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestLockRoundTrip(t *testing.T) {
	raw := readRoot(t, LockPath)
	l := realLock(t)
	if got := l.Encode(); !bytes.Equal(got, raw) {
		t.Errorf("Encode differs from %s:\n%s", LockPath, got)
	}
	// The lenient reading (json tags) agrees with the strict one.
	var lenient Lock
	if err := json.Unmarshal(raw, &lenient); err != nil {
		t.Fatal(err)
	}
	if got := lenient.Encode(); !bytes.Equal(got, raw) {
		t.Errorf("Encode after json.Unmarshal differs:\n%s", got)
	}
}

func TestNotice(t *testing.T) {
	l := realLock(t)
	if got, want := Notice(&l), readRoot(t, NoticePath); !bytes.Equal(got, want) {
		t.Errorf("Notice differs from %s:\n%s", NoticePath, got)
	}
}

func TestRealFiles(t *testing.T) {
	l := realLock(t)
	for _, s := range Files {
		b := readRoot(t, s.Vendored)
		lf := l.Upstream.Files[s.Upstream]
		if got := repo.BlobID(b); got != lf.GitBlobSHA1 {
			t.Errorf("%s: BlobID %s, lock %s", s.Vendored, got, lf.GitBlobSHA1)
		}
		if got := SHA256Hex(b); got != lf.SHA256 {
			t.Errorf("%s: sha256 %s, lock %s", s.Vendored, got, lf.SHA256)
		}
		if got := CountsOf(s.Kind, b); got != lf.Counts {
			t.Errorf("%s: CountsOf %+v, lock %+v", s.Vendored, got, lf.Counts)
		}
		if s.Kind == KindLicense {
			if got := SPDXOf(b); got != lf.SPDX {
				t.Errorf("SPDXOf(LICENSE) = %q, lock %q", got, lf.SPDX)
			}
		}
	}
}

// TestSyncedLock rebuilds the lock from the vendored bytes alone, as sync
// does, and checks it and its NOTICE against the committed files.
func TestSyncedLock(t *testing.T) {
	real := realLock(t)
	l := Lock{Schema: 4, Upstream: LockUpstream{
		Repo: RepoPath, Submodule: Submodule, Branch: Branch,
		Commit: real.Upstream.Commit, CommitDate: real.Upstream.CommitDate,
		Files: map[string]LockFile{},
	}}
	for _, s := range Files {
		b := readRoot(t, s.Vendored)
		f := LockFile{Vendored: s.Vendored, GitBlobSHA1: repo.BlobID(b), SHA256: SHA256Hex(b), Counts: CountsOf(s.Kind, b)}
		if s.Kind == KindLicense {
			f.SPDX = SPDXOf(b)
		}
		l.Upstream.Files[s.Upstream] = f
	}
	if !bytes.Equal(l.Encode(), readRoot(t, LockPath)) {
		t.Errorf("lock rebuilt from the vendored files differs:\n%s", l.Encode())
	}
	if !bytes.Equal(Notice(&l), readRoot(t, NoticePath)) {
		t.Errorf("NOTICE rebuilt from the vendored files differs:\n%s", Notice(&l))
	}
}

func TestParseLockRejects(t *testing.T) {
	raw := string(readRoot(t, LockPath))
	const clean = `"brave-lists/clean-urls.json": {
        "vendored": "rules/brave/clean-urls.json",`
	tests := []struct {
		name, old, new, want string
	}{
		{"schema", `"schema": 4`, `"schema": 3`, "schema 3, want 4"},
		{"unknown_field", `"schema": 4,`, `"schema": 4, "checked": "x",`, `unknown field "checked"`},
		{"duplicate_key", `"schema": 4,`, `"schema": 4, "schema": 4,`, `duplicate key "schema"`},
		{"repo", `"repo": "github.com/brave/adblock-lists"`, `"repo": "github.com/evil/adblock-lists"`, "upstream.repo"},
		{"branch", `"branch": "master"`, `"branch": "main"`, "upstream.branch"},
		{"short_commit", `"commit": "8846788d578bf89696ad4ec5cdff845dfb170f1a"`, `"commit": "8846788d"`, "upstream.commit"},
		{"date", `"commit_date": "2026-10-14T19:19:33-07:00"`, `"commit_date": "2026-10-14"`, "commit_date"},
		{"vendored", clean, strings.Replace(clean, "rules/brave/clean-urls.json", "rules/overlay/overlay.json", 1), ".vendored"},
		{"missing_file", `"LICENSE": {`, `"LICENSE2": {`, `missing "LICENSE"`},
		{"includes_on_strip", `"counts": {"rules": 51, "params": 425, "global_params": 213}`, `"counts": {"rules": 51, "params": 425, "global_params": 213, "includes": 1}`, "unexpected includes"},
		{"missing_includes", `"counts": {"rules": 48, "includes": 190}`, `"counts": {"rules": 48}`, "missing includes"},
		{"counts_on_license", `"spdx": "MPL-2.0"`, `"spdx": "MPL-2.0", "counts": {"rules": 1}`, "counts on LICENSE"},
		{"missing_spdx", `,
        "spdx": "MPL-2.0"`, ``, "missing spdx"},
		{"uppercase_sha", `"sha256": "c2664e01`, `"sha256": "C2664E01`, ".sha256"},
		{"trailing", "}\n}\n", "}\n}\n{}", "trailing data"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := strings.Replace(raw, tt.old, tt.new, 1)
			if b == raw {
				t.Fatalf("mutation %q not applied", tt.old)
			}
			_, err := ParseLock([]byte(b))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ParseLock error %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestCountsOfLenient(t *testing.T) {
	tests := []struct {
		name string
		kind Kind
		in   string
		want Counts
	}{
		{"empty_list", KindStrip, `[]`, Counts{}},
		{"truncated", KindStrip, `[{"include": ["*://*/*"], "params": ["a"]`, Counts{}},
		{"object", KindStrip, `{"include": []}`, Counts{}},
		{"non_object_element", KindStrip, `[1, "x", {"include": ["*://a.com/*"], "params": ["a", "b"]}]`, Counts{Rules: 3, Params: 2}},
		{"global", KindStrip, `[{"include": ["*://a.com/*", "*://*/*"], "params": ["a", "b"]}, {"include": ["*://*/x*"], "params": ["c"]}]`, Counts{Rules: 2, Params: 3, GlobalParams: 3}},
		{"global_once_per_rule", KindStrip, `[{"include": ["*://*/*", "http://*/*"], "params": ["a"]}]`, Counts{Rules: 1, Params: 1, GlobalParams: 1}},
		{"subdomain_wildcard_not_global", KindStrip, `[{"include": ["*://*.a.com/*"], "params": ["a"]}]`, Counts{Rules: 1, Params: 1}},
		{"renamed_key", KindStrip, `[{"include": ["*://*/*"], "parameters": ["a"]}]`, Counts{Rules: 1}},
		{"params_not_array", KindStrip, `[{"include": ["*://*/*"], "params": "a"}]`, Counts{Rules: 1}},
		{"non_string_include", KindStrip, `[{"include": [1, "*://*/*"], "params": ["a"]}]`, Counts{Rules: 1, Params: 1, GlobalParams: 1}},
		{"debounce", KindDebounce, `[{"include": ["*://a/*", "*://b/*"], "params": ["x"]}, {"include": ["*://c/*"]}]`, Counts{Rules: 2, Includes: 3}},
		{"license", KindLicense, `[{"include": ["*://a/*"]}]`, Counts{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CountsOf(tt.kind, []byte(tt.in)); got != tt.want {
				t.Errorf("CountsOf = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSPDXOf(t *testing.T) {
	for in, want := range map[string]string{
		"Mozilla Public License Version 2.0\n==\n": "MPL-2.0",
		"Mozilla Public License Version 2.0\r\n":   "MPL-2.0",
		"Mozilla Public License Version 2.0":       "MPL-2.0",
		"Mozilla Public License Version 1.1\n":     "NOASSERTION",
		"MIT License\n":                            "NOASSERTION",
		"":                                         "NOASSERTION",
	} {
		if got := SPDXOf([]byte(in)); got != want {
			t.Errorf("SPDXOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInWriteSet(t *testing.T) {
	for p, want := range map[string]bool{
		"rules.lock.json":               true,
		"rules/brave/clean-urls.json":   true,
		"rules/brave/NOTICE":            true,
		"rules/brave/LICENSE":           true,
		"rules/brave/new-file":          true,
		"rules/brave/":                  false,
		"rules/brave":                   false,
		"rules/brave/sub/x.json":        false,
		"rules/brave/..":                false,
		"rules/brave/.":                 false,
		"rules/brave//x":                false,
		"rules/overlay/overlay.json":    false,
		"rules/acks.json":               false,
		"./rules.lock.json":             false,
		"tools/upstream/adblock-lists":  false,
		"rules/brave/../acks.json":      false,
		"rules/overlay/../brave/NOTICE": false,
	} {
		if got := InWriteSet(p); got != want {
			t.Errorf("InWriteSet(%q) = %v, want %v", p, got, want)
		}
	}
}

// canonTable is the root module's internal/query TestCanon table, copied so
// the parity holds even when that file is absent.
var canonTable = []struct{ in, want string }{
	{"utm_source", "utm_source"},
	{"UTM_SOURCE", "utm_source"},
	{"utm%5Fsource", "utm_source"},
	{"utm%5fSource", "utm_source"},
	{"%41", "a"},
	{"%2541", "%41"},
	{"a+b", "a+b"},
	{"a%2Bb", "a+b"},
	{"a%20b", "a b"},
	{"%", "%"},
	{"%4", "%4"},
	{"%zz", "%zz"},
	{"%%41", "%a"},
	{"ÉtÉ", "ÉtÉ"},
	{"%C3%89", "É"},
	{"\xff", "\xff"},
	{"", ""},
	{"%4G", "%4g"},
	{"a%", "a%"},
	{"a%4", "a%4"},
	{"%5A%5a", "zz"},
}

func TestCanon(t *testing.T) {
	for _, tt := range canonTable {
		if got := Canon(tt.in); got != tt.want {
			t.Errorf("Canon(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestCanonParity runs the root module's own TestCanon table (read from its
// source) against Canon, so a change there that is not mirrored here fails.
func TestCanonParity(t *testing.T) {
	path := filepath.Join(root, "internal", "query", "query_test.go")
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Skipf("root internal/query not available: %v", err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if d, ok := d.(*ast.FuncDecl); ok && d.Name.Name == "TestCanon" {
			fn = d
		}
	}
	if fn == nil {
		t.Fatal("no TestCanon in root internal/query/query_test.go")
	}
	n := 0
	ast.Inspect(fn, func(node ast.Node) bool {
		cl, ok := node.(*ast.CompositeLit)
		if !ok || len(cl.Elts) != 2 {
			return true
		}
		var s [2]string
		for i, e := range cl.Elts {
			lit, ok := e.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if s[i], err = strconv.Unquote(lit.Value); err != nil {
				t.Fatal(err)
			}
		}
		n++
		if got := Canon(s[0]); got != s[1] {
			t.Errorf("Canon(%q) = %q, root query.Canon wants %q", s[0], got, s[1])
		}
		return false
	})
	if n < 10 {
		t.Errorf("found only %d rows in the root TestCanon table; has its shape changed?", n)
	}
}

func intp(n int) *int { return &n }

const hex64 = "c2664e019673f58dbefed33ed32f355dd5c8a781e395059918d87020a9e4dc05"

func TestParseAcks(t *testing.T) {
	good := `{
  "schema": 1,
  "acks": [
    {"check": "counts", "metric": "union.global_params", "min": 200,
     "vendored_sha256": "` + hex64 + `", "expires": "2026-12-01",
     "reason": "upstream deduplicated the global list", "by": "maintainer"},
    {"check": "risky", "scope": "*://*.example.com/*", "param": "t",
     "vendored_sha256": "` + hex64 + `", "expires": "2026-12-01", "reason": "share token on that host only", "by": "maintainer"}
  ],
  "holds": [
    {"file": "brave-lists/clean-urls.json", "sha256": "` + hex64 + `",
     "expires": "2026-11-15", "reason": "waiting on upstream", "by": "maintainer"}
  ]
}`
	a, errs := ParseAcks([]byte(good))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if len(a.Acks) != 2 || len(a.Holds) != 1 || *a.Acks[0].Min != 200 || a.Acks[1].Param != "t" || a.Holds[0].Expires != "2026-11-15" {
		t.Errorf("ParseAcks = %+v", a)
	}
	if _, errs := ParseAcks([]byte(`{"schema": 1, "acks": [], "holds": []}`)); len(errs) > 0 {
		t.Errorf("empty file: %v", errs)
	}

	ack := Ack{Check: "counts", Metric: "union.params", Min: intp(1), VendoredSHA256: hex64, Expires: "2026-12-01", Reason: "r", By: "b"}
	hold := Hold{File: "brave-lists/debounce.json", SHA256: hex64, Expires: "2026-12-01", Reason: "r", By: "b"}
	enc := func(acks []Ack, holds []Hold) []byte {
		b, err := json.Marshal(Acks{Schema: 1, Acks: acks, Holds: holds})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if _, errs := ParseAcks(enc([]Ack{ack}, []Hold{hold})); len(errs) > 0 {
		t.Fatalf("valid entries: %v", errs)
	}
	mut := func(f func(*Ack)) []Ack { k := ack; f(&k); return []Ack{k} }
	mutH := func(f func(*Hold)) []Hold { h := hold; f(&h); return []Hold{h} }
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"unknown_field", []byte(`{"schema": 1, "acks": [], "holds": [], "x": 1}`), `unknown field "x"`},
		{"duplicate_key", []byte(`{"schema": 1, "acks": [], "holds": [], "acks": []}`), `duplicate key "acks"`},
		{"schema", []byte(`{"schema": 2, "acks": [], "holds": []}`), "schema 2"},
		{"missing_holds", []byte(`{"schema": 1, "acks": []}`), "missing holds"},
		{"truncated", []byte(`{"schema": 1, "acks": [`), "unexpected end"},
		{"unknown_check", enc(mut(func(k *Ack) { k.Check = "count" }), nil), `check "count"`},
		{"unknown_metric", enc(mut(func(k *Ack) { k.Metric = "union.rules" }), nil), `metric "union.rules"`},
		{"golden_metric_on_counts", enc(mut(func(k *Ack) { k.Metric = "corpus.exact" }), nil), "not a counts metric"},
		{"missing_min", enc(mut(func(k *Ack) { k.Min = nil }), nil), "missing min"},
		{"negative_min", enc(mut(func(k *Ack) { k.Min = intp(-1) }), nil), "min is negative"},
		{"scope_on_counts", enc(mut(func(k *Ack) { k.Scope = "*://*/*" }), nil), "only by a risky ack"},
		{"risky_without_param", enc(mut(func(k *Ack) { k.Check, k.Metric, k.Min, k.Scope = "risky", "", nil, "*://*/*" }), nil), "missing param"},
		{"risky_with_metric", enc(mut(func(k *Ack) { k.Check, k.Scope, k.Param = "risky", "*://*/*", "t" }), nil), "not used by a risky ack"},
		{"short_sha", enc(mut(func(k *Ack) { k.VendoredSHA256 = hex64[:12] }), nil), "vendored_sha256"},
		{"bad_date", enc(mut(func(k *Ack) { k.Expires = "2026-13-01" }), nil), "expires"},
		{"no_reason", enc(mut(func(k *Ack) { k.Reason = " " }), nil), "missing reason"},
		{"no_by", enc(mut(func(k *Ack) { k.By = "" }), nil), "missing by"},
		{"hold_license", enc(nil, mutH(func(h *Hold) { h.File = "LICENSE" })), `file "LICENSE"`},
		{"hold_vendored_path", enc(nil, mutH(func(h *Hold) { h.File = "rules/brave/clean-urls.json" })), "is not one of"},
		{"hold_sha", enc(nil, mutH(func(h *Hold) { h.SHA256 = strings.ToUpper(hex64) })), "sha256"},
		{"hold_expires", enc(nil, mutH(func(h *Hold) { h.Expires = "" })), "expires"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, errs := ParseAcks(tt.in)
			for _, err := range errs {
				if strings.Contains(err.Error(), tt.want) {
					return
				}
			}
			t.Errorf("ParseAcks errors %v, want one containing %q", errs, tt.want)
		})
	}
}

// TestRealAcks parses the committed rules/acks.json, which the root gate
// also reads.
func TestRealAcks(t *testing.T) {
	if _, errs := ParseAcks(readRoot(t, AcksPath)); len(errs) > 0 {
		t.Errorf("%s: %v", AcksPath, errs)
	}
}

func TestExpired(t *testing.T) {
	tests := []struct {
		expires string
		now     time.Time
		want    bool
	}{
		{"2026-12-01", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), false},
		{"2026-12-01", time.Date(2026, 12, 1, 23, 59, 59, 0, time.UTC), false},
		{"2026-12-01", time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC), true},
		{"2026-12-01", time.Date(2026, 11, 30, 12, 0, 0, 0, time.UTC), false},
		// 2026-12-01 20:00 at -05:00 is 2026-12-02 01:00 UTC.
		{"2026-12-01", time.Date(2026, 12, 1, 20, 0, 0, 0, time.FixedZone("", -5*3600)), true},
		{"bad", time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), true},
	}
	for _, tt := range tests {
		if got := Expired(tt.expires, tt.now); got != tt.want {
			t.Errorf("Expired(%q, %v) = %v, want %v", tt.expires, tt.now, got, tt.want)
		}
	}
}

func TestSummaryDataRoundTrip(t *testing.T) {
	d := SummaryData{
		Schema:         SummarySchema,
		HeadError:      "x",
		Corpus:         []CorpusRow{{ID: "a-1", Input: "https://a/?x=1&y=<2>", Base: "https://a/", Head: "https://a/", Expected: "https://a/", Alt: []string{}, BrokenHead: true}},
		CorpusScopes:   map[string]bool{"*://*.a.com/*": true},
		Hygiene:        []HygieneNote{{Kind: HygieneStaleKeep, RuleID: "K-1", Param: "st"}},
		AffiliateHosts: map[string]string{"a.example": "Net"},
	}
	got, err := ParseSummaryData(d.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Encode(), d.Encode()) {
		t.Errorf("round trip: %s", got.Encode())
	}
	if !bytes.Contains(d.Encode(), []byte("y=<2>")) {
		t.Error("Encode escapes HTML")
	}
	if _, err := ParseSummaryData([]byte(`{"schema": 1, "corpus": [], "extra": 1}`)); err == nil {
		t.Error("unknown field accepted")
	}
	if _, err := ParseSummaryData([]byte(`{"schema": 2}`)); err == nil {
		t.Error("schema 2 accepted")
	}
}
