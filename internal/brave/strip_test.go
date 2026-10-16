package brave

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/internal/rule"
)

func readData(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../" + path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ruleErrors(t *testing.T, err error) []*rule.Error {
	t.Helper()
	var out []*rule.Error
	j, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("error %v (%T) is not an errors.Join", err, err)
	}
	for _, e := range j.Unwrap() {
		var re *rule.Error
		if !errors.As(e, &re) {
			t.Fatalf("error %v (%T) is not a *rule.Error", e, e)
		}
		out = append(out, re)
	}
	return out
}

func TestCompileStripRealData(t *testing.T) {
	for _, tc := range []struct {
		set, file              string
		rules, params, globals int
	}{
		{"brave/clean-urls", "rules/brave/clean-urls.json", 51, 425, 213},
		{"brave/query-filter", "rules/brave/query-filter.json", 4, 71, 65},
	} {
		t.Run(tc.set, func(t *testing.T) {
			rs, err := CompileStrip(tc.set, readData(t, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			var params, globals int
			seen := map[string]bool{}
			for _, r := range rs {
				if r.Action != rule.Strip || len(r.Include) == 0 || len(r.Params) == 0 || r.Unwrap != nil || r.Affiliate || len(r.Tags) > 0 {
					t.Errorf("%s: unexpected shape %+v", r.ID, r)
				}
				if seen[r.ID] {
					t.Errorf("duplicate ID %q", r.ID)
				}
				seen[r.ID] = true
				params += len(r.Params)
				if slices.Contains(r.Include, "*://*/*") {
					globals += len(r.Params)
				}
			}
			if len(rs) != tc.rules || params != tc.params || globals != tc.globals {
				t.Errorf("got %d rules, %d params, %d global params; want %d, %d, %d", len(rs), params, globals, tc.rules, tc.params, tc.globals)
			}
		})
	}

	rs, err := CompileStrip("brave/clean-urls", readData(t, "rules/brave/clean-urls.json"))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]rule.Rule{}
	for _, r := range rs {
		ids[r.ID] = r
	}
	// Design §3 and §9 examples, and the two global rules of §14.1 "Config
	// semantics": both carry an exclude, so no ID is exactly "*://*/*".
	for _, id := range []string{
		"*://*.amazon.ae/*",
		"*://www.wsj.com/*",
		"*://*/* -*://app.hive.co/*",
		"*://*/* -https://urldefense.com/v3/*",
		"*://*.twitter.com/* -*://*.twitter.com/i/redirect?*",
	} {
		if _, ok := ids[id]; !ok {
			t.Errorf("missing rule ID %q", id)
		}
	}
	if _, ok := ids["*://*/*"]; ok {
		t.Error(`unexpected rule ID "*://*/*"`)
	}
	// Params are stored raw: Brave's %24deep_link is matched canonically by
	// the engine, not rewritten here.
	found := false
	for _, r := range rs {
		if slices.Contains(r.Params, "%24deep_link") {
			found = true
		}
	}
	if !found {
		t.Error(`raw param "%24deep_link" not found`)
	}
}

func TestCompileStripIDs(t *testing.T) {
	const data = `[
		{"include": ["*://b.example/*", "*://a.example/*"], "params": ["x"]},
		{"include": ["*://a.example/*"], "exclude": [], "params": ["y"]},
		{"include": ["*://a.example/*"], "exclude": ["*://a.example/z/*", "*://a.example/y/*"], "params": ["z"]},
		{"include": ["*://a.example/*"], "params": ["w"]}
	]`
	rs, err := CompileStrip("s", []byte(data))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rs {
		got = append(got, r.ID)
	}
	want := []string{
		"*://a.example/*",
		"*://a.example/*#2",
		"*://a.example/* -*://a.example/y/*",
		"*://a.example/*#3",
	}
	if !slices.Equal(got, want) {
		t.Errorf("IDs = %q, want %q", got, want)
	}
	// Include keeps file order; empty exclude is nil.
	if !slices.Equal(rs[0].Include, []string{"*://b.example/*", "*://a.example/*"}) || rs[1].Exclude != nil {
		t.Errorf("rule shape: %+v / %+v", rs[0], rs[1])
	}
}

func TestCompileStripErrors(t *testing.T) {
	for _, tc := range []struct {
		name, data     string
		ruleID, field  string
		wantSubstrings []string
	}{
		{"not JSON", `[`, "", "", nil},
		{"trailing data", `[] []`, "", "", []string{"trailing"}},
		{"not an array", `{}`, "", "", []string{"want array"}},
		{"element not an object", `["x"]`, "#0", "", []string{"want object"}},
		{"unknown key", `[{"include": ["*://a/*"], "params": ["x"], "comment": "y"}]`, "#0", "comment", []string{"unknown field"}},
		{"key case", `[{"Include": ["*://a/*"], "params": ["x"]}]`, "#0", "Include", []string{"unknown field"}},
		{"duplicate key", `[{"include": ["*://a/*"], "params": ["x"], "params": ["y"]}]`, "", "", []string{`duplicate key "params"`}},
		{"missing include", `[{"params": ["x"]}]`, "#0", "include", []string{"missing"}},
		{"empty include", `[{"include": [], "params": ["x"]}]`, "#0", "include", []string{"empty"}},
		{"null include", `[{"include": null, "params": ["x"]}]`, "#0", "include", []string{"have null"}},
		{"non-string include", `[{"include": [1], "params": ["x"]}]`, "#0", "include[0]", []string{"have number"}},
		{"null pattern", `[{"include": [null], "params": ["x"]}]`, "#0", "include[0]", []string{"have null"}},
		{"empty pattern", `[{"include": [""], "params": ["x"]}]`, "#0", "include[0]", []string{"empty string"}},
		{"label host", `[{"include": ["*://*.amazon.*/*"], "params": ["x"]}]`, "#0", "include[0]", []string{"label.*"}},
		{"label host apex", `[{"include": ["*://*/*"], "exclude": ["*://amazon.*:443/*"], "params": ["x"]}]`, "#0", "exclude[0]", []string{"label.*"}},
		{"missing params", `[{"include": ["*://a/*"]}]`, "*://a/*", "params", []string{"missing"}},
		{"empty params", `[{"include": ["*://a/*"], "params": []}]`, "*://a/*", "params", []string{"empty"}},
		{"non-string param", `[{"include": ["*://a/*"], "params": ["x", {}]}]`, "*://a/*", "params[1]", []string{"have object"}},
		{"empty param", `[{"include": ["*://a/*"], "params": [""]}]`, "*://a/*", "params[0]", []string{"empty string"}},
		{"suffixed ID collides", `[{"include": ["*://a/*"], "params": ["x"]}, {"include": ["*://a/*"], "params": ["x"]}, {"include": ["*://a/*#2"], "params": ["x"]}]`, "*://a/*#2", "", []string{"duplicate rule ID"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs, err := CompileStrip("brave/test", []byte(tc.data))
			if err == nil || rs != nil {
				t.Fatalf("got %d rules, err %v; want an error and no rules", len(rs), err)
			}
			es := ruleErrors(t, err)
			e := es[0]
			if e.Set != "brave/test" || e.RuleID != tc.ruleID || e.Field != tc.field {
				t.Errorf("error %q: set/rule/field = %q/%q/%q, want brave/test/%q/%q", e, e.Set, e.RuleID, e.Field, tc.ruleID, tc.field)
			}
			for _, s := range tc.wantSubstrings {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q does not contain %q", err, s)
				}
			}
		})
	}
}

// Every problem is reported, not only the first.
func TestCompileStripReportsAll(t *testing.T) {
	const data = `[
		{"include": [], "params": ["x"]},
		{"include": ["*://a/*"], "params": [""]},
		{"include": ["*://b/*"], "params": ["x"], "x": 1}
	]`
	_, err := CompileStrip("s", []byte(data))
	if es := ruleErrors(t, err); len(es) != 3 {
		t.Errorf("got %d errors, want 3: %v", len(es), err)
	}
}
