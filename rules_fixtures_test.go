package linkclean

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Espresso-Aficionados/linkclean/internal/query"
)

// The §11.8 fixtures of the root gates. Each is a txtar file in gatesDir,
// run in-process: it starts from the real data and working-tree inputs, with
// base equal to them, and applies its sections in order:
//
//	-- head/<path> --   replace a head file (the embedded set or the tree)
//	-- base/<path> --   replace a base file
//	-- ops --           one operation per line (see fixtureState.op)
//
// ${sha:<path>}, ${short:<path>} (12 hex), ${basesha:<path>} and ${var:<name>}
// expand in file sections and ops. Fixtures assert the reasons gates give,
// not counts, so they survive data updates. gatesDir is in the nested tools
// module, so it is absent from the module zip and the test skips.
// Git-dependent fixtures run in tools/rulegen through `go test`.
const gatesDir = "tools/rulegen/testdata/gates"

// gateFixtures are the §11.8 rows (or their gate parts) the root gates own.
// Each must have a txtar file; extra files in gatesDir run too.
var gateFixtures = []string{
	// Provenance and acks.
	"provenance_no_upstream_dir", "gates_without_base_dir", "license_changed", "notice_stale", "expired_ack_unrelated_pr",
	// Schema.
	"renamed_key", "truncated_json", "debounce_unknown_action",
	// Counts.
	"empty_list", "shrink_16pct", "ack_on_branch_then_superseded", "params_moved_between_files", "debounce_shrink",
	// Holds.
	"hold_on_branch", "hold_on_main", "hold_survives_new_head", "hold_stale_after_merge",
	"hold_lapsed_bytes_changed", "hold_expired", "hold_query_filter_only", "debounce_hold",
	// Compile.
	"invalid_pattern", "ftp_scheme", "qf_index_syntax", "overlay_floating_glob", "overlay_global_regexp_probe",
	"keep_coverage_partial", "keep_coverage_new_scope", "debounce_unclassified_pattern",
	"debounce_uncompiled_action_not_off", "debounce_shared_pattern_uncompiled_rule", "debounce_rule_id_collision",
	"affiliate_network_invalid",
	// Risky.
	"risky_added_to_existing_global", "risky_scoped_new_host", "functional_param_global",
	// Golden.
	"golden_break",
}

func TestRulesFixtures(t *testing.T) {
	if fi, err := os.Stat(gatesDir); err != nil || !fi.IsDir() {
		t.Skipf("%s absent (not in the module zip): %v", gatesDir, err)
	}
	files, err := filepath.Glob(filepath.Join(gatesDir, "*.txtar"))
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]string{}
	for _, f := range files {
		have[strings.TrimSuffix(filepath.Base(f), ".txtar")] = f
	}
	for _, name := range gateFixtures {
		if have[name] == "" {
			t.Errorf("fixture %s: no %s/%s.txtar", name, gatesDir, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(have)) {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(have[name])
			if err != nil {
				t.Fatal(err)
			}
			runGateFixture(t, have[name], data)
		})
	}
}

// gateRecorder is the testing.TB a fixture runs a gate with: it records
// errors and log lines instead of failing. FailNow aborts the gate.
type gateRecorder struct {
	testing.TB // the fixture's test, for everything not overridden
	errs, logs []string
}

type gateAbort struct{}

func (r *gateRecorder) Helper()                   {}
func (r *gateRecorder) Error(args ...any)         { r.errs = append(r.errs, fmt.Sprint(args...)) }
func (r *gateRecorder) Errorf(f string, a ...any) { r.errs = append(r.errs, fmt.Sprintf(f, a...)) }
func (r *gateRecorder) Fatal(args ...any)         { r.Error(args...); r.FailNow() }
func (r *gateRecorder) Fatalf(f string, a ...any) { r.Errorf(f, a...); r.FailNow() }
func (r *gateRecorder) Fail()                     { r.errs = append(r.errs, "(Fail)") }
func (r *gateRecorder) FailNow()                  { panic(gateAbort{}) }
func (r *gateRecorder) Failed() bool              { return len(r.errs) > 0 }
func (r *gateRecorder) Log(args ...any)           { r.logs = append(r.logs, fmt.Sprint(args...)) }
func (r *gateRecorder) Logf(f string, a ...any)   { r.logs = append(r.logs, fmt.Sprintf(f, a...)) }
func (r *gateRecorder) Skip(args ...any)          { r.Log(args...); r.SkipNow() }
func (r *gateRecorder) Skipf(f string, a ...any)  { r.Logf(f, a...); r.SkipNow() }
func (r *gateRecorder) SkipNow()                  { panic(gateAbort{}) }
func (r *gateRecorder) Skipped() bool             { return false }

// recordGate runs one gate check on e and returns what it reported.
func recordGate(t testing.TB, e *gateEnv, check string) *gateRecorder {
	return recordFunc(t, check, func(r testing.TB) { checkRules(r, e, check) })
}

// recordFunc runs f with a recorder; a FailNow in f ends f only.
func recordFunc(t testing.TB, what string, f func(testing.TB)) (r *gateRecorder) {
	r = &gateRecorder{TB: t}
	defer func() {
		if v := recover(); v != nil {
			if _, ok := v.(gateAbort); !ok {
				t.Fatalf("%s panicked: %v\n%s", what, v, debug.Stack())
			}
		}
	}()
	f(r)
	return r
}

// realInputs are the files a fixture starts from: the embedded set plus the
// working-tree files the gates read.
var realInputs = sync.OnceValues(func() (map[string][]byte, error) {
	out := map[string][]byte{}
	add := func(fsys fs.FS, glob string) error {
		m, err := fs.Glob(fsys, glob)
		if err != nil {
			return err
		}
		for _, p := range m {
			b, err := fs.ReadFile(fsys, p)
			if err != nil {
				return err
			}
			out[p] = b
		}
		return nil
	}
	tree := os.DirFS(".")
	for _, g := range []string{"rules/brave/*.json", "rules/overlay/*.json", lockPath} {
		if err := add(rulesFS, g); err != nil {
			return nil, err
		}
	}
	for _, g := range []string{acksPath, "rules/brave/LICENSE", "rules/brave/NOTICE", ".gitmodules",
		"testdata/golden/*", "testdata/keep-coverage-exempt.txt", corpusDir + "/*"} {
		if err := add(tree, g); err != nil {
			return nil, err
		}
	}
	return out, nil
})

// embedded reports whether a path is in the embedded set (defaults.go).
func embedded(p string) bool {
	for _, g := range []string{"rules/brave/*.json", "rules/overlay/*.json"} {
		if ok, _ := path.Match(g, p); ok {
			return true
		}
	}
	return p == lockPath
}

// inBase reports whether `rulegen basedir` writes a path.
func inBase(p string) bool {
	return strings.HasPrefix(p, "rules/") || p == lockPath || p == ".gitmodules" || p == "GITLINK"
}

// fixtureState is a fixture's head and base.
type fixtureState struct {
	t           *testing.T
	file        string
	head, base  map[string][]byte // base nil: no LINKCLEAN_BASE_DIR
	headGitlink string
	now         time.Time
	vars        map[string]string
	last        *gateRecorder
	lastCheck   string
}

func runGateFixture(t *testing.T, file string, data []byte) {
	real, err := realInputs()
	if err != nil {
		t.Fatal(err)
	}
	s := &fixtureState{t: t, file: file, head: cloneFiles(real, nil), vars: map[string]string{},
		now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	s.base = cloneFiles(real, inBase)
	_, sections := parseTxtar(data)
	if len(sections) == 0 {
		t.Fatal("no sections")
	}
	for _, sec := range sections {
		switch {
		case sec.Name == "ops":
			for i, line := range strings.Split(string(sec.Data), "\n") {
				if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
					s.op(fmt.Sprintf("%s:%d", file, sec.Line+1+i), line)
				}
			}
		case strings.HasPrefix(sec.Name, "head/"):
			s.head[strings.TrimPrefix(sec.Name, "head/")] = []byte(s.expand(string(sec.Data)))
		case strings.HasPrefix(sec.Name, "base/"):
			if s.base == nil {
				t.Fatalf("%s:%d: base file with no base", file, sec.Line)
			}
			s.base[strings.TrimPrefix(sec.Name, "base/")] = []byte(s.expand(string(sec.Data)))
		default:
			t.Fatalf("%s:%d: unknown section %q", file, sec.Line, sec.Name)
		}
	}
	if s.last == nil {
		t.Fatal("fixture runs no gate")
	}
}

func cloneFiles(m map[string][]byte, keep func(string) bool) map[string][]byte {
	out := map[string][]byte{}
	for k, v := range m {
		if keep == nil || keep(k) {
			out[k] = slices.Clone(v)
		}
	}
	return out
}

func mapFS(m map[string][]byte, keep func(string) bool) fstest.MapFS {
	out := fstest.MapFS{}
	for k, v := range m {
		if keep == nil || keep(k) {
			out[k] = &fstest.MapFile{Data: v, Mode: 0o644}
		}
	}
	return out
}

// env is the gateEnv of the current state.
func (s *fixtureState) env() *gateEnv {
	e := &gateEnv{data: mapFS(s.head, embedded), tree: mapFS(s.head, nil), headGitlink: s.headGitlink, now: s.now}
	if s.base != nil {
		e.base = mapFS(s.base, nil)
	}
	h := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(s.head)) {
		if embedded(k) {
			fmt.Fprintf(h, "%s %d\n", k, len(s.head[k]))
			h.Write(s.head[k])
		}
	}
	e.key = "fixture:" + fmt.Sprintf("%x", h.Sum(nil))
	return e
}

var varRe = regexp.MustCompile(`\$\{(sha|short|basesha|var):([^}]+)\}`)

func (s *fixtureState) expand(in string) string {
	return varRe.ReplaceAllStringFunc(in, func(m string) string {
		sub := varRe.FindStringSubmatch(m)
		switch sub[1] {
		case "sha", "short":
			b, ok := s.head[sub[2]]
			if !ok {
				s.t.Fatalf("%s: ${%s:%s}: no such head file", s.file, sub[1], sub[2])
			}
			if sub[1] == "short" {
				return sha256Hex(b)[:12]
			}
			return sha256Hex(b)
		case "basesha":
			b, ok := s.base[sub[2]]
			if !ok {
				s.t.Fatalf("%s: ${basesha:%s}: no such base file", s.file, sub[2])
			}
			return sha256Hex(b)
		}
		v, ok := s.vars[sub[2]]
		if !ok {
			s.t.Fatalf("%s: ${var:%s} is not set", s.file, sub[2])
		}
		return v
	})
}

// op runs one ops line:
//
//	run <check>                 run a gate (a gateChecks name or Golden/a|b|c)
//	pass                        the last run reported no error
//	fail <text>                 the last run reported an error containing text
//	nofail <text>               ... no error containing text
//	log <text>                  the last run logged a line containing text
//	logcount <n> <text>         ... exactly n lines containing text
//	nolog <text>                ... no line containing text
//	relock                      re-lock the head's vendored hashes and the NOTICE
//	relock-lock                 re-lock the hashes, leaving the NOTICE stale
//	base none|head|real         no base; base = the head now; base = the real inputs
//	gitlink <sha>               head gitlink; base-gitlink <sha> sets base's GITLINK
//	now <YYYY-MM-DD>            the clock (default 2026-10-16T12:00Z)
//	let <name> <value>          set ${var:name}
//	rm <path>                   remove a head file
//	replace <path> <old> <new>  replace the one occurrence of old (JSON strings)
//	append <path> <text>        append text (a JSON string) to a head file
//	truncate <path> <n>         keep the first n bytes
//	jset|jappend|jremove <path> <jpath> <value>   edit a JSON file
//	jrename <path> <jpath> <old> <new>
//	drop-global <n>             drop n global clean-urls names that query-filter lacks
//	move-global <n>             move n of them to query-filter's global rule
//	drop-debounce <n>           drop n includes from the largest debounce rule
//	clean <config> <url> <want> Clean under head data and config (JSON)
//	rule-ids                    compiled rule IDs are distinct
//	has-rule <id>               a compiled rule has that full ID
//
// A jpath is a JSON array: string keys, integer indexes, and {"key": value}
// objects, which select the first array element where, for every key, the
// element's value equals value or is an array containing it.
func (s *fixtureState) op(where, line string) {
	t := s.t
	line = s.expand(line)
	name, rest, _ := strings.Cut(line, " ")
	rest = strings.TrimSpace(rest)
	fatal := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("%s: %s: %s", where, line, fmt.Sprintf(format, args...))
	}
	need := func() *gateRecorder {
		if s.last == nil {
			fatal("no gate has run")
		}
		return s.last
	}
	dump := func(r *gateRecorder) string {
		return fmt.Sprintf("\n  errors:\n    %s\n  logs:\n    %s", strings.Join(r.errs, "\n    "), strings.Join(r.logs, "\n    "))
	}
	count := func(lines []string, text string) int {
		n := 0
		for _, l := range lines {
			if strings.Contains(l, text) {
				n++
			}
		}
		return n
	}
	switch name {
	case "run":
		s.last, s.lastCheck = recordGate(t, s.env(), rest), rest
	case "pass":
		if r := need(); len(r.errs) > 0 {
			t.Errorf("%s: %s: want pass%s", where, s.lastCheck, dump(r))
		}
	case "fail", "nofail":
		r := need()
		if n := count(r.errs, rest); (n > 0) != (name == "fail") {
			t.Errorf("%s: %s: want %s %q%s", where, s.lastCheck, name, rest, dump(r))
		}
	case "log", "nolog":
		r := need()
		if n := count(r.logs, rest); (n > 0) != (name == "log") {
			t.Errorf("%s: %s: want %s %q%s", where, s.lastCheck, name, rest, dump(r))
		}
	case "logcount":
		r := need()
		ns, text, _ := strings.Cut(rest, " ")
		want, err := strconv.Atoi(ns)
		if err != nil {
			fatal("%v", err)
		}
		if n := count(r.logs, text); n != want {
			t.Errorf("%s: %s: %d log lines contain %q, want %d%s", where, s.lastCheck, n, text, want, dump(r))
		}
	case "relock", "relock-lock":
		s.relock(name == "relock", fatal)
	case "base":
		switch rest {
		case "none":
			s.base = nil
		case "head":
			s.base = cloneFiles(s.head, inBase)
			delete(s.base, "GITLINK")
			if s.headGitlink != "" {
				s.base["GITLINK"] = []byte(s.headGitlink + "\n")
			}
		case "real":
			real, _ := realInputs()
			s.base = cloneFiles(real, inBase)
		default:
			fatal("want none, head or real")
		}
	case "gitlink":
		s.headGitlink = rest
	case "base-gitlink":
		if s.base == nil {
			fatal("no base")
		}
		s.base["GITLINK"] = []byte(rest + "\n")
	case "now":
		d, err := time.Parse(time.DateOnly, rest)
		if err != nil {
			fatal("%v", err)
		}
		s.now = d.Add(12 * time.Hour)
	case "let":
		k, v, _ := strings.Cut(rest, " ")
		s.vars[k] = v
	case "rm":
		if _, ok := s.head[rest]; !ok {
			fatal("no such file")
		}
		delete(s.head, rest)
	case "replace":
		p, args := s.args(rest, 2, fatal)
		old, nw := args[0].(string), args[1].(string)
		if c := bytes.Count(s.file1(p, fatal), []byte(old)); c != 1 {
			fatal("%q occurs %d times", old, c)
		}
		s.head[p] = bytes.Replace(s.head[p], []byte(old), []byte(nw), 1)
	case "append":
		p, args := s.args(rest, 1, fatal)
		s.head[p] = append(s.file1(p, fatal), args[0].(string)...)
	case "truncate":
		p, n, _ := strings.Cut(rest, " ")
		k, err := strconv.Atoi(n)
		if b := s.file1(p, fatal); err != nil || k > len(b) {
			fatal("bad length")
		}
		s.head[p] = s.head[p][:k]
	case "jset", "jappend", "jremove":
		p, args := s.args(rest, 2, fatal)
		s.editJSON(p, args[0], fatal, func(parent any, key any) any {
			cur := jget(parent, key)
			switch name {
			case "jset":
				return args[1]
			case "jappend":
				a, ok := cur.([]any)
				if !ok {
					fatal("not an array")
				}
				return append(a, args[1])
			}
			a, ok := cur.([]any)
			i := slices.IndexFunc(a, func(v any) bool { return jequal(v, args[1]) })
			if !ok || i < 0 {
				fatal("%v not in the array", args[1])
			}
			return slices.Delete(slices.Clone(a), i, i+1)
		})
	case "jrename":
		p, args := s.args(rest, 3, fatal)
		s.editJSON(p, args[0], fatal, func(parent any, key any) any {
			m, ok := jget(parent, key).(map[string]any)
			old, nw := args[1].(string), args[2].(string)
			if _, has := m[old]; !ok || !has {
				fatal("no key %q", old)
			}
			m[nw] = m[old]
			delete(m, old)
			return m
		})
	case "drop-global", "move-global":
		n, err := strconv.Atoi(rest)
		if err != nil {
			fatal("%v", err)
		}
		s.globalNames(n, name == "move-global", fatal)
	case "drop-debounce":
		n, err := strconv.Atoi(rest)
		if err != nil {
			fatal("%v", err)
		}
		s.dropDebounce(n, fatal)
	case "clean":
		_, args := s.args("- "+rest, 3, fatal)
		var cfg Config
		b, _ := json.Marshal(args[0])
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			fatal("config: %v", err)
		}
		srcs, err := s.env().sources()
		if err != nil {
			fatal("%v", err)
		}
		cfg.Sources = srcs
		c, err := New(cfg)
		if err != nil {
			fatal("%v", err)
		}
		if w := c.Warnings(); len(w) > 0 {
			t.Errorf("%s: warnings %v", where, w)
		}
		if got := c.Clean(args[1].(string)); got != args[2].(string) {
			t.Errorf("%s: Clean(%s)\n  got  %s\n  want %s", where, args[1], got, args[2])
		}
	case "rule-ids", "has-rule":
		srcs, err := s.env().sources()
		if err != nil {
			fatal("%v", err)
		}
		seen := map[string]bool{}
		for _, set := range srcs {
			for _, r := range set.Rules {
				id := set.Name + ":" + r.ID
				if seen[id] && name == "rule-ids" {
					t.Errorf("%s: rule ID %s is not unique", where, id)
				}
				seen[id] = true
			}
		}
		if name == "has-rule" && !seen[rest] {
			t.Errorf("%s: no compiled rule %s", where, rest)
		}
	default:
		fatal("unknown op")
	}
}

// file1 returns a head file, which must exist.
func (s *fixtureState) file1(p string, fatal func(string, ...any)) []byte {
	b, ok := s.head[p]
	if !ok {
		fatal("no head file %s", p)
	}
	return b
}

// args splits "<path> <json>..." into the path and n JSON values.
func (s *fixtureState) args(rest string, n int, fatal func(string, ...any)) (string, []any) {
	p, js, _ := strings.Cut(rest, " ")
	dec := json.NewDecoder(strings.NewReader(js))
	dec.UseNumber()
	var out []any
	for range n {
		var v any
		if err := dec.Decode(&v); err != nil {
			fatal("argument %d: %v", len(out)+1, err)
		}
		out = append(out, v)
	}
	if dec.More() {
		fatal("more than %d arguments", n)
	}
	return p, out
}

// editJSON replaces the value at jpath in head file p with f(parent, key).
func (s *fixtureState) editJSON(p string, jpath any, fatal func(string, ...any), f func(parent, key any) any) {
	var doc any
	dec := json.NewDecoder(bytes.NewReader(s.file1(p, fatal)))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		fatal("%s: %v", p, err)
	}
	segs, ok := jpath.([]any)
	if !ok {
		fatal("jpath %v is not an array", jpath)
	}
	root := []any{doc}
	parent, key := any(root), any(json.Number("0"))
	for _, seg := range segs {
		parent, key = jget(parent, key), jkey(jget(parent, key), seg, fatal)
	}
	jput(parent, key, f(parent, key))
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root[0]); err != nil {
		fatal("%v", err)
	}
	s.head[p] = buf.Bytes()
}

// jkey resolves one jpath segment against v to a map key or array index.
func jkey(v, seg any, fatal func(string, ...any)) any {
	switch sel := seg.(type) {
	case string:
		if _, ok := v.(map[string]any); !ok {
			fatal("key %q on a non-object", sel)
		}
		return sel
	case json.Number:
		a, ok := v.([]any)
		i, err := strconv.Atoi(string(sel))
		if !ok || err != nil || i < 0 || i >= len(a) {
			fatal("index %v out of range", sel)
		}
		return sel
	case map[string]any:
		a, ok := v.([]any)
		if !ok || len(sel) == 0 {
			fatal("selector %v needs an array and a key", sel)
		}
		for i, e := range a {
			m, _ := e.(map[string]any)
			all := true
			for k, want := range sel {
				got := m[k]
				ga, _ := got.([]any)
				if !jequal(got, want) && !slices.ContainsFunc(ga, func(x any) bool { return jequal(x, want) }) {
					all = false
				}
			}
			if all {
				return json.Number(strconv.Itoa(i))
			}
		}
		fatal("selector %v matches nothing", sel)
	}
	fatal("bad jpath segment %v", seg)
	return nil
}

func jget(v, key any) any {
	switch k := key.(type) {
	case string:
		return v.(map[string]any)[k]
	case json.Number:
		i, _ := strconv.Atoi(string(k))
		return v.([]any)[i]
	}
	return nil
}

func jput(v, key, val any) {
	switch k := key.(type) {
	case string:
		v.(map[string]any)[k] = val
	case json.Number:
		i, _ := strconv.Atoi(string(k))
		v.([]any)[i] = val
	}
}

func jequal(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

var noticeFileRe = regexp.MustCompile(`(?m)^\S+ [0-9a-f]{40} [0-9a-f]{64}$`)

// relock rewrites the head lock's hashes for the head bytes (what `rulegen
// sync` would write for them) and, with notice, the NOTICE's.
func (s *fixtureState) relock(notice bool, fatal func(string, ...any)) {
	lock, err := readLock(mapFS(s.head, embedded))
	if err != nil {
		fatal("%v", err)
	}
	for up, lf := range lock.Upstream.Files {
		if b, ok := s.head[lf.Vendored]; ok {
			lf.GitBlobSHA1, lf.SHA256 = gitBlobSHA1(b), sha256Hex(b)
			lock.Upstream.Files[up] = lf
		}
	}
	// The NOTICE lists "<upstream path> <blob> <sha256>" per file.
	n := noticeFileRe.ReplaceAllStringFunc(string(s.head["rules/brave/NOTICE"]), func(l string) string {
		up, _, _ := strings.Cut(l, " ")
		if lf, ok := lock.Upstream.Files[up]; ok {
			return up + " " + lf.GitBlobSHA1 + " " + lf.SHA256
		}
		return l
	})
	out, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		fatal("%v", err)
	}
	s.head[lockPath] = append(out, '\n')
	if notice {
		s.head["rules/brave/NOTICE"] = []byte(n)
	}
}

// globalNames drops (or, with move, moves to query-filter's first global
// rule) the n smallest canonical names of clean-urls' global rules that
// query-filter's global rules lack: a change confined to the union's global
// pairs (drop) or to neither union count (move).
func (s *fixtureState) globalNames(n int, move bool, fatal func(string, ...any)) {
	const cu, qf = "rules/brave/clean-urls.json", "rules/brave/query-filter.json"
	var cuDoc, qfDoc []any
	for p, d := range map[string]*[]any{cu: &cuDoc, qf: &qfDoc} {
		dec := json.NewDecoder(bytes.NewReader(s.file1(p, fatal)))
		dec.UseNumber()
		if err := dec.Decode(d); err != nil {
			fatal("%s: %v", p, err)
		}
	}
	global := func(e any) (map[string]any, bool) {
		m, _ := e.(map[string]any)
		return m, slices.ContainsFunc(lenientStrings(m["include"]), isGlobalPattern)
	}
	inQF := map[string]bool{}
	var qfGlobal map[string]any
	for _, e := range qfDoc {
		if m, ok := global(e); ok {
			if qfGlobal == nil {
				qfGlobal = m
			}
			for _, p := range lenientStrings(m["params"]) {
				inQF[query.Canon(p)] = true
			}
		}
	}
	names := map[string]bool{}
	for _, e := range cuDoc {
		if m, ok := global(e); ok {
			for _, p := range lenientStrings(m["params"]) {
				if !inQF[query.Canon(p)] {
					names[query.Canon(p)] = true
				}
			}
		}
	}
	pick := slices.Sorted(maps.Keys(names))
	if len(pick) < n || (move && qfGlobal == nil) {
		fatal("only %d candidate names", len(pick))
	}
	pick = pick[:n]
	for _, e := range cuDoc {
		if m, ok := global(e); ok {
			var keep []any
			for _, p := range m["params"].([]any) {
				if ps, _ := p.(string); !slices.Contains(pick, query.Canon(ps)) {
					keep = append(keep, p)
				}
			}
			m["params"] = keep
		}
	}
	if move {
		for _, p := range pick {
			qfGlobal["params"] = append(qfGlobal["params"].([]any), p)
		}
	}
	for p, d := range map[string][]any{cu: cuDoc, qf: qfDoc} {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(d); err != nil {
			fatal("%v", err)
		}
		s.head[p] = buf.Bytes()
	}
}

// dropDebounce removes the last n includes of the debounce rule with the most.
func (s *fixtureState) dropDebounce(n int, fatal func(string, ...any)) {
	const p = "rules/brave/debounce.json"
	var doc []any
	dec := json.NewDecoder(bytes.NewReader(s.file1(p, fatal)))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		fatal("%v", err)
	}
	var big map[string]any
	for _, e := range doc {
		if m, _ := e.(map[string]any); big == nil || len(lenientStrings(m["include"])) > len(lenientStrings(big["include"])) {
			big = m
		}
	}
	inc, _ := big["include"].([]any)
	if len(inc) <= n {
		fatal("largest debounce rule has %d includes", len(inc))
	}
	big["include"] = inc[:len(inc)-n]
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		fatal("%v", err)
	}
	s.head[p] = buf.Bytes()
}

// TestProbeBuiltinGlobalRegexps: the global ParamRegexps probe covers the
// built-in sets as well as the overlay (§11.3 "overlay or built-in"). Brave's
// compilers emit no ParamRegexps today, so this runs on a synthetic set.
func TestProbeBuiltinGlobalRegexps(t *testing.T) {
	for _, set := range []string{"brave/clean-urls", "brave/debounce", overlaySet} {
		r := &gateRecorder{TB: t}
		srcs := []RuleSet{{Name: set, Rules: []Rule{
			{ID: "scoped", Action: Strip, Include: []string{"*://*.example.com/*"}, ParamRegexps: []string{"p.*"}},
			{ID: "global", Action: Strip, Include: []string{"*://*/*"}, ParamRegexps: []string{"ps.*"}},
		}}}
		probeGlobalRegexps(r, srcs, map[string]bool{"psc": true})
		want := []string{set + `:global: global paramRegexp "ps.*" matches corpus functional name "psc"`}
		if !slices.Equal(r.errs, want) {
			t.Errorf("%s: got %q, want %q", set, r.errs, want)
		}
	}
}

// TestGateRecorder: a gate's Fatal aborts only that gate, and is recorded.
func TestGateRecorder(t *testing.T) {
	e := &gateEnv{data: fstest.MapFS{}, tree: fstest.MapFS{}, now: time.Now(), key: "empty"}
	r := recordGate(t, e, "Provenance")
	if len(r.errs) != 1 || !strings.Contains(r.errs[0], lockPath) {
		t.Errorf("errs = %q", r.errs)
	}
	r = recordGate(t, e, "Nope")
	if len(r.errs) != 1 || !strings.Contains(r.errs[0], `unknown check "Nope"`) {
		t.Errorf("errs = %q", r.errs)
	}
}
