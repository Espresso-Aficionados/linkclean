package linkclean

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
)

// txtarFile is one "-- name --" section of a txtar archive.
type txtarFile struct {
	Name string
	Data []byte
	Line int // line of the section marker
}

// parseTxtar splits data into its leading comment and its sections, as
// golang.org/x/tools/txtar does (without the dependency).
func parseTxtar(data []byte) (comment []byte, files []txtarFile) {
	lines := bytes.SplitAfter(data, []byte("\n"))
	cur := -1
	for i, l := range lines {
		s := strings.TrimRight(string(l), "\r\n")
		if strings.HasPrefix(s, "-- ") && strings.HasSuffix(s, " --") && len(s) > 6 {
			files = append(files, txtarFile{Name: strings.TrimSpace(s[3 : len(s)-3]), Line: i + 1})
			cur = len(files) - 1
			continue
		}
		if cur < 0 {
			comment = append(comment, l...)
		} else {
			files[cur].Data = append(files[cur].Data, l...)
		}
	}
	return comment, files
}

// goldenCase is one case of the §6.3 grammar (testdata/golden/positive.txtar,
// testdata/semantics/*.txtar).
type goldenCase struct {
	ID, File  string
	Line      int
	In        string
	Want      string
	WantStrip string // "" if absent
	Policy    string // "both", "keep" or "strip"
	Config    Config
	RawConfig string
	Rules     []string
	Abandoned *bool
	Skipped   string
	Hops      []map[string]any // nil: not compared; empty non-nil: "hops: none"
	Removed   []map[string]any
	Kept      []map[string]any
}

func parseCases(file string, data []byte) ([]goldenCase, error) {
	_, files := parseTxtar(data)
	var out []goldenCase
	seen := map[string]bool{}
	for _, f := range files {
		c := goldenCase{ID: f.Name, File: file, Line: f.Line, Policy: "both"}
		if seen[c.ID] {
			return nil, fmt.Errorf("%s:%d: duplicate case %q", file, f.Line, c.ID)
		}
		seen[c.ID] = true
		var hasIn, hasWant bool
		sc := bufio.NewScanner(bytes.NewReader(f.Data))
		sc.Buffer(nil, 1<<20)
		for n := f.Line + 1; sc.Scan(); n++ {
			line := sc.Text()
			if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, val, ok := strings.Cut(line, ": ")
			if !ok {
				key, val, ok = strings.Cut(line, ":")
			}
			if !ok {
				return nil, fmt.Errorf("%s:%d: %q is not \"key: value\"", file, n, line)
			}
			obj := func() (map[string]any, error) {
				var m map[string]any
				dec := json.NewDecoder(strings.NewReader(val))
				dec.UseNumber()
				if err := dec.Decode(&m); err != nil {
					return nil, fmt.Errorf("%s:%d: %s: %v", file, n, key, err)
				}
				return m, nil
			}
			var err error
			var m map[string]any
			switch key {
			case "in":
				if hasIn {
					return nil, fmt.Errorf("%s:%d: second in:", file, n)
				}
				c.In, hasIn = val, true
			case "want":
				if hasWant {
					return nil, fmt.Errorf("%s:%d: second want:", file, n)
				}
				c.Want, hasWant = val, true
			case "want-strip":
				c.WantStrip = val
			case "policy":
				if val != "both" && val != "keep" && val != "strip" {
					return nil, fmt.Errorf("%s:%d: policy %q", file, n, val)
				}
				c.Policy = val
			case "config":
				dec := json.NewDecoder(strings.NewReader(val))
				dec.DisallowUnknownFields()
				if err := dec.Decode(&c.Config); err != nil {
					return nil, fmt.Errorf("%s:%d: config: %v", file, n, err)
				}
				if c.Config.Affiliate != AffiliateKeep || c.Config.Sources != nil {
					return nil, fmt.Errorf("%s:%d: config may not set Affiliate or Sources", file, n)
				}
				c.RawConfig = val
			case "rules":
				c.Rules = strings.Fields(val)
			case "abandoned":
				b := val == "true"
				if !b && val != "false" {
					return nil, fmt.Errorf("%s:%d: abandoned %q", file, n, val)
				}
				c.Abandoned = &b
			case "skipped":
				c.Skipped = val
			case "hops":
				if val != "none" {
					return nil, fmt.Errorf("%s:%d: hops: %q (want none)", file, n, val)
				}
				c.Hops = []map[string]any{}
			case "hop":
				if m, err = obj(); err == nil {
					c.Hops = append(c.Hops, m)
				}
			case "removed":
				if val == "none" {
					c.Removed = []map[string]any{}
				} else if m, err = obj(); err == nil {
					c.Removed = append(c.Removed, m)
				}
			case "kept":
				if m, err = obj(); err == nil {
					c.Kept = append(c.Kept, m)
				}
			case "note":
			default:
				return nil, fmt.Errorf("%s:%d: unknown key %q", file, n, key)
			}
			if err != nil {
				return nil, err
			}
		}
		if !hasIn || !hasWant {
			return nil, fmt.Errorf("%s:%d: case %q needs in: and want:", file, f.Line, c.ID)
		}
		out = append(out, c)
	}
	return out, nil
}

// policies returns the affiliate policies a case runs under, each with its
// expected output.
func (c *goldenCase) policies() []struct {
	p    AffiliatePolicy
	want string
} {
	want := func(w string) string {
		if w == "unchanged" {
			return c.In
		}
		return w
	}
	strip := c.Want
	if c.WantStrip != "" {
		strip = c.WantStrip
	}
	type pw = struct {
		p    AffiliatePolicy
		want string
	}
	switch c.Policy {
	case "keep":
		return []pw{{AffiliateKeep, want(c.Want)}}
	case "strip":
		return []pw{{AffiliateStrip, want(c.Want)}}
	}
	return []pw{{AffiliateKeep, want(c.Want)}, {AffiliateStrip, want(strip)}}
}

// cleanerCache shares Cleaners between cases with the same configuration.
var cleanerCache sync.Map // string -> *Cleaner

func cachedCleaner(t testing.TB, key string, cfg Config) *Cleaner {
	t.Helper()
	if c, ok := cleanerCache.Load(key); ok {
		return c.(*Cleaner)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New(%s): %v", key, err)
	}
	cleanerCache.Store(key, c)
	return c
}

// caseCleaner returns the Cleaner a case runs with under policy p, over
// sources (nil: the defaults).
func caseCleaner(t testing.TB, c *goldenCase, p AffiliatePolicy, sources []RuleSet, srcKey string) *Cleaner {
	cfg := c.Config
	cfg.Affiliate = p
	cfg.Sources = sources
	return cachedCleaner(t, fmt.Sprintf("%s|%d|%s", srcKey, p, c.RawConfig), cfg)
}

// checkCase runs every assertion of the §6.3 grammar for c.
func checkCase(t *testing.T, c *goldenCase, sources []RuleSet, srcKey string) {
	t.Helper()
	for _, pw := range c.policies() {
		cl := caseCleaner(t, c, pw.p, sources, srcKey)
		name := map[AffiliatePolicy]string{AffiliateKeep: "keep", AffiliateStrip: "strip"}[pw.p]
		got := cl.Clean(c.In)
		if got != pw.want {
			t.Errorf("%s:%d %s [%s]:\n  in   %s\n  got  %s\n  want %s", c.File, c.Line, c.ID, name, c.In, got, pw.want)
		}
		checkInvariants(t, cl, c.In, got)
		r := cl.Explain(c.In)
		if c.Abandoned != nil && r.UnwrapAbandoned != *c.Abandoned {
			t.Errorf("%s [%s]: UnwrapAbandoned = %v, want %v", c.ID, name, r.UnwrapAbandoned, *c.Abandoned)
		}
		if c.Skipped != "" && skipName(r.Skipped) != c.Skipped {
			t.Errorf("%s [%s]: Skipped = %s, want %s", c.ID, name, skipName(r.Skipped), c.Skipped)
		}
		if c.Hops != nil {
			compareMaps(t, c.ID+" ["+name+"] hops", hopMaps(r.Hops), c.Hops)
		}
		if c.Removed != nil {
			compareMaps(t, c.ID+" ["+name+"] removed", removalMaps(r.Removed), c.Removed)
		}
		if c.Kept != nil {
			compareMaps(t, c.ID+" ["+name+"] kept", keptMaps(r.Kept), c.Kept)
		}
	}
}

// checkInvariants asserts §5.6 for one input and its Clean output.
func checkInvariants(t testing.TB, cl *Cleaner, in, out string) {
	t.Helper()
	if again := cl.Clean(out); again != out {
		t.Errorf("not idempotent:\n  in   %s\n  out  %s\n  out2 %s", in, out, again)
	}
	r := cl.Explain(in)
	if r.URL != out {
		t.Errorf("Explain(%q).URL = %q, Clean = %q", in, r.URL, out)
	}
	if r.Changed != (out != in) {
		t.Errorf("Explain(%q).Changed = %v", in, r.Changed)
	}
	if out == in {
		return
	}
	for i := 0; i < len(out); i++ {
		if out[i] <= 0x20 || out[i] == 0x7f {
			t.Errorf("output of %q has byte %#x: %q", in, out[i], out)
			break
		}
	}
	if _, err := url.Parse(in); err == nil || len(r.Hops) > 0 {
		if _, err := url.Parse(out); err != nil {
			t.Errorf("url.Parse(%q) (from %q): %v", out, in, err)
		}
	}
}

func skipName(s SkipReason) string {
	return [...]string{"", "scheme", "unparseable", "ambiguous", "toolong", "archive", "roundcap"}[s]
}

func partName(p Part) string { return [...]string{"", "path", "query", "fragment", "nested"}[p] }

func keepReasonName(k KeepReason) string { return [...]string{"", "rule", "affiliate", "signed"}[k] }

func catName(c Category) string {
	b, err := c.MarshalText()
	if err != nil {
		return fmt.Sprint(uint8(c))
	}
	return string(b)
}

func hopMaps(hs []Hop) []map[string]any {
	var out []map[string]any
	for _, h := range hs {
		out = append(out, map[string]any{
			"from": h.From, "to": h.To, "rule": h.Rule, "category": catName(h.Category),
			"fromHost": h.FromHost, "network": h.Beneficiary.Network, "id": h.Beneficiary.ID,
		})
	}
	return out
}

func removalMaps(rs []Removal) []map[string]any {
	var out []map[string]any
	for _, r := range rs {
		out = append(out, map[string]any{
			"part": partName(r.Part), "category": catName(r.Category), "name": r.Name, "value": r.Value,
			"text": r.Text, "nestedIn": r.NestedIn, "rule": r.Rule,
			"network": r.Beneficiary.Network, "id": r.Beneficiary.ID,
		})
	}
	return out
}

func keptMaps(ks []Kept) []map[string]any {
	var out []map[string]any
	for _, k := range ks {
		out = append(out, map[string]any{"name": k.Name, "reason": keepReasonName(k.Reason), "rule": k.Rule})
	}
	return out
}

// compareMaps compares got with want entry by entry, on the keys want lists.
func compareMaps(t *testing.T, what string, got, want []map[string]any) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: got %d entries, want %d:\n  got  %v\n  want %v", what, len(got), len(want), got, want)
		return
	}
	for i := range want {
		for k, w := range want[i] {
			g, ok := got[i][k]
			if !ok {
				t.Errorf("%s[%d]: unknown key %q", what, i, k)
				continue
			}
			if fmt.Sprint(g) != fmt.Sprint(w) {
				t.Errorf("%s[%d].%s = %q, want %q", what, i, k, g, w)
			}
		}
	}
}

// negativeCase is one line of testdata/golden/negative.txt.
type negativeCase struct {
	ID, URL string
	Line    int
}

func parseNegatives(data []byte) ([]negativeCase, error) {
	var out []negativeCase
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		id, u, ok := strings.Cut(line, " ")
		if !ok || strings.ContainsAny(u, " \t") {
			return nil, fmt.Errorf("negative.txt:%d: want \"<id> <url>\", have %q", i+1, line)
		}
		out = append(out, negativeCase{ID: id, URL: u, Line: i + 1})
	}
	return out, nil
}

func readCases(t testing.TB, path string) []goldenCase {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := parseCases(path, data)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func TestGoldenFilesParse(t *testing.T) {
	cs := readCases(t, "testdata/golden/positive.txtar")
	if len(cs) == 0 {
		t.Fatal("no positive cases")
	}
	data, err := os.ReadFile("testdata/golden/negative.txt")
	if err != nil {
		t.Fatal(err)
	}
	ns, err := parseNegatives(data)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, c := range cs {
		ids[c.ID] = true
	}
	for _, n := range ns {
		if ids[n.ID] {
			t.Errorf("case %q is in both golden files", n.ID)
		}
		ids[n.ID] = true
	}
	t.Logf("%d positive, %d negative cases", len(cs), len(ns))
}

func TestTxtarParse(t *testing.T) {
	comment, files := parseTxtar([]byte("c1\nc2\n-- a --\nx\n-- b --\ny\nz\n"))
	if string(comment) != "c1\nc2\n" || len(files) != 2 || files[0].Name != "a" || string(files[1].Data) != "y\nz\n" {
		t.Fatalf("got %q %+v", comment, files)
	}
	_, err := parseCases("x", []byte("-- a --\nin: u\n"))
	if err == nil {
		t.Error("case without want: accepted")
	}
	_, err = parseCases("x", []byte("-- a --\nin: u\nwant: v\nbogus: 1\n"))
	if err == nil {
		t.Error("unknown key accepted")
	}
}
