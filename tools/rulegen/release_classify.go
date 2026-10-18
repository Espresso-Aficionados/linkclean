package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
)

// relClass is the release class of a path changed since the last tag
// (§11.7 data release, step 3).
type relClass int

const (
	relData  relClass = iota // the vendored rules and the lock; classification-only debounce-classes.json
	relInert                 // cannot change what a consumer's build compiles or embeds
	relDeps                  // go.mod/go.sum with only require versions changed
	relOther                 // anything else: needs a manual release
)

func (c relClass) String() string {
	return [...]string{"data", "inert", "deps", "anything else"}[c]
}

// relPath is one changed path and its class. Note says why a path that is
// data or deps only conditionally fell to relOther.
type relPath struct {
	Path  string
	Class relClass
	Note  string
}

func (p relPath) String() string {
	if p.Note != "" {
		return p.Path + " (" + p.Note + ")"
	}
	return p.Path
}

// relEntry is a debounce classification entry.
type relEntry struct{ Pattern, Class string }

// relChanges classifies every path that differs between the last tag and
// the commit being released.
type relChanges struct {
	Paths       []relPath
	Classified  []relEntry // entries added for patterns new upstream (classification-only diff)
	Declassed   []string   // entries removed for patterns gone upstream
	Deps        []string   // "module old → new", from go.mod
	GoDirective bool       // the go or toolchain directive changed
}

// of returns the paths of class c.
func (ch *relChanges) of(c relClass) []relPath {
	var out []relPath
	for _, p := range ch.Paths {
		if p.Class == c {
			out = append(out, p)
		}
	}
	return out
}

// relStaticClass classifies p by its path alone. go.mod and
// debounce-classes.json come back as their best case (deps, data); the
// caller downgrades them after looking at their diff.
func relStaticClass(p string) relClass {
	switch {
	case strings.HasPrefix(p, "rules/brave/"), p == data.LockPath, p == data.ClassesPath:
		return relData
	// Overlay, golden expectations and the CHANGELOG are never inert,
	// whatever their extension.
	case strings.HasPrefix(p, "rules/overlay/"), strings.HasPrefix(p, "testdata/golden/"), path.Base(p) == "CHANGELOG.md":
		return relOther
	// Go builds no package in a testdata directory at any depth, so a
	// package's own test data (discordlink/testdata/) is inert like the
	// root's; golden-row tracking stays with the root's testdata/golden/.
	// TestReleaseInertNotEmbedded checks that no non-test file embeds an
	// inert path.
	case strings.HasPrefix(p, ".github/"), strings.HasPrefix(p, "tools/"), p == data.GitmodulesPath, p == data.AcksPath,
		strings.HasPrefix(p, "testdata/"), strings.Contains(p, "/testdata/"), strings.HasSuffix(p, "_test.go"), strings.HasSuffix(p, ".md"):
		return relInert
	case p == "go.mod", p == "go.sum":
		return relDeps
	}
	return relOther
}

// relMinorPath reports whether a changed path forces at least a minor
// release (§11.7 manual, step 2; §13): a non-test .go file, the overlay
// (other than a classification-only diff) or a golden expectation.
func relMinorPath(p relPath) bool {
	if p.Class != relOther {
		return false
	}
	return strings.HasSuffix(p.Path, ".go") && !strings.HasSuffix(p.Path, "_test.go") ||
		strings.HasPrefix(p.Path, "rules/overlay/") || strings.HasPrefix(p.Path, "testdata/golden/")
}

// relClassify classifies the diff between base (the last tag's commit, or
// the empty tree when there is none) and head.
func relClassify(ctx context.Context, r repo.Repo, base, head string) (*relChanges, error) {
	names, err := r.DiffNames(ctx, base, head)
	if err != nil {
		return nil, err
	}
	ch := &relChanges{}
	show := func(rev, p string) ([]byte, bool, error) { return r.Show(ctx, rev, p) }
	for _, p := range names {
		rp := relPath{Path: p, Class: relStaticClass(p)}
		switch p {
		case "go.mod":
			oldB, oldOK, err := show(base, p)
			if err != nil {
				return nil, err
			}
			newB, newOK, err := show(head, p)
			if err != nil {
				return nil, err
			}
			d := relGoModDiff(oldB, oldOK, newB, newOK)
			ch.Deps, ch.GoDirective = d.deps, d.goChanged
			if d.note != "" {
				rp.Class, rp.Note = relOther, d.note
			}
		case data.ClassesPath:
			var in [4][]byte
			var ok [4]bool
			for i, src := range [4][2]string{{base, data.ClassesPath}, {head, data.ClassesPath}, {base, relDebouncePath}, {head, relDebouncePath}} {
				if in[i], ok[i], err = show(src[0], src[1]); err != nil {
					return nil, err
				}
			}
			d := relClassesDiff(in[0], ok[0], in[1], ok[1], in[2], ok[2], in[3], ok[3])
			if d.note != "" {
				rp.Class, rp.Note = relOther, d.note
			} else {
				ch.Classified, ch.Declassed = d.added, d.removed
			}
		}
		ch.Paths = append(ch.Paths, rp)
	}
	return ch, nil
}

// relDebouncePath is the vendored debounce list, whose include patterns
// decide whether a debounce-classes.json diff is classification only.
var relDebouncePath = func() string {
	for _, s := range data.Files {
		if s.Kind == data.KindDebounce {
			return s.Vendored
		}
	}
	panic("rulegen: no debounce file in data.Files")
}()

// relGoMod is the outcome of comparing two go.mod files.
type relGoMod struct {
	deps      []string // "module old → new"
	goChanged bool     // go or toolchain directive changed
	note      string   // non-empty when the diff is not require versions only
}

// relGoModDiff allows a go.mod diff that changes nothing but the require
// versions of modules already required (§11.7 deps).
func relGoModDiff(oldB []byte, oldOK bool, newB []byte, newOK bool) relGoMod {
	switch {
	case !oldOK:
		return relGoMod{note: "added", goChanged: true}
	case !newOK:
		return relGoMod{note: "deleted", goChanged: true}
	}
	o, err1 := relParseGoMod(oldB)
	n, err2 := relParseGoMod(newB)
	if err1 != nil || err2 != nil {
		return relGoMod{note: "unparsable: " + fmt.Sprint(relErrorsOr(err1, err2))}
	}
	var d relGoMod
	if o.directive("go") != n.directive("go") || o.directive("toolchain") != n.directive("toolchain") {
		d.goChanged = true
		d.note = "go or toolchain directive changed"
	}
	for _, m := range slices.Sorted(maps.Keys(n.req)) {
		if old, ok := o.req[m]; ok && old.version != n.req[m].version {
			d.deps = append(d.deps, m+" "+old.version+" → "+n.req[m].version)
		}
	}
	if d.note != "" {
		return d
	}
	switch {
	case !slices.Equal(slices.Sorted(maps.Keys(o.req)), slices.Sorted(maps.Keys(n.req))):
		d.note = "required modules added or removed"
	case !slices.Equal(o.other, n.other):
		d.note = "changes other than require versions"
	default:
		for m, r := range n.req {
			if o.req[m].indirect != r.indirect {
				d.note = "changes other than require versions"
			}
		}
	}
	return d
}

func relErrorsOr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

type relRequire struct {
	version  string
	indirect bool
}

// relModFile is a go.mod reduced to what the deps rule compares: require
// entries by module, and every other statement in order with comments
// dropped.
type relModFile struct {
	req   map[string]relRequire
	other []string
}

func (m relModFile) directive(verb string) string {
	for _, s := range m.other {
		if v, ok := strings.CutPrefix(s, verb+" "); ok {
			return v
		}
	}
	return ""
}

// relParseGoMod parses the go.mod syntax: line statements and
// parenthesized blocks, // comments, and quoted tokens.
func relParseGoMod(b []byte) (relModFile, error) {
	m := relModFile{req: map[string]relRequire{}}
	block := ""
	for i, line := range strings.Split(string(b), "\n") {
		toks, comment, err := relGoModLine(line)
		if err != nil {
			return m, fmt.Errorf("go.mod:%d: %w", i+1, err)
		}
		if len(toks) == 0 {
			continue
		}
		if block != "" {
			if len(toks) == 1 && toks[0] == ")" {
				block = ""
				continue
			}
			toks = append([]string{block}, toks...)
		} else if len(toks) == 2 && toks[1] == "(" {
			block = toks[0]
			continue
		}
		if toks[0] == "require" {
			if len(toks) != 3 {
				return m, fmt.Errorf("go.mod:%d: malformed require", i+1)
			}
			if _, dup := m.req[toks[1]]; dup {
				return m, fmt.Errorf("go.mod:%d: %s required twice", i+1, toks[1])
			}
			c := strings.TrimSpace(comment)
			m.req[toks[1]] = relRequire{version: toks[2], indirect: c == "indirect" || strings.HasPrefix(c, "indirect;")}
			continue
		}
		m.other = append(m.other, strings.Join(toks, " "))
	}
	if block != "" {
		return m, fmt.Errorf("go.mod: unterminated %s block", block)
	}
	return m, nil
}

// relGoModLine splits one go.mod line into tokens (quoted strings
// unquoted) and the text of its // comment.
func relGoModLine(line string) (toks []string, comment string, err error) {
	for i := 0; i < len(line); {
		c := line[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case strings.HasPrefix(line[i:], "//"):
			return toks, line[i+2:], nil
		case c == '"' || c == '`':
			j := i + 1
			for j < len(line) && line[j] != c {
				if c == '"' && line[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(line) {
				return nil, "", fmt.Errorf("unterminated string")
			}
			s := line[i : j+1]
			if c == '"' {
				if s, err = strconv.Unquote(s); err != nil {
					return nil, "", err
				}
			} else {
				s = s[1 : len(s)-1]
			}
			toks = append(toks, s)
			i = j + 1
		case c == '(' || c == ')':
			toks = append(toks, string(c))
			i++
		default:
			j := i
			for j < len(line) && !strings.ContainsRune(" \t\r\"`()", rune(line[j])) && !strings.HasPrefix(line[j:], "//") {
				j++
			}
			toks = append(toks, line[i:j])
			i = j
		}
	}
	return toks, "", nil
}

// relClassesOutcome is the outcome of comparing two debounce-classes.json
// files.
type relClassesOutcome struct {
	added   []relEntry
	removed []string
	note    string // non-empty when the diff is not classification only
}

// relClassesDiff allows a debounce-classes.json diff that only adds entries
// for patterns present in head's debounce.json and absent from base's, and
// deletes only entries for patterns absent from head's (§11.7, MA-5). The
// diff is judged as a whole: one disallowed change makes all of it
// anything else.
func relClassesDiff(oldB []byte, oldOK bool, newB []byte, newOK bool, oldDeb []byte, oldDebOK bool, newDeb []byte, newDebOK bool) relClassesOutcome {
	switch {
	case !oldOK:
		return relClassesOutcome{note: "added"}
	case !newOK:
		return relClassesOutcome{note: "deleted"}
	}
	oTop, oPat, err1 := relParseClasses(oldB)
	nTop, nPat, err2 := relParseClasses(newB)
	if err := relErrorsOr(err1, err2); err != nil {
		return relClassesOutcome{note: "unparsable: " + err.Error()}
	}
	oInc, err1 := relIncludes(oldDeb, oldDebOK)
	nInc, err2 := relIncludes(newDeb, newDebOK)
	if err := relErrorsOr(err1, err2); err != nil {
		return relClassesOutcome{note: "debounce list unreadable: " + err.Error()}
	}
	var out relClassesOutcome
	var bad []string
	if !reflect.DeepEqual(oTop, nTop) {
		bad = append(bad, "changes fields other than patterns")
	}
	for _, p := range slices.Sorted(maps.Keys(nPat)) {
		old, had := oPat[p]
		switch {
		case !had && nInc[p] && !oInc[p]:
			out.added = append(out.added, relEntry{Pattern: p, Class: relEntryClass(nPat[p])})
		case !had:
			bad = append(bad, fmt.Sprintf("adds entry %q for a pattern not new in %s", p, relDebouncePath))
		case !reflect.DeepEqual(old, nPat[p]):
			bad = append(bad, fmt.Sprintf("changes existing entry %q", p))
		}
	}
	for _, p := range slices.Sorted(maps.Keys(oPat)) {
		if _, kept := nPat[p]; kept {
			continue
		}
		if nInc[p] {
			bad = append(bad, fmt.Sprintf("removes entry %q for a pattern still in %s", p, relDebouncePath))
		} else {
			out.removed = append(out.removed, p)
		}
	}
	if len(bad) == 0 && len(out.added) == 0 && len(out.removed) == 0 {
		bad = append(bad, "reformatted without adding or removing entries")
	}
	if len(bad) > 0 {
		return relClassesOutcome{note: strings.Join(bad, "; ")}
	}
	return out
}

// relParseClasses decodes debounce-classes.json into its top-level fields
// other than "patterns" and the pattern entries, each as a generic value.
func relParseClasses(b []byte) (top map[string]any, pats map[string]any, err error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, nil, err
	}
	top = map[string]any{}
	for k, v := range raw {
		if k == "patterns" {
			if err := json.Unmarshal(v, &pats); err != nil {
				return nil, nil, fmt.Errorf("patterns: %w", err)
			}
			continue
		}
		var x any
		if err := json.Unmarshal(v, &x); err != nil {
			return nil, nil, err
		}
		top[k] = x
	}
	if pats == nil {
		return nil, nil, fmt.Errorf("no patterns object")
	}
	return top, pats, nil
}

func relEntryClass(v any) string {
	if m, ok := v.(map[string]any); ok {
		if c, ok := m["class"].(string); ok {
			return c
		}
	}
	return "?"
}

// relIncludes returns the include patterns of a debounce list; an absent
// list has none.
func relIncludes(b []byte, ok bool) (map[string]bool, error) {
	set := map[string]bool{}
	if !ok {
		return set, nil
	}
	var rules []struct {
		Include []string `json:"include"`
	}
	if err := json.Unmarshal(b, &rules); err != nil {
		return nil, err
	}
	for _, r := range rules {
		for _, p := range r.Include {
			set[p] = true
		}
	}
	return set, nil
}
