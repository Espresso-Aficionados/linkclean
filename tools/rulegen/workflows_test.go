package main

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestWorkflowSecrets keeps the Pushover credentials, which live in Actions
// secrets, out of reach of anything a pull request or an alert's text
// controls. It is a line scan with no YAML dependency: it understands the
// workflow shapes this repository writes (block mappings, flow collections
// on one key's line, block-scalar run: bodies) and reports anything else it
// cannot place, such as anchors, aliases and tags (at the start of a value
// or anywhere in a flow collection) or run: inside a flow collection, as a
// violation, so an unusual form fails instead of slipping past. The rules,
// one id each:
//
//   - secrets-outside-alert: only rules-alert.yml may mention the secrets
//     context, in any form (secrets.X, secrets['X'], toJSON(secrets));
//     .yml and .yaml files are both workflows.
//   - secrets-inherit, tojson-secrets: never, in any workflow.
//   - secrets-not-step-env, secrets-in-run: secrets appear only in a
//     step-level env: (a key of a jobs.<id>.steps item, the item's "- "
//     line included), never in a workflow- or job-level env:, with:, if: or
//     run: text.
//   - secrets-with-uses: a step holding secrets runs a script, not an
//     action.
//   - run-expression: ${{ }} in run: text (single-line, or any block scalar
//     |, >, |-, >-, |+, >+, with or without an indentation indicator) may
//     reference only matrix.* and github.event_name, with literals and
//     operators.
//   - matrix-expression: no ${{ }} under a job's strategy:, so a matrix
//     holds literals only and matrix.* in run: text inlines nothing a
//     workflow input or event controls.
//   - xtrace: no set -x (or an option cluster with x), bash -x or xtrace in
//     run: or shell: text, quoted (eval "set -x", sh -c 'set -x') or not;
//     xtrace nowhere.
//   - secret-output: in a step holding secrets, a credential variable is
//     referenced only inside the V=$(curl ...) capture, nothing dumps the
//     environment (leading redirections and assignments do not hide the
//     command), curl runs only inside that capture and without -v or
//     --trace, and the response V is used only as
//     echo "$V" | jq ... >/dev/null or grep -q... <<<"$V".
//   - trigger-forbidden: no pull_request_target or workflow_run trigger.
//   - alert-trigger: rules-alert.yml's only trigger is workflow_dispatch.
//   - on-unparsed, yaml-unsupported: a form the scan cannot read.
//
// The package directory is tools/rulegen, so the repository's workflows are
// at ../../.github/workflows. The negatives are testdata/workflows/*.txtar.
func TestWorkflowSecrets(t *testing.T) {
	t.Run("real", func(t *testing.T) {
		dir := filepath.Join("..", "..", ".github", "workflows")
		files := wfReadDir(t, dir)
		for _, name := range []string{"ci.yml", "fuzz.yml", wfAlertFile} {
			if _, ok := files[name]; !ok {
				t.Fatalf("%s: %s missing", dir, name)
			}
		}
		if vs := wfCheck(files); len(vs) > 0 {
			t.Errorf("the repository's workflows break the credential rules:\n\t%s", wfJoin(vs))
		}

		// The scan must understand the real alert workflow: exactly one step
		// holds the credentials, and it is the one that calls curl.
		alert := wfParse(wfAlertFile, files[wfAlertFile])
		var holders []string
		for _, s := range alert.steps() {
			if s.secret {
				holders = append(holders, s.script())
			}
		}
		if len(holders) != 1 || !strings.Contains(holders[0], "curl") {
			t.Errorf("%s: steps holding secrets = %q, want the one pushover step", wfAlertFile, holders)
		}
	})

	// ci.yml's two run: > bodies are scanned: an inputs.body expression on
	// the last line of each is reported there.
	t.Run("real_ci_folded_bodies_scanned", func(t *testing.T) {
		files := wfReadDir(t, filepath.Join("..", "..", ".github", "workflows"))
		ci := wfParse("ci.yml", files["ci.yml"])
		var folded []int // last body line of each run: > body
		for _, s := range ci.steps() {
			for i, l := range s.run {
				if !l.content && strings.TrimSpace(wfStripComment(l.value)) == ">" {
					last := 0
					for _, b := range s.run[i+1:] {
						if strings.TrimSpace(b.text) != "" {
							last = b.n
						}
					}
					folded = append(folded, last)
				}
			}
		}
		if len(folded) != 2 || slices.Contains(folded, 0) {
			t.Fatalf("ci.yml: last lines of the run: > bodies = %v, want two", folded)
		}
		lines := strings.Split(string(files["ci.yml"]), "\n")
		var want []string
		for _, n := range folded {
			lines[n-1] += " ${{ inputs.body }}"
			want = append(want, fmt.Sprintf("ci.yml:%d %s", n, wfRuleRunExpr))
		}
		files["ci.yml"] = []byte(strings.Join(lines, "\n"))
		if got := wfKeys(wfCheck(files)); !slices.Equal(got, want) {
			t.Errorf("got\n\t%s\nwant\n\t%s", strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
		}
	})

	fixtures, err := filepath.Glob(filepath.Join("testdata", "workflows", "*.txtar"))
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, fx := range fixtures {
		name := strings.TrimSuffix(filepath.Base(fx), ".txtar")
		have[name] = true
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(fx)
			if err != nil {
				t.Fatal(err)
			}
			sections := wfTxtar(data)
			wantText, ok := sections["want"]
			if !ok {
				t.Fatal("no -- want -- section")
			}
			delete(sections, "want")
			dir := t.TempDir()
			for p, b := range sections {
				if err := os.WriteFile(filepath.Join(dir, p), b, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var want []string
			for _, w := range strings.Split(string(wantText), "\n") {
				if w = strings.TrimSpace(w); w != "" {
					want = append(want, w)
				}
			}
			slices.Sort(want)
			vs := wfCheck(wfReadDir(t, dir))
			if got := wfKeys(vs); !slices.Equal(got, want) {
				t.Errorf("got\n\t%s\nwant\n\t%s", wfJoin(vs), strings.Join(want, "\n\t"))
			}
		})
	}
	// The negatives the design lists for this test must all be present.
	for _, name := range []string{
		"secrets_in_ci_yml",
		"tojson_secrets",
		"secrets_index_syntax",
		"secrets_inherit",
		"secrets_job_env",
		"secrets_in_run",
		"inputs_body_single_line_run",
		"inputs_body_folded_run",
		"inputs_body_literal_strip_run",
		"set_euxo_pipefail",
		"trigger_pull_request_target",
		"trigger_workflow_run",
		"alert_second_trigger",
		"secrets_in_yaml_workflow",
	} {
		if !have[name] {
			t.Errorf("fixture testdata/workflows/%s.txtar missing", name)
		}
	}
}

const wfAlertFile = "rules-alert.yml"

const (
	wfRuleOutsideAlert = "secrets-outside-alert"
	wfRuleInherit      = "secrets-inherit"
	wfRuleToJSON       = "tojson-secrets"
	wfRuleNotStepEnv   = "secrets-not-step-env"
	wfRuleInRun        = "secrets-in-run"
	wfRuleWithUses     = "secrets-with-uses"
	wfRuleRunExpr      = "run-expression"
	wfRuleMatrixExpr   = "matrix-expression"
	wfRuleXtrace       = "xtrace"
	wfRuleOutput       = "secret-output"
	wfRuleTrigger      = "trigger-forbidden"
	wfRuleAlertTrigger = "alert-trigger"
	wfRuleOn           = "on-unparsed"
	wfRuleYAML         = "yaml-unsupported"
)

type wfViolation struct {
	file string
	line int
	rule string
	msg  string
}

func (v wfViolation) key() string { return fmt.Sprintf("%s:%d %s", v.file, v.line, v.rule) }

// wfKeys returns the sorted, de-duplicated "file:line rule" keys of vs.
func wfKeys(vs []wfViolation) []string {
	var ks []string
	for _, v := range vs {
		ks = append(ks, v.key())
	}
	slices.Sort(ks)
	return slices.Compact(ks)
}

func wfJoin(vs []wfViolation) string {
	var b strings.Builder
	for i, v := range vs {
		if i > 0 {
			b.WriteString("\n\t")
		}
		fmt.Fprintf(&b, "%s: %s", v.key(), v.msg)
	}
	return b.String()
}

// wfReadDir reads every *.yml and *.yaml file directly in dir, keyed by
// base name: the files GitHub runs as workflows.
func wfReadDir(t testing.TB, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, e := range entries {
		if ext := path.Ext(e.Name()); e.IsDir() || ext != ".yml" && ext != ".yaml" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = b
	}
	return files
}

// wfTxtar splits a txtar archive into its files; the leading comment is
// dropped.
func wfTxtar(data []byte) map[string][]byte {
	files := map[string][]byte{}
	var name string
	var cur []byte
	inFile := false
	for line := range bytes.Lines(data) {
		s := strings.TrimRight(string(line), "\r\n")
		if strings.HasPrefix(s, "-- ") && strings.HasSuffix(s, " --") && len(s) > 6 {
			if inFile {
				files[name] = cur
			}
			name, cur, inFile = strings.TrimSpace(s[3:len(s)-3]), nil, true
			continue
		}
		if inFile {
			cur = append(cur, line...)
		}
	}
	if inFile {
		files[name] = cur
	}
	return files
}

// wfNode is one level of a line's place in the document: a mapping key or
// a sequence item.
type wfNode struct {
	col  int    // column of the key, or of the item's "-"
	key  string // "" for a sequence item
	item bool
	open bool // a key with nothing after the colon: its children follow
	line int  // the line the node starts on, which identifies it
}

// wfLine is one line the scan placed. YAML comment lines and blank lines
// outside a value are dropped.
type wfLine struct {
	n       int
	text    string
	path    []wfNode // outermost first; the last node is the key or item the line belongs to
	content bool     // a block-scalar body line or a continuation of the last node's value
	value   string   // on the node's own line: everything after "key:" (or "- "), comment included
}

// own reports whether l is the line its last node starts on.
func (l wfLine) own() bool { return !l.content && len(l.path) > 0 && l.path[len(l.path)-1].line == l.n }

type wfFile struct {
	name  string
	lines []wfLine
	bad   []wfViolation
}

var (
	wfKeyRe     = regexp.MustCompile(`^("(?:[^"\\]|\\.)*"|'(?:[^']|'')*'|[A-Za-z0-9_$][A-Za-z0-9_$.\-]*)[ \t]*:(?:[ \t]+(.*))?$`)
	wfBlockRe   = regexp.MustCompile(`^[|>](?:[1-9][+-]?|[+-][1-9]?)?$`)
	wfSecretsRe = regexp.MustCompile(`(?i)\bsecrets\b`)
	wfInheritRe = regexp.MustCompile(`(?i)\bsecrets\s*:\s*inherit\b`)
	wfToJSONRe  = regexp.MustCompile(`(?i)\btojson\s*\(\s*secrets\s*\)`)
	wfXtraceRe  = regexp.MustCompile(`(?i)\bxtrace\b`)
	wfFlowRunRe = regexp.MustCompile(`[{,]\s*["']?(?:run|shell)["']?\s*:`)
	wfEnvKeyRe  = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)["']?\s*:\s*["']?\$\{\{[^}]*\b(?i:secrets)\b`)
)

func (f *wfFile) unsupported(n int, msg string) {
	f.bad = append(f.bad, wfViolation{f.name, n, wfRuleYAML, msg})
}

// wfParse places every line of a workflow by indentation. A key with a
// value on its line owns the following lines indented deeper than the key
// (a block-scalar body, or a plain or flow value continued); a key with no
// value has children.
func wfParse(name string, src []byte) *wfFile {
	f := &wfFile{name: name}
	var stack []wfNode
	contentCol := -1
	flow := false // the content lines continue a flow collection
	started := false
	for i, raw := range strings.Split(string(src), "\n") {
		n := i + 1
		text := strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimSpace(text)
		indent := len(text) - len(strings.TrimLeft(text, " "))
		if contentCol >= 0 {
			if trimmed == "" || indent > contentCol {
				f.lines = append(f.lines, wfLine{n: n, text: text, path: slices.Clone(stack), content: true})
				if flow && wfFlowProps(wfStripComment(trimmed), true) {
					f.unsupported(n, "anchor, alias or tag in a flow collection")
				}
				continue
			}
			contentCol = -1
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if indent < len(text) && text[indent] == '\t' {
			f.unsupported(n, "tab in indentation")
			continue
		}
		if trimmed == "---" && !started {
			continue
		}
		if trimmed == "---" || trimmed == "..." || strings.HasPrefix(trimmed, "--- ") || strings.HasPrefix(trimmed, "%") {
			f.unsupported(n, "document marker or directive")
			continue
		}
		started = true

		rest, col := text[indent:], indent
		dash := wfIsDash(rest)
		for len(stack) > 0 {
			top := stack[len(stack)-1]
			if top.col < col || top.col == col && dash && !top.item && top.open {
				break
			}
			stack = stack[:len(stack)-1]
		}
		items := 0
		for dash {
			stack = append(stack, wfNode{col: col, item: true, open: true, line: n})
			items++
			rest = rest[1:]
			sp := len(rest) - len(strings.TrimLeft(rest, " "))
			col += 1 + sp
			rest = rest[sp:]
			dash = wfIsDash(rest)
		}
		if rest == "" {
			f.lines = append(f.lines, wfLine{n: n, text: text, path: slices.Clone(stack)})
			continue
		}
		m := wfKeyRe.FindStringSubmatch(rest)
		if m == nil {
			if items == 0 {
				f.unsupported(n, "not a mapping key or a sequence item")
				continue
			}
			// A scalar or flow-collection item.
			stack[len(stack)-1].open = false
			f.lines = append(f.lines, wfLine{n: n, text: text, path: slices.Clone(stack), value: rest})
			v := wfStripComment(rest)
			f.props(n, v)
			contentCol, flow = stack[len(stack)-1].col, wfIsFlow(v)
			continue
		}
		key, value := wfUnquote(m[1]), m[2]
		v := wfStripComment(value)
		stack = append(stack, wfNode{col: col, key: key, open: v == "", line: n})
		f.lines = append(f.lines, wfLine{n: n, text: text, path: slices.Clone(stack), value: value})
		if v != "" {
			f.props(n, v)
			contentCol, flow = col, wfIsFlow(v)
		}
	}
	return f
}

// props reports an anchor, alias or tag in the value v of line n: at its
// start, or starting any node of a flow collection.
func (f *wfFile) props(n int, v string) {
	switch {
	case v != "" && strings.ContainsAny(v[:1], "&*!"):
		f.unsupported(n, "anchor, alias or tag")
	case wfIsFlow(v) && wfFlowProps(v, false):
		f.unsupported(n, "anchor, alias or tag in a flow collection")
	}
}

func wfIsFlow(v string) bool { return v != "" && (v[0] == '{' || v[0] == '[') }

// wfFlowProps reports an &, * or ! starting a node of a flow collection:
// outside a quoted scalar, with nothing but blanks between it and the
// "{", "[", "," or ":" before it (or the start of the line, if start is
// set: a continued collection). Inside a plain scalar (a glob, a
// background job, a && b) they are text.
func wfFlowProps(v string, start bool) bool {
	node := start
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == ' ' || c == '\t':
		case node && (c == '"' || c == '\''):
			j := i + 1
			for ; j < len(v); j++ {
				if c == '"' && v[j] == '\\' {
					j++
				} else if v[j] == c {
					if c == '\'' && j+1 < len(v) && v[j+1] == '\'' {
						j++
						continue
					}
					break
				}
			}
			i, node = j, false
		case node && (c == '&' || c == '*' || c == '!'):
			return true
		default:
			node = strings.IndexByte("{[,:", c) >= 0
		}
	}
	return false
}

func wfIsDash(s string) bool { return s == "-" || strings.HasPrefix(s, "- ") }

func wfUnquote(k string) string {
	if len(k) >= 2 && (k[0] == '"' || k[0] == '\'') && k[len(k)-1] == k[0] {
		return k[1 : len(k)-1]
	}
	return k
}

// wfStripComment drops a trailing YAML comment from a value and trims it.
// Quotes are honoured only where YAML gives them meaning: in a quoted
// scalar or a flow collection.
func wfStripComment(v string) string {
	v = strings.TrimSpace(v)
	quoting := v != "" && strings.ContainsRune(`"'{[`, rune(v[0]))
	var q byte
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case q != 0:
			if c == '\\' && q == '"' {
				i++
			} else if c == q {
				q = 0
			}
		case quoting && (c == '"' || c == '\''):
			q = c
		case c == '#' && (i == 0 || v[i-1] == ' ' || v[i-1] == '\t'):
			return strings.TrimSpace(v[:i])
		}
	}
	return v
}

// wfStep places a line inside a job step: the step item's line, and the
// key directly under the item ("" on the item's own scalar line).
func wfStep(path []wfNode) (step int, key string, ok bool) {
	if len(path) < 4 || path[0].key != "jobs" || path[1].item || path[2].key != "steps" || !path[3].item {
		return 0, "", false
	}
	if len(path) > 4 {
		key = path[4].key
	}
	return path[3].line, key, true
}

type wfStepInfo struct {
	line   int
	secret bool     // its env: mentions secrets
	vars   []string // env names whose value mentions secrets
	uses   int      // line of its uses:, 0 if none
	run    []wfLine // its run: key line and body
}

// script returns the step's run: text with body lines in order; the key
// line contributes its value unless that is a block indicator.
func (s *wfStepInfo) script() string {
	var b strings.Builder
	for _, l := range wfScriptLines(s.run) {
		b.WriteString(l.s)
		b.WriteByte('\n')
	}
	return b.String()
}

func (f *wfFile) steps() []*wfStepInfo {
	byLine := map[int]*wfStepInfo{}
	var order []*wfStepInfo
	for _, l := range f.lines {
		id, key, ok := wfStep(l.path)
		if !ok {
			continue
		}
		s := byLine[id]
		if s == nil {
			s = &wfStepInfo{line: id}
			byLine[id] = s
			order = append(order, s)
		}
		switch key {
		case "env":
			if wfSecretsRe.MatchString(l.text) {
				s.secret = true
				for _, m := range wfEnvKeyRe.FindAllStringSubmatch(l.text, -1) {
					s.vars = append(s.vars, m[1])
				}
			}
		case "uses":
			if l.own() && len(l.path) == 5 {
				s.uses = l.n
			}
		case "run":
			s.run = append(s.run, l)
		}
	}
	return order
}

// wfRunText is the part of a run: line that is script text.
func wfRunText(l wfLine) string {
	if l.content {
		return l.text
	}
	if l.own() {
		return l.value
	}
	return l.text
}

func wfCheck(files map[string][]byte) []wfViolation {
	var out []wfViolation
	for _, name := range slices.Sorted(maps.Keys(files)) {
		f := wfParse(name, files[name])
		out = append(out, f.bad...)
		out = append(out, f.check()...)
	}
	return out
}

func (f *wfFile) check() []wfViolation {
	var out []wfViolation
	add := func(n int, rule, format string, args ...any) {
		out = append(out, wfViolation{f.name, n, rule, fmt.Sprintf(format, args...)})
	}
	for _, l := range f.lines {
		_, key, inStep := wfStep(l.path)
		isRun := inStep && key == "run"
		last := l.path[len(l.path)-1]
		if wfSecretsRe.MatchString(l.text) {
			if f.name != wfAlertFile {
				add(l.n, wfRuleOutsideAlert, "secrets mentioned outside %s", wfAlertFile)
			}
			switch {
			case isRun:
				add(l.n, wfRuleInRun, "secrets in run: text")
			case !inStep || key != "env":
				add(l.n, wfRuleNotStepEnv, "secrets outside a step-level env:")
			}
		}
		if wfInheritRe.MatchString(l.text) {
			add(l.n, wfRuleInherit, "secrets: inherit")
		}
		if wfToJSONRe.MatchString(l.text) {
			add(l.n, wfRuleToJSON, "toJSON(secrets)")
		}
		if wfXtraceRe.MatchString(l.text) {
			add(l.n, wfRuleXtrace, "xtrace")
		}
		if wfInStrategy(l.path) && strings.Contains(l.text, "${{") {
			add(l.n, wfRuleMatrixExpr, "${{ }} under strategy: (matrix values must be literals)")
		}
		switch {
		case isRun:
			text := wfRunText(l)
			for _, e := range wfExprs(text) {
				if e.err != "" {
					add(l.n, wfRuleRunExpr, "%s", e.err)
				} else if !wfExprAllowed(e.expr) {
					add(l.n, wfRuleRunExpr, "${{%s}} in run: text: only matrix.* and github.event_name may be inlined", e.expr)
				}
			}
			if wfTraces(text) {
				add(l.n, wfRuleXtrace, "shell tracing in run: text")
			}
		case last.key == "shell":
			if wfTraces(l.text) {
				add(l.n, wfRuleXtrace, "shell tracing in shell:")
			}
		case wfFlowRunRe.MatchString(l.text):
			add(l.n, wfRuleYAML, "run: or shell: inside a flow collection")
		}
	}

	out = append(out, f.checkOn()...)

	for _, s := range f.steps() {
		if !s.secret {
			continue
		}
		if s.uses != 0 {
			add(s.uses, wfRuleWithUses, "a step holding secrets runs an action")
		}
		for _, v := range wfOutput(s) {
			add(v.line, wfRuleOutput, "%s", v.msg)
		}
	}
	return out
}

// wfInStrategy reports a line under a job's strategy: key, its own line
// included.
func wfInStrategy(path []wfNode) bool {
	return len(path) >= 3 && path[0].key == "jobs" && !path[1].item && path[2].key == "strategy"
}

// checkOn reads the top-level on: key in each form (scalar, flow sequence,
// flow mapping, block mapping, block sequence).
func (f *wfFile) checkOn() []wfViolation {
	var out []wfViolation
	add := func(n int, rule, format string, args ...any) {
		out = append(out, wfViolation{f.name, n, rule, fmt.Sprintf(format, args...)})
	}
	type trigger struct {
		name string
		line int
	}
	var triggers []trigger
	onLine := 0
	var inline strings.Builder
	for _, l := range f.lines {
		if len(l.path) == 0 || l.path[0].key != "on" || l.path[0].item {
			continue
		}
		if l.own() && len(l.path) == 1 {
			if onLine != 0 {
				add(l.n, wfRuleOn, "on: repeated")
			}
			onLine = l.n
			inline.WriteString(wfStripComment(l.value))
			continue
		}
		if len(l.path) == 1 && l.content {
			inline.WriteString(" " + wfStripComment(l.text))
			continue
		}
		if len(l.path) == 2 && l.own() {
			if nd := l.path[1]; nd.item {
				triggers = append(triggers, trigger{wfUnquote(wfStripComment(l.value)), l.n})
			} else {
				triggers = append(triggers, trigger{nd.key, l.n})
			}
		}
	}
	if onLine == 0 {
		return append(out, wfViolation{f.name, 1, wfRuleOn, "no top-level on:"})
	}
	if v := strings.TrimSpace(inline.String()); v != "" {
		if len(triggers) > 0 {
			add(onLine, wfRuleOn, "on: has both a value and children")
		}
		names, ok := wfFlowNames(v)
		if !ok {
			add(onLine, wfRuleOn, "cannot read on: %s", v)
		}
		for _, nm := range names {
			triggers = append(triggers, trigger{nm, onLine})
		}
	}
	if len(triggers) == 0 {
		add(onLine, wfRuleOn, "on: names no trigger")
	}
	for _, tr := range triggers {
		if tr.name == "pull_request_target" || tr.name == "workflow_run" {
			add(tr.line, wfRuleTrigger, "%s trigger", tr.name)
		}
	}
	if f.name == wfAlertFile && (len(triggers) != 1 || triggers[0].name != "workflow_dispatch") {
		add(onLine, wfRuleAlertTrigger, "%s must have workflow_dispatch as its only trigger", wfAlertFile)
	}
	return out
}

// wfFlowNames reads a scalar, a flow sequence of scalars or the top-level
// keys of a flow mapping.
func wfFlowNames(v string) ([]string, bool) {
	if v[0] != '[' && v[0] != '{' {
		if strings.ContainsAny(v, ",:[]{}") {
			return nil, false
		}
		return []string{wfUnquote(v)}, true
	}
	closer := map[byte]byte{'[': ']', '{': '}'}[v[0]]
	if v[len(v)-1] != closer {
		return nil, false
	}
	var names []string
	for _, part := range wfSplitTop(v[1 : len(v)-1]) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if v[0] == '{' {
			k, _, ok := strings.Cut(part, ":")
			if !ok {
				return nil, false
			}
			part = strings.TrimSpace(k)
		}
		if strings.ContainsAny(part, "[]{}:,") {
			return nil, false
		}
		names = append(names, wfUnquote(part))
	}
	return names, true
}

// wfSplitTop splits s at commas outside quotes and brackets.
func wfSplitTop(s string) []string {
	var parts []string
	depth, start := 0, 0
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == '[' || c == '{':
			depth++
		case c == ']' || c == '}':
			depth--
		case c == ',' && depth == 0:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

type wfExpr struct{ expr, err string }

// wfExprs returns every ${{ }} expression in s.
func wfExprs(s string) []wfExpr {
	var out []wfExpr
	for {
		i := strings.Index(s, "${{")
		if i < 0 {
			return out
		}
		j := strings.Index(s[i+3:], "}}")
		if j < 0 {
			return append(out, wfExpr{err: "unterminated ${{ in run: text"})
		}
		out = append(out, wfExpr{expr: s[i+3 : i+3+j]})
		s = s[i+3+j+2:]
	}
}

var (
	wfIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*(?:\.[A-Za-z0-9_*-]+)*`)
	wfNumRe   = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?`)
)

// wfExprAllowed reports whether an expression references only matrix.*
// and github.event_name, with string, number and boolean literals,
// comparison and logical operators and parentheses. Function calls and
// index syntax are refused.
func wfExprAllowed(e string) bool {
	for i := 0; i < len(e); {
		c := e[i]
		switch {
		case c == ' ' || c == '\t' || c == '(' || c == ')':
			i++
		case c == '\'':
			j := i + 1
			for {
				k := strings.IndexByte(e[j:], '\'')
				if k < 0 {
					return false
				}
				j += k + 1
				if j < len(e) && e[j] == '\'' {
					j++
					continue
				}
				break
			}
			i = j
		case strings.HasPrefix(e[i:], "==") || strings.HasPrefix(e[i:], "!=") ||
			strings.HasPrefix(e[i:], "<=") || strings.HasPrefix(e[i:], ">=") ||
			strings.HasPrefix(e[i:], "&&") || strings.HasPrefix(e[i:], "||"):
			i += 2
		case c == '!' || c == '<' || c == '>':
			i++
		case wfNumRe.MatchString(e[i:]):
			i += len(wfNumRe.FindString(e[i:]))
		default:
			id := wfIdentRe.FindString(e[i:])
			if id == "" {
				return false
			}
			i += len(id)
			rest := strings.TrimLeft(e[i:], " \t")
			if strings.HasPrefix(rest, "(") || strings.HasPrefix(rest, "[") {
				return false
			}
			switch {
			case id == "github.event_name", id == "true", id == "false", id == "null":
			case strings.HasPrefix(id, "matrix.") && len(id) > len("matrix."):
			default:
				return false
			}
		}
	}
	return true
}

// wfTraces reports shell tracing: set with an option cluster holding x, or
// a shell (sh, bash, dash, ksh, zsh) started with one. Quote characters
// are dropped from the tokens first, so set -x inside a quoted string run
// by eval or sh -c counts too.
func wfTraces(s string) bool {
	var toks []string
	for _, t := range wfShellTokens(s) {
		if t = strings.Trim(t, `"'`); t != "" {
			toks = append(toks, t)
		}
	}
	for i, t := range toks {
		switch {
		case t == "set":
			for _, a := range toks[i+1:] {
				if a == "--" || wfSeparator(a) {
					break
				}
				if wfXCluster(a) {
					return true
				}
			}
		case slices.Contains([]string{"sh", "bash", "dash", "ksh", "zsh"}, path.Base(t)):
			for _, a := range toks[i+1:] {
				if !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+") {
					break
				}
				if wfXCluster(a) {
					return true
				}
			}
		}
	}
	return false
}

func wfXCluster(a string) bool {
	return strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a, 'x')
}

func wfSeparator(t string) bool { return strings.ContainsAny(t[:1], ";&|()`#") }

// wfShellTokens splits s at blanks, with ; & | ( ) ` and # as tokens of
// their own.
func wfShellTokens(s string) []string {
	var toks []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		case strings.ContainsRune(";&|()`#", r):
			flush()
			toks = append(toks, string(r))
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return toks
}

type wfScriptLine struct {
	n int
	s string
}

// wfScriptLines returns a step's run: text as logical lines, backslash
// continuations joined, each with the line it starts on.
func wfScriptLines(run []wfLine) []wfScriptLine {
	var out []wfScriptLine
	cont := false
	for _, l := range run {
		s := wfRunText(l)
		if l.own() && wfBlockRe.MatchString(wfStripComment(s)) {
			continue
		}
		s = strings.TrimSpace(s)
		if cont {
			out[len(out)-1].s = strings.TrimSuffix(out[len(out)-1].s, `\`) + " " + s
		} else {
			out = append(out, wfScriptLine{l.n, s})
		}
		cont = strings.HasSuffix(s, `\`)
	}
	return out
}

// wfSegments splits a shell line at && || ; and newlines outside quotes
// and $( ), keeping pipelines whole.
func wfSegments(s string) []string {
	var segs []string
	var q byte
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && q != '\'':
			i++
		case q == '\'':
			if c == '\'' {
				q = 0
			}
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			depth++
			i++
		case c == ')' && depth > 0:
			depth--
		case q == '"':
			if c == '"' {
				q = 0
			}
		case c == '\'' || c == '"':
			q = c
		case depth > 0:
		case c == ';' || c == '\n' || (c == '&' || c == '|') && i+1 < len(s) && s[i+1] == c:
			segs = append(segs, s[start:i])
			if c != ';' && c != '\n' {
				i++
			}
			start = i + 1
		}
	}
	return append(segs, s[start:])
}

var (
	wfCurlRe     = regexp.MustCompile(`\bcurl\b`)
	wfCaptureRe  = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=\$\(\s*curl\s(.*)\)$`)
	wfVerboseRe  = regexp.MustCompile(`(?:^|\s)(?:-[A-Za-z]*v[A-Za-z]*|--verbose|--trace(?:-ascii|-config)?)(?:[\s=]|$)`)
	wfKeywordsRe = regexp.MustCompile(`^(?:(?:!|if|then|else|elif|do|while|until|\{|\()\s+)*`)
)

var (
	wfAssignRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	wfRedirectRe = regexp.MustCompile(`^[0-9]*(?:&>>|&>|>>|>\||>&|<&|<<<|<<|<>|>|<)`)
)

// wfCommand drops the leading assignments (A=1, A="x y") and redirections
// (>&2, 2>/dev/null, > file) of a simple command's words, leaving the
// command name first.
func wfCommand(words []string) []string {
	for len(words) > 0 {
		w := words[0]
		switch {
		case wfAssignRe.MatchString(w):
			words = words[1:]
			for q := strings.Count(w, `"`)%2 + strings.Count(w, `'`)%2; q > 0 && len(words) > 0; words = words[1:] {
				q -= strings.Count(words[0], `"`)%2 + strings.Count(words[0], `'`)%2
			}
		case wfRedirectRe.MatchString(w):
			words = words[1:]
			if w == wfRedirectRe.FindString(w) && len(words) > 0 {
				words = words[1:] // the target is the next word
			}
		default:
			return words
		}
	}
	return words
}

// wfOutput applies the secret-output rule to a step that holds secrets.
func wfOutput(s *wfStepInfo) []struct {
	line int
	msg  string
} {
	var out []struct {
		line int
		msg  string
	}
	add := func(n int, msg string) {
		out = append(out, struct {
			line int
			msg  string
		}{n, msg})
	}
	ref := func(seg, v string) bool {
		return regexp.MustCompile(`\$\{?` + regexp.QuoteMeta(v) + `\b`).MatchString(seg)
	}
	var resp []string
	for _, l := range wfScriptLines(s.run) {
		for _, seg := range wfSegments(l.s) {
			seg = strings.TrimSpace(seg)
			if seg == "" {
				continue
			}
			capture := wfCurlRe.MatchString(seg) && wfCaptureRe.MatchString(seg)
			for _, v := range s.vars {
				if ref(seg, v) && !capture {
					add(l.n, "references the credential variable $"+v+" outside the curl capture")
				}
			}
			body := strings.TrimSpace(seg[len(wfKeywordsRe.FindString(seg)):])
			words := wfCommand(strings.Fields(body))
			if len(words) == 0 {
				continue
			}
			cmd := words[0]
			switch {
			case cmd == "printenv" || cmd == "env" || cmd == "set" && len(words) == 1,
				slices.Contains([]string{"declare", "typeset", "export"}, cmd) && slices.Contains(words[1:], "-p"):
				add(l.n, "dumps the environment")
			}
			if wfCurlRe.MatchString(seg) {
				m := wfCaptureRe.FindStringSubmatch(seg)
				if m == nil {
					add(l.n, "curl outside a V=$(curl ...) capture")
				} else {
					resp = append(resp, m[1])
					if wfVerboseRe.MatchString(m[2]) {
						add(l.n, "curl with -v or --trace")
					}
					continue
				}
			}
			for _, v := range resp {
				if !ref(seg, v) {
					continue
				}
				q := regexp.QuoteMeta(v)
				jq := regexp.MustCompile(`^echo\s+"?\$\{?` + q + `\}?"?\s*\|\s*jq\s.*>\s*/dev/null$`)
				grep := regexp.MustCompile(`^!?\s*grep\s+-q[A-Za-z]*\s.*<<<\s*"?\$\{?` + q + `\}?"?$`)
				if !jq.MatchString(seg) && !grep.MatchString(seg) {
					add(l.n, "the response $"+v+" is used outside jq >/dev/null or grep -q")
				}
			}
		}
	}
	return out
}
