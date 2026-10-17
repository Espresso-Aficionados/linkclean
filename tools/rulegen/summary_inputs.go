package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
)

// The summary decodes base and head data leniently at the raw-JSON level, so
// a schema-broken file still yields a summary; the gates report the schema.

// sumSide is one side of the summary: base (the basedir output set) or head
// (the working tree).
type sumSide struct {
	name    string // "base" or "head"
	fsys    fs.FS
	gitlink string     // "" when the side has none
	lock    *data.Lock // nil when absent or not JSON
	notes   []string   // parse problems, for the header

	strip    [2]sumStrip // clean-urls, query-filter
	debounce []sumDebRule
	classes  map[string]sumClass // debounce-classes.json patterns
}

// read returns a file of the side, or nil when it is absent.
func (s *sumSide) read(p string) []byte {
	b, err := fs.ReadFile(s.fsys, p)
	if err != nil {
		return nil
	}
	return b
}

// sha256Of is the sha256 of the side's vendored copy of the upstream file
// up, or "" when it is absent.
func (s *sumSide) sha256Of(up string) string {
	spec, _ := data.SpecOf(up)
	b := s.read(spec.Vendored)
	if b == nil {
		return ""
	}
	return data.SHA256Hex(b)
}

// lockSHA256 is the sha256 base's lock records for up, falling back to the
// vendored bytes when the lock is unusable.
func (s *sumSide) lockSHA256(up string) string {
	if s.lock != nil {
		if f, ok := s.lock.Upstream.Files[up]; ok && f.SHA256 != "" {
			return f.SHA256
		}
	}
	return s.sha256Of(up)
}

// sumParseError describes why b is not JSON, with the byte offset.
func sumParseError(b []byte) string {
	var v any
	err := json.Unmarshal(b, &v)
	if err == nil {
		return ""
	}
	if se, ok := errors.AsType[*json.SyntaxError](err); ok {
		return fmt.Sprintf(data.SumParseError, se.Offset) + ": " + se.Error()
	}
	return err.Error()
}

// load decodes the side's rule data.
func (s *sumSide) load() {
	if b := s.read(data.LockPath); b == nil {
		s.notes = append(s.notes, fmt.Sprintf("%s %s: missing", s.name, data.LockPath))
	} else if msg := sumParseError(b); msg != "" {
		s.notes = append(s.notes, fmt.Sprintf("%s %s: %s", s.name, data.LockPath, msg))
	} else {
		var l data.Lock
		if json.Unmarshal(b, &l) == nil {
			s.lock = &l
		} else {
			s.notes = append(s.notes, fmt.Sprintf("%s %s: unexpected shape", s.name, data.LockPath))
		}
	}
	for i, spec := range data.Files[:2] {
		b := s.read(spec.Vendored)
		s.strip[i] = sumParseStrip(b)
		s.noteJSON(spec.Vendored, b)
	}
	b := s.read(data.Files[2].Vendored)
	s.debounce = sumParseDebounce(b)
	s.noteJSON(data.Files[2].Vendored, b)
	b = s.read(data.ClassesPath)
	s.classes = sumParseClasses(b)
	s.noteJSON(data.ClassesPath, b)
}

func (s *sumSide) noteJSON(p string, b []byte) {
	switch {
	case b == nil:
		s.notes = append(s.notes, fmt.Sprintf("%s %s: missing", s.name, p))
	default:
		if msg := sumParseError(b); msg != "" {
			s.notes = append(s.notes, fmt.Sprintf("%s %s: %s", s.name, p, msg))
		}
	}
}

// sumElems decodes a JSON array of objects, keeping the objects.
func sumElems(b []byte) []map[string]any {
	var raw []any
	if json.Unmarshal(b, &raw) != nil {
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

// sumStrings keeps the non-empty strings of a JSON array.
func sumStrings(v any) []string {
	a, _ := v.([]any)
	var out []string
	for _, e := range a {
		if s, ok := e.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// sumPair is a (scope, canon(param)) pair: scope is "global" for an include
// whose host is "*", else the include pattern.
type sumPair struct{ scope, param string }

func sumComparePairs(a, b sumPair) int {
	if c := strings.Compare(a.scope, b.scope); c != 0 {
		return c
	}
	return strings.Compare(a.param, b.param)
}

// sumIsGlobal reports whether a pattern's host is "*", as the root gates
// define it.
func sumIsGlobal(p string) bool {
	_, rest, _ := strings.Cut(p, "://")
	return strings.HasPrefix(rest, "*/")
}

func sumScope(include string) string {
	if sumIsGlobal(include) {
		return "global"
	}
	return include
}

// sumStrip is a strip file (clean-urls.json, query-filter.json).
type sumStrip struct {
	counts data.Counts
	pairs  map[sumPair]bool
}

func sumParseStrip(b []byte) sumStrip {
	s := sumStrip{counts: data.CountsOf(data.KindStrip, b), pairs: map[sumPair]bool{}}
	for _, m := range sumElems(b) {
		for _, in := range sumStrings(m["include"]) {
			for _, p := range sumStrings(m["params"]) {
				s.pairs[sumPair{sumScope(in), data.Canon(p)}] = true
			}
		}
	}
	return s
}

// union is the set of distinct pairs of both strip files.
func (s *sumSide) union() map[sumPair]bool {
	u := map[sumPair]bool{}
	for _, f := range s.strip {
		for p := range f.pairs {
			u[p] = true
		}
	}
	return u
}

// sumDebRule is one element of debounce.json.
type sumDebRule struct {
	index                 int
	include               []string
	action, param, scheme string
	pref                  string // value, when the key is present
	hasPref, hasTemplate  bool
}

// compiled reports whether linkclean v1 compiles the rule: action
// "redirect" without pref or redirect_url_template.
func (r *sumDebRule) compiled() bool {
	return r.action == "redirect" && !r.hasPref && !r.hasTemplate
}

// uncompiledWhy is the "off" reason for an uncompiled rule, in the style of
// debounce-classes.json.
func (r *sumDebRule) uncompiledWhy() string {
	var parts []string
	if r.action != "redirect" {
		parts = append(parts, fmt.Sprintf("action %q", r.action))
	}
	if r.hasPref {
		parts = append(parts, "pref "+r.pref)
	}
	if r.hasTemplate {
		parts = append(parts, "redirect_url_template")
	}
	return strings.Join(parts, ", ") + " not supported in v1"
}

func sumParseDebounce(b []byte) []sumDebRule {
	var raw []any
	if json.Unmarshal(b, &raw) != nil {
		return nil
	}
	var out []sumDebRule
	for i, v := range raw {
		m, _ := v.(map[string]any)
		r := sumDebRule{index: i, include: sumStrings(m["include"])}
		r.action, _ = m["action"].(string)
		r.param, _ = m["param"].(string)
		r.scheme, _ = m["prepend_scheme"].(string)
		if pv, ok := m["pref"]; ok {
			r.hasPref = true
			r.pref = fmt.Sprint(pv)
		}
		_, r.hasTemplate = m["redirect_url_template"]
		out = append(out, r)
	}
	return out
}

// sumClass is one debounce-classes.json entry.
type sumClass struct {
	Class   string `json:"class"`
	Network string `json:"network"`
}

func sumParseClasses(b []byte) map[string]sumClass {
	var doc struct {
		Patterns map[string]json.RawMessage `json:"patterns"`
	}
	out := map[string]sumClass{}
	if json.Unmarshal(b, &doc) != nil {
		return out
	}
	for p, raw := range doc.Patterns {
		var c sumClass
		_ = json.Unmarshal(raw, &c)
		out[p] = c
	}
	return out
}

// sumPatternHost is the host of a match pattern without a leading "*.".
func sumPatternHost(p string) string {
	return strings.TrimPrefix(data.PatternHost(p), "*.")
}

// sumPatternPath is the path glob of a match pattern ("" without one).
func sumPatternPath(p string) string {
	_, rest, ok := strings.Cut(p, "://")
	if !ok {
		return ""
	}
	_, pth, ok := strings.Cut(rest, "/")
	if !ok {
		return ""
	}
	return "/" + pth
}

// sumNeverStrip is the built-in never-strip list of the rules-risky gate. It
// is a copy of the root module's neverStrip; a test compares the two.
var sumNeverStrip = strings.Fields(`id q query s t p page v u url lang hl start sort list index size type format ref tab
	token k variant sku language code state st key nonce redirect next return i f c g in`)

const (
	sumRiskyParamsPath = "testdata/golden/risky-params.txt"
	sumCorpusDir       = "testdata/corpus/eval-2026-10"
)

// sumRiskyNames maps every risky name to its source: the never-strip list,
// testdata/golden/risky-params.txt and the eval corpus's functional_params,
// all read from the working tree.
func sumRiskyNames(tree fs.FS) (map[string]string, []string) {
	var notes []string
	out := map[string]string{}
	files, _ := fs.Glob(tree, sumCorpusDir+"/*.json")
	for _, f := range files {
		if path.Base(f) == "adjudicated.json" {
			continue
		}
		b, _ := fs.ReadFile(tree, f)
		var items []struct {
			Functional []string `json:"functional_params"`
		}
		if err := json.Unmarshal(b, &items); err != nil {
			notes = append(notes, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		for _, it := range items {
			for _, n := range it.Functional {
				out[data.Canon(n)] = "corpus functional_params"
			}
		}
	}
	if b, err := fs.ReadFile(tree, sumRiskyParamsPath); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			if l := strings.TrimSpace(sc.Text()); l != "" && !strings.HasPrefix(l, "#") {
				out[data.Canon(l)] = "risky-params.txt"
			}
		}
	}
	for _, n := range sumNeverStrip {
		out[n] = "never-strip"
	}
	return out, notes
}

// sumAffiliateGlobs is the §4.4 affiliate-class candidate heuristic, as
// globs over a canonical name ('*' matches any run of characters).
var sumAffiliateGlobs = []string{
	"aff*", "ir*", "impr*", "cjevent", "awc", "sscid", "ran*", "*ref*", "refcode",
	"partner*", "clickid", "camp*", "mk*", "tag", "link*",
}

// sumAffiliateName reports whether a canonical name matches the heuristic.
func sumAffiliateName(name string) bool {
	return slices.ContainsFunc(sumAffiliateGlobs, func(g string) bool { return sumGlob(g, name) })
}

// sumAffiliateHost reports whether any DNS label of host matches the
// heuristic.
func sumAffiliateHost(host string) bool {
	return slices.ContainsFunc(strings.Split(strings.ToLower(host), "."), sumAffiliateName)
}

// sumGlob matches s against a glob whose only metacharacter is '*'.
func sumGlob(g, s string) bool {
	parts := strings.Split(g, "*")
	if len(parts) == 1 {
		return g == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}
