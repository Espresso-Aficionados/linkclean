package brave

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/internal/rule"
)

func TestCompileDebounceRealData(t *testing.T) {
	rs, st, err := CompileDebounce(readData(t, "rules/brave/debounce.json"), readData(t, "rules/overlay/debounce-classes.json"))
	if err != nil {
		t.Fatal(err)
	}
	// §4.5.3: 48 rules, 190 instances; 49 rules over 139 instances, 51 skipped.
	want := DebounceStats{Rules: 48, Instances: 190, CompiledRules: 49, CompiledInstances: 139, SkippedInstances: 51}
	if !reflect.DeepEqual(st, want) {
		t.Errorf("stats = %+v, want %+v", st, want)
	}
	if len(rs) != st.CompiledRules {
		t.Errorf("%d rules, stats say %d", len(rs), st.CompiledRules)
	}

	byID := map[string]rule.Rule{}
	patternRules := map[string]int{}
	for _, r := range rs {
		if _, dup := byID[r.ID]; dup {
			t.Errorf("duplicate ID %q", r.ID)
		}
		byID[r.ID] = r
		for _, p := range r.Include {
			patternRules[p]++
		}
		if r.Action != rule.Unwrap || r.Unwrap == nil || len(r.Unwrap.QueryParams) != 1 || r.Unwrap.AllowSameSite ||
			!slices.Equal(r.Tags, []string{"debounce"}) || len(r.Params) > 0 || len(r.Exclude) > 0 {
			t.Errorf("%s: unexpected shape %+v", r.ID, r)
		}
		switch {
		case r.Affiliate && r.Network == "":
			t.Errorf("%s: affiliate rule without Network", r.ID)
		case r.Affiliate && r.IDParam != "" && r.IDPathRegexp != "":
			t.Errorf("%s: two locators", r.ID)
		case !r.Affiliate && (r.Network != "" || r.IDParam != "" || r.IDPathRegexp != ""):
			t.Errorf("%s: unwrap rule with affiliate data", r.ID)
		}
	}

	// The clixGalore collision (§4.5.3, fixture debounce_rule_id_collision).
	for id, param := range map[string]string{
		"*://www.clixgalore.com/Lead.aspx*":   "AffDirectURL",
		"*://www.clixgalore.com/Lead.aspx*#2": "LP",
	} {
		r, ok := byID[id]
		if !ok || r.Unwrap.QueryParams[0] != param || r.Unwrap.PrependScheme != "http" || r.Network != "clixGalore" || r.IDParam != "AfID" {
			t.Errorf("%s: %+v", id, r)
		}
	}
	// One Impact group names every Impact click domain (§4.5.3 "Hop.Rule names the group").
	if r := byID["*://*.3anx.net/c/*"]; r.Network != "Impact" || r.IDPathRegexp != "^/c/([0-9]+)/" || len(r.Include) != 56 ||
		!slices.Contains(r.Include, "*://*.sjv.io/c/*") || !slices.Contains(r.Include, "*://goto.walmart.com/c/*") {
		t.Errorf("Impact group: %+v", r)
	}
	// ShareASale: prepend_scheme maps to PrependScheme (DV-13).
	if r := byID["*://shareasale-analytics.com/r.cfm?*"]; r.Network != "ShareASale" || r.IDParam != "u" || r.Unwrap.PrependScheme != "https" ||
		!slices.Equal(r.Unwrap.QueryParams, []string{"urllink"}) {
		t.Errorf("ShareASale: %+v", r)
	}
	// Commission Factory's param is stored as written (DV-14: matched canonically).
	if r := byID["*://t.cfjump.com/*"]; !slices.Equal(r.Unwrap.QueryParams, []string{"Url"}) || r.IDPathRegexp != "^/([0-9]+)/" {
		t.Errorf("Commission Factory: %+v", r)
	}
	// MediaMarkt compiles once, from the redirect rule (shared-pattern exception).
	if n := patternRules["*://pvn.mediamarkt.de/trck/eclick/*"]; n != 1 {
		t.Errorf("MediaMarkt compiled %d times, want 1", n)
	}
	// Awin cread.php appears in two compiled redirect rules and compiles in both.
	if n := patternRules["*://www.awin1.com/cread.php?*"]; n != 2 {
		t.Errorf("awin cread.php compiled %d times, want 2", n)
	}
	// Inert upstream patterns without '*' compile verbatim (rows 147, 148).
	for _, p := range []string{"*://www.tkqlhce.com/click-", "*://t.lever-analytics.com/email-link?"} {
		if patternRules[p] != 1 {
			t.Errorf("%s compiled %d times, want 1", p, patternRules[p])
		}
	}
}

// The classification table's class counts (§4.5.3: 113 / 24 / 48 over 185
// patterns). The compiler does not depend on them; the design pins them.
func TestDebounceClassesRealData(t *testing.T) {
	l := &errList{set: rule.DebounceSet}
	cls := l.classes(readData(t, "rules/overlay/debounce-classes.json"))
	if err := l.err(); err != nil {
		t.Fatal(err)
	}
	n := map[string]int{}
	for _, c := range cls {
		n[c.class]++
	}
	if len(cls) != 185 || n[classAffiliate] != 113 || n[classUnwrap] != 24 || n[classOff] != 48 {
		t.Errorf("%d patterns, classes %v; want 185, affiliate 113, unwrap 24, off 48", len(cls), n)
	}
}

// classesJSON renders a classification table; each entry is "key=value" pairs.
func classesJSON(t *testing.T, entries map[string][]string) []byte {
	t.Helper()
	pm := map[string]map[string]string{}
	for p, kvs := range entries {
		m := map[string]string{}
		for _, kv := range kvs {
			k, v, _ := strings.Cut(kv, "=")
			m[k] = v
		}
		pm[p] = m
	}
	b, err := json.Marshal(map[string]any{"schema": 1, "patterns": pm})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var (
	affN = []string{"class=affiliate", "network=Net N", "idParam=p", "ref=doc"}
	affM = []string{"class=affiliate", "network=Net M", "why=no single ID"}
	unw  = []string{"class=unwrap"}
	off  = []string{"class=off", "why=not compiled"}
)

// §14.1 "Debounce compilation": a mixed element is split into groups by
// (class, network, locator) in file order.
func TestCompileDebounceGrouping(t *testing.T) {
	deb := `[
		{"include": ["*://z.example/*", "*://u.example/*", "*://b.example/*", "*://o.example/*", "*://m.example/*", "*://a.example/*"],
		 "exclude": ["*://z.example/no/*"], "action": "redirect", "param": "url"}
	]`
	cls := classesJSON(t, map[string][]string{
		"*://z.example/*": affN, "*://b.example/*": affN, "*://a.example/*": affN,
		"*://u.example/*": unw, "*://o.example/*": off, "*://m.example/*": affM,
	})
	rs, st, err := CompileDebounce([]byte(deb), cls)
	if err != nil {
		t.Fatal(err)
	}
	type shape struct {
		ID        string
		Affiliate bool
		Network   string
		IDParam   string
		Include   []string
	}
	var got []shape
	for _, r := range rs {
		got = append(got, shape{r.ID, r.Affiliate, r.Network, r.IDParam, r.Include})
		if !slices.Equal(r.Exclude, []string{"*://z.example/no/*"}) || r.Unwrap.QueryParams[0] != "url" {
			t.Errorf("%s: exclude/param %+v", r.ID, r)
		}
	}
	want := []shape{
		{"*://a.example/*", true, "Net N", "p", []string{"*://z.example/*", "*://b.example/*", "*://a.example/*"}},
		{"*://u.example/*", false, "", "", []string{"*://u.example/*"}},
		{"*://m.example/*", true, "Net M", "", []string{"*://m.example/*"}},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("rules\n got %+v\nwant %+v", got, want)
	}
	if !reflect.DeepEqual(st, DebounceStats{Rules: 1, Instances: 6, CompiledRules: 3, CompiledInstances: 5, SkippedInstances: 1}) {
		t.Errorf("stats %+v", st)
	}
	// Groups do not share slices.
	rs[0].Exclude[0] = "changed"
	if rs[1].Exclude[0] != "*://z.example/no/*" {
		t.Error("groups share the Exclude slice")
	}
}

func TestCompileDebounceUncompiled(t *testing.T) {
	// Every uncompiled form skips its off instances.
	deb := `[
		{"include": ["*://a.example/*"], "exclude": [], "action": "base64,redirect", "param": "u"},
		{"include": ["*://b.example/*"], "exclude": [], "action": "regex-path", "param": "^/x/(.*)"},
		{"include": ["*://c.example/*"], "exclude": [], "action": "regex-path-template", "param": "^/(.*)", "redirect_url_template": "https://$1"},
		{"include": ["*://d.example/*"], "exclude": [], "action": "redirect", "param": "u", "pref": "brave.de_amp.enabled", "prepend_scheme": "https"},
		{"include": ["*://e.example/*"], "exclude": [], "action": "redirect", "param": "u", "redirect_url_template": "https://x/$1"}
	]`
	cls := classesJSON(t, map[string][]string{
		"*://a.example/*": off, "*://b.example/*": off, "*://c.example/*": off, "*://d.example/*": off, "*://e.example/*": off,
	})
	rs, st, err := CompileDebounce([]byte(deb), cls)
	if err != nil || len(rs) != 0 {
		t.Fatalf("rules %v, err %v", rs, err)
	}
	if !reflect.DeepEqual(st, DebounceStats{Rules: 5, Instances: 5, SkippedInstances: 5}) {
		t.Errorf("stats %+v", st)
	}

	// Classed non-off in an uncompiled element: an error naming the element.
	for i, why := range []string{`action "base64,redirect"`, `action "regex-path"`, `action "regex-path-template"`, `"pref"`, `"redirect_url_template"`} {
		p := fmt.Sprintf("*://%c.example/*", 'a'+i)
		cls := classesJSON(t, map[string][]string{
			"*://a.example/*": off, "*://b.example/*": off, "*://c.example/*": off, "*://d.example/*": off, "*://e.example/*": off,
			p: unw,
		})
		_, _, err := CompileDebounce([]byte(deb), cls)
		if err == nil {
			t.Fatalf("%s classed unwrap: no error", p)
		}
		es := ruleErrors(t, err)
		wantMsg := fmt.Sprintf("%q classed unwrap but %s is not compiled in v1", p, why)
		if len(es) != 1 || es[0].RuleID != fmt.Sprintf("#%d", i) || es[0].Field != "include[0]" || es[0].Err.Error() != wantMsg {
			t.Errorf("got %v, want rule #%d include[0]: %s", err, i, wantMsg)
		}
	}
}

// §14.1: the MediaMarkt shape must pass, and its twin must fail.
func TestCompileDebounceSharedPattern(t *testing.T) {
	const mm = "*://pvn.mediamarkt.de/trck/eclick/*"
	cls := classesJSON(t, map[string][]string{
		mm:                  {"class=affiliate", "network=MediaMarktSaturn partner programme", "why=no documented publisher parameter"},
		"*://x.example/*":   off,
		"*://red.example/*": unw,
	})
	pass := `[
		{"include": ["*://red.example/*", "` + mm + `"], "exclude": [], "action": "redirect", "param": "url"},
		{"include": ["*://x.example/*", "` + mm + `"], "exclude": [], "action": "base64,redirect", "param": "url"}
	]`
	rs, st, err := CompileDebounce([]byte(pass), cls)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || !slices.Equal(rs[1].Include, []string{mm}) || !rs[1].Affiliate {
		t.Errorf("rules %+v", rs)
	}
	if !reflect.DeepEqual(st, DebounceStats{Rules: 2, Instances: 4, CompiledRules: 2, CompiledInstances: 2, SkippedInstances: 2}) {
		t.Errorf("stats %+v", st)
	}

	fail := `[
		{"include": ["*://red.example/*"], "exclude": [], "action": "redirect", "param": "url"},
		{"include": ["*://x.example/*", "` + mm + `"], "exclude": [], "action": "base64,redirect", "param": "url"}
	]`
	_, _, err = CompileDebounce([]byte(fail), cls)
	if err == nil {
		t.Fatal("twin: no error")
	}
	es := ruleErrors(t, err)
	if len(es) != 1 || es[0].RuleID != "#1" || es[0].Field != "include[1]" || !strings.Contains(es[0].Error(), `classed affiliate but action "base64,redirect" is not compiled`) {
		t.Errorf("twin: %v", err)
	}
}

func TestCompileDebounceFields(t *testing.T) {
	deb := `[
		{"include": ["*://www.tkqlhce.com/click-"], "exclude": [], "action": "redirect", "param": "url"},
		{"include": ["*://s.example/r.cfm?*"], "exclude": [], "action": "redirect", "param": "urllink", "prepend_scheme": "https"},
		{"include": ["*://c.example/*"], "exclude": [], "action": "redirect", "param": "Url"},
		{"include": ["*://www.tkqlhce.com/click-"], "exclude": [], "action": "redirect", "param": "u2"}
	]`
	cls := classesJSON(t, map[string][]string{
		"*://www.tkqlhce.com/click-": {"class=affiliate", "network=CJ Affiliate", "idPath=^/click-([0-9]+)-", "ref=doc"},
		"*://s.example/r.cfm?*":      unw,
		"*://c.example/*":            unw,
		"*://stale.example/*":        off,
		"*://also-stale.example/*":   unw,
	})
	rs, st, err := CompileDebounce([]byte(deb), cls)
	if err != nil {
		t.Fatal(err)
	}
	// An exact path compiles verbatim; matching it (only "/click-") is the engine's.
	if r := rs[0]; r.ID != "*://www.tkqlhce.com/click-" || r.IDPathRegexp != "^/click-([0-9]+)-" || r.Unwrap.PrependScheme != "" {
		t.Errorf("exact path: %+v", r)
	}
	if r := rs[1]; r.Unwrap.PrependScheme != "https" || r.Unwrap.QueryParams[0] != "urllink" {
		t.Errorf("prepend_scheme: %+v", r)
	}
	if r := rs[2]; r.Unwrap.QueryParams[0] != "Url" {
		t.Errorf("param not stored raw: %+v", r)
	}
	if r := rs[3]; r.ID != "*://www.tkqlhce.com/click-#2" {
		t.Errorf("collision suffix: %q", r.ID)
	}
	if !slices.Equal(st.Stale, []string{"*://also-stale.example/*", "*://stale.example/*"}) {
		t.Errorf("stale = %q", st.Stale)
	}
}

func TestCompileDebounceErrors(t *testing.T) {
	okClasses := classesJSON(t, map[string][]string{"*://a.example/*": unw})
	okDeb := `[{"include": ["*://a.example/*"], "exclude": [], "action": "redirect", "param": "u"}]`
	for _, tc := range []struct {
		name          string
		deb           string
		classes       []byte
		ruleID, field string
		msg           string
	}{
		{"unclassified", `[{"include": ["*://a.example/*"], "exclude": [], "action": "redirect", "param": "u"},
			{"include": ["*://a.example/*", "*://new.example/*"], "exclude": [], "action": "redirect", "param": "u"}]`,
			okClasses, "#1", "include[1]", `unclassified pattern "*://new.example/*"`},
		{"unknown key", `[{"include": ["*://a.example/*"], "exclude": [], "action": "redirect", "param": "u", "priority": 1}]`,
			okClasses, "#0", "priority", "unknown field"},
		{"unknown action", `[{"include": ["*://a.example/*"], "exclude": [], "action": "base64", "param": "u"}]`,
			okClasses, "#0", "action", `unknown action "base64"`},
		{"missing action", `[{"include": ["*://a.example/*"], "exclude": [], "param": "u"}]`,
			okClasses, "#0", "action", "missing"},
		{"empty include", `[{"include": [], "exclude": [], "action": "redirect", "param": "u"}]`,
			okClasses, "#0", "include", "empty"},
		{"missing exclude", `[{"include": ["*://a.example/*"], "action": "redirect", "param": "u"}]`,
			okClasses, "#0", "exclude", "missing"},
		{"empty param", `[{"include": ["*://a.example/*"], "exclude": [], "action": "redirect", "param": ""}]`,
			okClasses, "#0", "param", "empty string"},
		{"non-string param", `[{"include": ["*://a.example/*"], "exclude": [], "action": "redirect", "param": ["u"]}]`,
			okClasses, "#0", "param", "have array"},
		{"bad prepend_scheme", `[{"include": ["*://a.example/*"], "exclude": [], "action": "redirect", "param": "u", "prepend_scheme": "ftp"}]`,
			okClasses, "#0", "prepend_scheme", `have "ftp"`},
		{"label host", `[{"include": ["*://*.amazon.*/*"], "exclude": [], "action": "redirect", "param": "u"}]`,
			okClasses, "#0", "include[0]", "label.*"},
		{"not an array", `{}`, okClasses, "", "debounce.json", "want array"},

		{"classes: not JSON", okDeb, []byte(`{`), "", "debounce-classes.json", ""},
		{"classes: schema", okDeb, []byte(`{"schema": 2, "patterns": {"*://a.example/*": {"class": "unwrap"}}}`),
			"", "debounce-classes.json.schema", "want 1"},
		{"classes: unknown top key", okDeb, []byte(`{"schema": 1, "patterns": {"*://a.example/*": {"class": "unwrap"}}, "x": 1}`),
			"", "debounce-classes.json.x", "unknown field"},
		{"classes: duplicate pattern", okDeb, []byte(`{"schema": 1, "patterns": {"*://a.example/*": {"class": "unwrap"}, "*://a.example/*": {"class": "off", "why": "x"}}}`),
			"", "debounce-classes.json", "duplicate key"},
		{"classes: unknown entry key", okDeb, classesJSON(t, map[string][]string{"*://a.example/*": {"class=unwrap", "noID=x"}}),
			"", `debounce-classes.json["*://a.example/*"].noID`, "unknown field"},
		{"classes: bad class", okDeb, classesJSON(t, map[string][]string{"*://a.example/*": {"class=tracker"}}),
			"", `debounce-classes.json["*://a.example/*"].class`, `have "tracker"`},
		{"classes: missing class", okDeb, classesJSON(t, map[string][]string{"*://a.example/*": {"why=x"}}),
			"", `debounce-classes.json["*://a.example/*"].class`, "missing"},
		{"classes: affiliate without network", okDeb, classesJSON(t, map[string][]string{"*://a.example/*": {"class=affiliate", "why=x"}}),
			"", `debounce-classes.json["*://a.example/*"].network`, "missing"},
		{"classes: network on unwrap", okDeb, classesJSON(t, map[string][]string{"*://a.example/*": {"class=unwrap", "network=X"}}),
			"", `debounce-classes.json["*://a.example/*"].network`, "only affiliate"},
		{"classes: bad network charset", okDeb, classesJSON(t, map[string][]string{"*://a.example/*": {"class=affiliate", "network=<@everyone>", "why=x"}}),
			"", `debounce-classes.json["*://a.example/*"].network`, "does not match"},
		{"classes: two locators", okDeb, classesJSON(t, map[string][]string{"*://a.example/*": {"class=affiliate", "network=X", "idParam=a", "idPath=^/(a)", "ref=r"}}),
			"", `debounce-classes.json["*://a.example/*"]`, "at most one locator"},
		{"classes: locator on unwrap", okDeb, classesJSON(t, map[string][]string{"*://a.example/*": {"class=unwrap", "idParam=a", "ref=r"}}),
			"", `debounce-classes.json["*://a.example/*"]`, "only affiliate"},
		{"classes: locator without ref", okDeb, classesJSON(t, map[string][]string{"*://a.example/*": {"class=affiliate", "network=X", "idParam=a"}}),
			"", `debounce-classes.json["*://a.example/*"].ref`, "missing"},
		{"classes: idPath does not compile", okDeb, classesJSON(t, map[string][]string{"*://a.example/*": {"class=affiliate", "network=X", "idPath=^/(", "ref=r"}}),
			"", `debounce-classes.json["*://a.example/*"].idPath`, "missing closing )"},
		{"classes: idPath two groups", okDeb, classesJSON(t, map[string][]string{"*://a.example/*": {"class=affiliate", "network=X", "idPath=^/(a)/(b)", "ref=r"}}),
			"", `debounce-classes.json["*://a.example/*"].idPath`, "exactly one capture group, have 2"},
		{"classes: idPath no group", okDeb, classesJSON(t, map[string][]string{"*://a.example/*": {"class=affiliate", "network=X", "idPath=^/a", "ref=r"}}),
			"", `debounce-classes.json["*://a.example/*"].idPath`, "exactly one capture group, have 0"},
		{"classes: off without why", okDeb, classesJSON(t, map[string][]string{"*://a.example/*": {"class=off"}}),
			"", `debounce-classes.json["*://a.example/*"].why`, "missing"},
		{"classes: affiliate without locator or why", okDeb, classesJSON(t, map[string][]string{"*://a.example/*": {"class=affiliate", "network=X"}}),
			"", `debounce-classes.json["*://a.example/*"].why`, "missing"},
		{"classes: empty idParam", okDeb, classesJSON(t, map[string][]string{"*://a.example/*": {"class=affiliate", "network=X", "idParam=", "ref=r"}}),
			"", `debounce-classes.json["*://a.example/*"].idParam`, "empty string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs, _, err := CompileDebounce([]byte(tc.deb), tc.classes)
			if err == nil || rs != nil {
				t.Fatalf("got %d rules, err %v; want an error and no rules", len(rs), err)
			}
			es := ruleErrors(t, err)
			e := es[0]
			if e.Set != rule.DebounceSet || e.RuleID != tc.ruleID || e.Field != tc.field || !strings.Contains(e.Err.Error(), tc.msg) {
				t.Errorf("got %q (rule %q, field %q); want rule %q, field %q, message containing %q", e, e.RuleID, e.Field, tc.ruleID, tc.field, tc.msg)
			}
		})
	}
}

func TestCheckDuplicateKeys(t *testing.T) {
	for in, dup := range map[string]bool{
		`{"a": 1, "b": {"a": 2}, "c": [{"a": 1}, {"a": 2}]}`: false,
		`{"a": {"x": 1}, "b": ["a", "a"], "c": "a"}`:         false,
		`[{"a": 1}, {"b": 1, "b": 2}]`:                       true,
		`{"a": {"x": [1, {"y": 1}], "x": 2}}`:                true,
		`{"a": [], "a": []}`:                                 true,
	} {
		if err := CheckDuplicateKeys([]byte(in)); (err != nil) != dup {
			t.Errorf("%s: err %v, want duplicate %v", in, err, dup)
		}
	}
}
