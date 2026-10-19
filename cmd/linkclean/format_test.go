package main

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Espresso-Aficionados/linkclean"
)

// hostile holds ESC, U+009B (C1), U+202E (bidi override), an invalid byte
// and a quote; written with byte escapes only.
const hostile = "a\x1b\xc2\x9b\xe2\x80\xae\xff\"b"

// hostileQ is strconv.Quote(hostile).
const hostileQ = `"a\x1b\u009b\u202e\xff\"b"`

// fullResult has every line kind and every optional field of a record.
func fullResult() linkclean.Result {
	return linkclean.Result{
		Input:           "https://in.example/",
		URL:             "https://example.com/?a=1&b=2",
		Changed:         true,
		UnwrapAbandoned: true,
		Signed:          true,
		Hops: []linkclean.Hop{
			{From: "f", To: "t", Rule: "set:hop1", Category: linkclean.CatRedirectUnwrapped, FromHost: "w.example"},
			{Rule: "set:hop2", Category: linkclean.CatAffiliate, FromHost: "aff.example",
				Beneficiary: linkclean.Beneficiary{Network: "Net", ID: "id-1"}},
			{Rule: "unshorten:x", Category: linkclean.CatUnshortened, FromHost: "s.example"},
			{Rule: "set:hop4", Category: linkclean.CatAffiliate, FromHost: "aff.example"},
		},
		Removed: []linkclean.Removal{
			{Part: linkclean.PartPath, Category: linkclean.CatPathSegment, Name: "/ref", Rule: "set:p"},
			{Part: linkclean.PartQuery, Category: linkclean.CatTracker, Name: "utm_source", Rule: "set:q"},
			{Part: linkclean.PartFragment, Category: linkclean.CatTracker, Name: "fbclid", Rule: "set:f"},
			{Part: linkclean.PartNested, Category: linkclean.CatTracker, Name: "gclid", NestedIn: "u", Rule: "set:n"},
			{Part: linkclean.PartQuery, Category: linkclean.CatAffiliate, Name: "tag", Rule: "set:a",
				Beneficiary: linkclean.Beneficiary{Network: "Amazon Associates", ID: "foo-20"}},
			{Part: linkclean.PartNested, Category: linkclean.CatAffiliate, Name: "tag", Rule: "set:a2"},
		},
		Kept: []linkclean.Kept{
			{Name: "tag", Reason: linkclean.KeptAffiliate, Rule: "set:k"},
			{Name: "x", Reason: linkclean.KeptByRule, Rule: "set:r"},
			{Name: "sig", Reason: linkclean.KeptSignedGuard},
		},
	}
}

// hostileResult carries hostile in every displayed string field.
func hostileResult() linkclean.Result {
	b := linkclean.Beneficiary{Network: hostile, ID: hostile}
	return linkclean.Result{
		URL:     hostile,
		Changed: true,
		Hops:    []linkclean.Hop{{Rule: hostile, Category: linkclean.CatAffiliate, FromHost: hostile, Beneficiary: b}},
		Removed: []linkclean.Removal{{Part: linkclean.PartNested, Category: linkclean.CatAffiliate, Name: hostile, NestedIn: hostile, Rule: hostile, Beneficiary: b}},
		Kept:    []linkclean.Kept{{Name: hostile, Reason: linkclean.KeptByRule, Rule: hostile}},
	}
}

func TestWriteExplain(t *testing.T) {
	tests := []struct {
		name string
		n    int
		r    linkclean.Result
		want string
	}{
		{"minimal", 1, linkclean.Result{URL: "https://a.example/"}, "line 1\nout \"https://a.example/\"\n\n"},
		{"full", 7, fullResult(), `line 7
out "https://example.com/?a=1&b=2"
changed
abandoned
signed
hop category=redirect rule="set:hop1" host="w.example"
hop category=affiliate rule="set:hop2" host="aff.example" network="Net" id="id-1"
hop category=unshortened rule="unshorten:x" host="s.example"
hop category=affiliate rule="set:hop4" host="aff.example" network="" id=""
removed part=path category=path name="/ref" rule="set:p"
removed part=query category=tracker name="utm_source" rule="set:q"
removed part=fragment category=tracker name="fbclid" rule="set:f"
removed part=nested category=tracker name="gclid" in="u" rule="set:n"
removed part=query category=affiliate name="tag" rule="set:a" network="Amazon Associates" id="foo-20"
removed part=nested category=affiliate name="tag" in="" rule="set:a2" network="" id=""
kept reason=affiliate name="tag" rule="set:k"
kept reason=rule name="x" rule="set:r"
kept reason=signed name="sig"

`},
		{"unknown", 2, linkclean.Result{
			URL:     "u",
			Skipped: 99,
			Hops:    []linkclean.Hop{{Rule: "r", FromHost: "h"}},
			Removed: []linkclean.Removal{{Part: 99, Category: 99, Name: "n", Rule: "r"}},
			Kept:    []linkclean.Kept{{Name: "k", Rule: "r"}},
		}, `line 2
out "u"
skipped unknown
hop category=unknown rule="r" host="h"
removed part=unknown category=unknown name="n" rule="r"
kept reason=unknown name="k" rule="r"

`},
		{"hostile", 3, hostileResult(), strings.ReplaceAll(`line 3
out Q
changed
hop category=affiliate rule=Q host=Q network=Q id=Q
removed part=nested category=affiliate name=Q in=Q rule=Q network=Q id=Q
kept reason=rule name=Q rule=Q

`, "Q", hostileQ)},
	}
	for skip, w := range map[linkclean.SkipReason]string{
		linkclean.SkipScheme: "scheme", linkclean.SkipUnparseable: "unparseable", linkclean.SkipAmbiguous: "ambiguous",
		linkclean.SkipTooLong: "toolong", linkclean.SkipArchive: "archive", linkclean.SkipRoundCap: "roundcap",
	} {
		tests = append(tests, struct {
			name string
			n    int
			r    linkclean.Result
			want string
		}{"skip " + w, 1, linkclean.Result{URL: "x", Input: "x", Skipped: skip}, "line 1\nout \"x\"\nskipped " + w + "\n\n"})
	}
	for _, tt := range tests {
		got := string(appendExplain([]byte("prefix"), tt.n, tt.r))
		if got != "prefix"+tt.want {
			t.Errorf("%s:\n got %q\nwant %q", tt.name, got, "prefix"+tt.want)
		}
		checkTerminalSafe(t, tt.name, []byte(got))
	}
	if got, want := string(appendExplainError(nil, 4, errLineTooLong)), "line 4\nerror \"line too long\"\n\n"; got != want {
		t.Errorf("error record = %q, want %q", got, want)
	}
}

// checkTerminalSafe fails unless b is valid UTF-8 whose only control
// character is '\n' and which has no other non-printable rune (C1, bidi and
// format characters, DEL).
func checkTerminalSafe(t *testing.T, name string, b []byte) {
	t.Helper()
	if !utf8.Valid(b) {
		t.Errorf("%s: output is not valid UTF-8: %q", name, b)
		return
	}
	for i, r := range string(b) {
		if r != '\n' && !strconv.IsPrint(r) {
			t.Errorf("%s: raw non-printable %U at byte %d: %q", name, r, i, b)
			return
		}
	}
}

func TestNoValuesEchoed(t *testing.T) {
	const secret = "SECRETVALUE"
	r := linkclean.Result{
		Input:   "https://in.example/?utm=" + secret,
		URL:     "https://out.example/",
		Changed: true,
		Hops: []linkclean.Hop{{From: "https://w.example/?u=" + secret, To: "https://t.example/?" + secret,
			Rule: "set:h", Category: linkclean.CatAffiliate, FromHost: "w.example",
			Beneficiary: linkclean.Beneficiary{Network: "NETWORK1", ID: "AFFID1"}}},
		Removed: []linkclean.Removal{
			{Part: linkclean.PartQuery, Category: linkclean.CatAffiliate, Name: "tag", Value: secret, Text: "tag=" + secret,
				Rule: "set:a", Beneficiary: linkclean.Beneficiary{Network: "NETWORK2", ID: "AFFID2"}},
			{Part: linkclean.PartQuery, Category: linkclean.CatTracker, Name: "utm", Value: secret, Text: "utm=" + secret,
				Rule: "set:t", Beneficiary: linkclean.Beneficiary{Network: "STRAYNET", ID: "STRAYID"}},
		},
	}
	ex := appendExplain(nil, 1, r)
	js, ok, err := appendJSON(nil, 1, r)
	if !ok || err != nil {
		t.Fatalf("appendJSON: ok %v, err %v", ok, err)
	}
	for name, out := range map[string]string{"explain": string(ex), "json": string(js)} {
		for _, bad := range []string{secret, "in.example", "STRAYNET", "STRAYID"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s output contains %q:\n%s", name, bad, out)
			}
		}
		for _, want := range []string{"NETWORK1", "AFFID1", "NETWORK2", "AFFID2", "out.example"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s output lacks %q:\n%s", name, want, out)
			}
		}
	}
}

// The test-local copy of the -json schema.
type (
	tRecord struct {
		Line      int        `json:"line"`
		Error     string     `json:"error"`
		URL       string     `json:"url"`
		Changed   bool       `json:"changed"`
		Skipped   string     `json:"skipped"`
		Abandoned bool       `json:"abandoned"`
		Signed    bool       `json:"signed"`
		Hops      []tHop     `json:"hops"`
		Removed   []tRemoval `json:"removed"`
		Kept      []tKept    `json:"kept"`
	}
	tHop struct {
		Category string `json:"category"`
		Rule     string `json:"rule"`
		FromHost string `json:"fromHost"`
		Network  string `json:"network"`
		ID       string `json:"id"`
	}
	tRemoval struct {
		Part     string `json:"part"`
		Category string `json:"category"`
		Name     string `json:"name"`
		NestedIn string `json:"nestedIn"`
		Rule     string `json:"rule"`
		Network  string `json:"network"`
		ID       string `json:"id"`
	}
	tKept struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
		Rule   string `json:"rule"`
	}
	tError struct {
		Line  int    `json:"line"`
		Error string `json:"error"`
	}
)

var (
	recordKeys  = []string{"line", "error", "url", "changed", "skipped", "abandoned", "signed", "hops", "removed", "kept"}
	hopKeys     = []string{"category", "rule", "fromHost", "network", "id"}
	removalKeys = []string{"part", "category", "name", "nestedIn", "rule", "network", "id"}
	keptKeys    = []string{"name", "reason", "rule"}
	errorKeys   = []string{"line", "error"}
)

// objectKeys returns the keys of the JSON object raw, in order, and the raw
// value of each.
func objectKeys(t *testing.T, raw []byte) ([]string, map[string]json.RawMessage) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %s (%v)", raw, err)
	}
	var keys []string
	vals := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		k := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
		vals[k] = v
	}
	return keys, vals
}

// checkArray checks that raw is a JSON array (never null) whose objects
// have exactly keys, in order.
func checkArray(t *testing.T, name string, raw json.RawMessage, keys []string) {
	t.Helper()
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil || elems == nil {
		t.Fatalf("%s: %s is not an array (%v)", name, raw, err)
	}
	for _, e := range elems {
		if got, _ := objectKeys(t, e); !slices.Equal(got, keys) {
			t.Errorf("%s keys = %q, want %q", name, got, keys)
		}
	}
}

// decodeStrict decodes line into v, rejecting unknown fields and trailing
// data.
func decodeStrict(t *testing.T, line []byte, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("decoding %s: %v", line, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		t.Fatalf("trailing data after %s", line)
	}
}

func TestJSONSchema(t *testing.T) {
	// U+009B, U+202E and U+E0001 (a non-BMP format character), byte-escaped.
	const odd = "\xc2\x9b\xe2\x80\xae\xf3\xa0\x80\x81"
	oddResult := linkclean.Result{
		URL: "https://example.com/" + odd, Changed: true,
		Removed: []linkclean.Removal{{Part: linkclean.PartQuery, Category: linkclean.CatTracker, Name: "n" + odd, Rule: "set:q"}},
	}
	results := map[string]linkclean.Result{
		"empty":   {URL: "https://example.com/?a=1&b=<2>", Skipped: linkclean.SkipArchive},
		"full":    fullResult(),
		"odd":     oddResult,
		"hostile": hostileResult(),
	}
	for name, r := range results {
		if name == "hostile" {
			r.URL = "https://example.com/" // the url check is TestJSONInvalidUTF8's
		}
		line, ok, err := appendJSON(nil, 5, r)
		if !ok || err != nil {
			t.Fatalf("%s: ok %v, err %v", name, ok, err)
		}
		if bytes.Count(line, []byte("\n")) != 1 || line[len(line)-1] != '\n' {
			t.Errorf("%s: not one line: %q", name, line)
		}
		if bytes.Contains(line, []byte("null")) {
			t.Errorf("%s: null in %s", name, line)
		}
		checkTerminalSafe(t, name, line)
		keys, vals := objectKeys(t, line)
		if !slices.Equal(keys, recordKeys) {
			t.Errorf("%s: record keys = %q, want %q", name, keys, recordKeys)
		}
		checkArray(t, name+" hops", vals["hops"], hopKeys)
		checkArray(t, name+" removed", vals["removed"], removalKeys)
		checkArray(t, name+" kept", vals["kept"], keptKeys)

		var rec tRecord
		decodeStrict(t, line, &rec)
		if rec.Line != 5 || rec.Error != "" || rec.URL != r.URL || rec.Changed != r.Changed ||
			len(rec.Hops) != len(r.Hops) || len(rec.Removed) != len(r.Removed) || len(rec.Kept) != len(r.Kept) {
			t.Errorf("%s: decoded %+v from %+v", name, rec, r)
		}
		again, err := appendEncoded(nil, rec)
		if err != nil || !bytes.Equal(again, line) {
			t.Errorf("%s: re-encoded\n%s\nwant\n%s", name, again, line)
		}
	}

	line, _, _ := appendJSON(nil, 1, results["empty"])
	if want := `{"line":1,"error":"","url":"https://example.com/?a=1&b=<2>","changed":false,"skipped":"archive","abandoned":false,"signed":false,"hops":[],"removed":[],"kept":[]}` + "\n"; string(line) != want {
		t.Errorf("empty record:\n got %s\nwant %s", line, want)
	}
	line, _, _ = appendJSON(nil, 7, fullResult())
	if want := `{"line":7,"error":"","url":"https://example.com/?a=1&b=2","changed":true,"skipped":"","abandoned":true,"signed":true,` +
		`"hops":[{"category":"redirect","rule":"set:hop1","fromHost":"w.example","network":"","id":""},` +
		`{"category":"affiliate","rule":"set:hop2","fromHost":"aff.example","network":"Net","id":"id-1"},` +
		`{"category":"unshortened","rule":"unshorten:x","fromHost":"s.example","network":"","id":""},` +
		`{"category":"affiliate","rule":"set:hop4","fromHost":"aff.example","network":"","id":""}],` +
		`"removed":[{"part":"path","category":"path","name":"/ref","nestedIn":"","rule":"set:p","network":"","id":""},` +
		`{"part":"query","category":"tracker","name":"utm_source","nestedIn":"","rule":"set:q","network":"","id":""},` +
		`{"part":"fragment","category":"tracker","name":"fbclid","nestedIn":"","rule":"set:f","network":"","id":""},` +
		`{"part":"nested","category":"tracker","name":"gclid","nestedIn":"u","rule":"set:n","network":"","id":""},` +
		`{"part":"query","category":"affiliate","name":"tag","nestedIn":"","rule":"set:a","network":"Amazon Associates","id":"foo-20"},` +
		`{"part":"nested","category":"affiliate","name":"tag","nestedIn":"","rule":"set:a2","network":"","id":""}],` +
		`"kept":[{"name":"tag","reason":"affiliate","rule":"set:k"},{"name":"x","reason":"rule","rule":"set:r"},` +
		`{"name":"sig","reason":"signed","rule":""}]}` + "\n"; string(line) != want {
		t.Errorf("full record:\n got %s\nwant %s", line, want)
	}

	line, _, _ = appendJSON(nil, 1, oddResult)
	for _, esc := range []string{`/\u009b\u202e\udb40\udc01"`, `"n\u009b\u202e\udb40\udc01"`} {
		if !bytes.Contains(line, []byte(esc)) {
			t.Errorf("odd record lacks %s: %s", esc, line)
		}
	}

	line = appendJSONError(nil, 9, errLineTooLong)
	if want := `{"line":9,"error":"line too long"}` + "\n"; string(line) != want {
		t.Errorf("error object = %s, want %s", line, want)
	}
	keys, _ := objectKeys(t, line)
	if !slices.Equal(keys, errorKeys) {
		t.Errorf("error keys = %q, want %q", keys, errorKeys)
	}
	var e tError
	decodeStrict(t, line, &e)
	if e != (tError{9, errLineTooLong}) {
		t.Errorf("decoded error object %+v", e)
	}
}

func TestJSONInvalidUTF8(t *testing.T) {
	line, ok, err := appendJSON(nil, 2, linkclean.Result{URL: "https://example.com/\xff", Changed: true})
	if ok || err != nil || string(line) != `{"line":2,"error":"not valid UTF-8"}`+"\n" {
		t.Errorf("invalid url: ok %v, err %v, line %s", ok, err, line)
	}

	b := linkclean.Beneficiary{Network: "N\xfe", ID: "\xff"}
	rec, ok := toRecord(3, linkclean.Result{
		URL:     "https://example.com/",
		Hops:    []linkclean.Hop{{Rule: "r\x80", Category: linkclean.CatAffiliate, FromHost: "h\xc2", Beneficiary: b}},
		Removed: []linkclean.Removal{{Part: linkclean.PartNested, Category: linkclean.CatAffiliate, Name: "a\xffb", NestedIn: "u\xe2\x80", Rule: "r", Beneficiary: b}},
		Kept:    []linkclean.Kept{{Name: "k\xff", Reason: linkclean.KeptByRule, Rule: "r\xff"}},
	})
	if !ok {
		t.Fatal("toRecord rejected a valid url")
	}
	wantHop := jsonHop{Category: "affiliate", Rule: "r%80", FromHost: "h%C2", Network: "N%FE", ID: "%FF"}
	wantRem := jsonRemoval{Part: "nested", Category: "affiliate", Name: "a%FFb", NestedIn: "u%E2%80", Rule: "r", Network: "N%FE", ID: "%FF"}
	wantKept := jsonKept{Name: "k%FF", Reason: "rule", Rule: "r%FF"}
	if rec.Hops[0] != wantHop || rec.Removed[0] != wantRem || rec.Kept[0] != wantKept {
		t.Errorf("toRecord = %+v", rec)
	}
	for in, want := range map[string]string{"": "", "abc": "abc", "a\xffb": "a%FFb", "\x80\x0f": "%80\x0f", "\xe2\x80\xae": "\xe2\x80\xae", "\xe2\x80": "%E2%80"} {
		if got := pctInvalid(in); got != want {
			t.Errorf("pctInvalid(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestHostileAffiliateID runs a real affiliate link whose ID holds a bidi
// override, ESC, a C1 control, an invalid byte and a newline through run in
// both modes: the ID is shown, escaped, and no raw control or bidi character
// reaches stdout. The rule is a human-owned overlay rule, so a Brave data
// refresh cannot change the result.
func TestHostileAffiliateID(t *testing.T) {
	const in = "https://www.amazon.com/dp/B0?tag=%E2%80%AE%1B%C2%9B%FF%0Ax"
	tests := []struct {
		args  []string
		stdin string
		want  string
	}{
		{[]string{"-affiliate", "strip", "-explain", in}, "", `id="\u202e\x1b\u009b\xff\nx"`},
		{[]string{"-affiliate", "strip", "-json", in}, "", `"id":"\u202e\u001b\u009b%FF\nx"`},
		{[]string{"-affiliate", "strip", "-explain"}, in + "\n", `id="\u202e\x1b\u009b\xff\nx"`},
		{[]string{"-affiliate", "strip", "-json"}, in + "\n", `"id":"\u202e\u001b\u009b%FF\nx"`},
	}
	for _, tt := range tests {
		var stdout, stderr bytes.Buffer
		code := run(env{strings.NewReader(tt.stdin), &stdout, &stderr}, tt.args)
		if code != 0 || stderr.Len() != 0 {
			t.Errorf("%q: code %d, stderr %q", tt.args, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), tt.want) {
			t.Errorf("%q: stdout lacks %s:\n%s", tt.args, tt.want, stdout.String())
		}
		if !strings.Contains(stdout.String(), "https://www.amazon.com/dp/B0") {
			t.Errorf("%q: stdout lacks the cleaned URL:\n%s", tt.args, stdout.String())
		}
		checkTerminalSafe(t, strings.Join(tt.args, " "), stdout.Bytes())
	}
}

// TestVocabularyComplete parses ../../result.go and checks that every
// constant of SkipReason, Part, KeepReason and Category has a word in the
// command's tables, so "unknown" stays a runtime fallback and the -json
// vocabulary cannot drift silently.
func TestVocabularyComplete(t *testing.T) {
	consts := enumConsts(t, "../../result.go", "SkipReason", "Part", "KeepReason", "Category")
	words := map[string]func(int64) string{
		"SkipReason": func(v int64) string { return skipName(linkclean.SkipReason(v)) },
		"Part":       func(v int64) string { return partName(linkclean.Part(v)) },
		"KeepReason": func(v int64) string { return keepName(linkclean.KeepReason(v)) },
		"Category":   func(v int64) string { return catName(linkclean.Category(v)) },
	}
	for typ, word := range words {
		vals := consts[typ]
		if len(vals) == 0 {
			t.Errorf("no %s constants found in result.go", typ)
			continue
		}
		seen := map[string]bool{}
		for name, v := range vals {
			w := word(v)
			switch {
			case typ == "SkipReason" && v == 0:
				if w != "" {
					t.Errorf("%s (%d) = %q, want \"\"", name, v, w)
				}
				continue
			case w == "" || w == "unknown":
				t.Errorf("%s %s (%d) has no word", typ, name, v)
			case seen[w]:
				t.Errorf("%s %s (%d): word %q used twice", typ, name, v, w)
			}
			seen[w] = true
			if typ == "Category" {
				if _, err := linkclean.Category(v).MarshalText(); err != nil {
					t.Errorf("Category %s: %v", name, err)
				}
			}
		}
	}
}

// enumConsts returns, per named type, the constants of that type declared in
// file and their values. It evaluates the forms result.go uses (iota,
// iota + N, an integer literal), including implicit repetition, and fails on
// any other form for a tracked type.
func enumConsts(t *testing.T, file string, types ...string) map[string]map[string]int64 {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]int64{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		var typ string
		var expr ast.Expr
		for iota, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			if len(vs.Values) > 0 {
				typ, expr = "", vs.Values[0]
				if id, ok := vs.Type.(*ast.Ident); ok {
					typ = id.Name
				}
			}
			if !slices.Contains(types, typ) {
				continue
			}
			v := evalConst(t, expr, int64(iota))
			for _, n := range vs.Names {
				if out[typ] == nil {
					out[typ] = map[string]int64{}
				}
				out[typ][n.Name] = v
			}
		}
	}
	return out
}

func evalConst(t *testing.T, e ast.Expr, iota int64) int64 {
	t.Helper()
	switch e := e.(type) {
	case *ast.Ident:
		if e.Name == "iota" {
			return iota
		}
	case *ast.BasicLit:
		if e.Kind == token.INT {
			v, err := strconv.ParseInt(e.Value, 0, 64)
			if err == nil {
				return v
			}
		}
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			return evalConst(t, e.X, iota) + evalConst(t, e.Y, iota)
		}
	}
	t.Fatalf("unsupported constant expression %T", e)
	return 0
}
