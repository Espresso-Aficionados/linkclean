package linkclean

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Espresso-Aficionados/linkclean/internal/rule"
)

// TestEmbedSet pins the embedded file set (design §11.7): moving a test input
// into the embedded set, or dropping a data file, must not go unnoticed.
func TestEmbedSet(t *testing.T) {
	var got []string
	err := fs.WalkDir(rulesFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			got = append(got, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"rules.lock.json",
		"rules/brave/clean-urls.json",
		"rules/brave/debounce.json",
		"rules/brave/query-filter.json",
		"rules/overlay/debounce-classes.json",
		"rules/overlay/overlay.json",
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("embedded files\n got %q\nwant %q", got, want)
	}
}

func TestDefaultSources(t *testing.T) {
	lock, err := readLock(rulesFS)
	if err != nil {
		t.Fatal(err)
	}
	sets := DefaultSources()
	var names []string
	for _, s := range sets {
		names = append(names, s.Name)
	}
	if want := []string{"brave/clean-urls", "brave/query-filter", "brave/debounce", "linkclean/overlay"}; !slices.Equal(names, want) {
		t.Fatalf("names %q, want %q", names, want)
	}
	for i, f := range braveFiles {
		s := sets[i]
		lf := lock.Upstream.Files[f.upstream]
		if s.License != "MPL-2.0" || s.Upstream != "github.com/brave/adblock-lists "+f.upstream ||
			s.Commit != lock.Upstream.Commit || len(s.Commit) != 40 || s.SHA256 != lf.SHA256 {
			t.Errorf("%s metadata: %q %q %q %q", s.Name, s.License, s.Upstream, s.Commit, s.SHA256)
		}
		for _, want := range []string{lf.Vendored, f.upstream, lock.Upstream.Commit, lf.SHA256, "MPL-2.0",
			"Source: https://github.com/brave/adblock-lists/tree/" + lock.Upstream.Commit + "/brave-lists"} {
			if !strings.Contains(s.Notice, want) {
				t.Errorf("%s notice does not name %q:\n%s", s.Name, want, s.Notice)
			}
		}
	}
	// The brave/debounce notice names both MPL inputs (D2).
	if !strings.Contains(sets[2].Notice, "rules/brave/debounce.json") || !strings.Contains(sets[2].Notice, "rules/overlay/debounce-classes.json") {
		t.Errorf("brave/debounce notice:\n%s", sets[2].Notice)
	}
	ov := sets[3]
	if ov.License != "MIT" || ov.Commit != "" || ov.SHA256 != "" || !strings.Contains(ov.Notice, "MIT") {
		t.Errorf("overlay metadata: %+v", RuleSet{Name: ov.Name, License: ov.License, Commit: ov.Commit, SHA256: ov.SHA256, Notice: ov.Notice})
	}

	// Counts (§4.5.2, §4.5.3); the overlay must exist and cite evidence.
	for i, n := range []int{51, 4, 49} {
		if len(sets[i].Rules) != n {
			t.Errorf("%s: %d rules, want %d", sets[i].Name, len(sets[i].Rules), n)
		}
	}
	if len(ov.Rules) == 0 {
		t.Error("overlay has no rules")
	}
	for _, r := range sets[2].Rules {
		if r.Action != Unwrap || !slices.Equal(r.Tags, []string{"debounce"}) || r.Unwrap == nil {
			t.Errorf("brave/debounce:%s: %+v", r.ID, r)
		}
	}
}

func TestDefaultSourcesDeepCopy(t *testing.T) {
	a := DefaultSources()
	before, _ := json.Marshal(DefaultSources())
	for i := range a {
		for j := range a[i].Rules {
			r := &a[i].Rules[j]
			r.ID += "x"
			r.Include[0] = "mutated"
			if len(r.Params) > 0 {
				r.Params[0] = "mutated"
			}
			if len(r.Tags) > 0 {
				r.Tags[0] = "mutated"
			}
			if r.Unwrap != nil {
				r.Unwrap.QueryParams[0] = "mutated"
				r.Unwrap.PrependScheme = "mutated"
			}
		}
		a[i].Rules = a[i].Rules[:0]
		a[i].Notice = ""
	}
	after, _ := json.Marshal(DefaultSources())
	if string(before) != string(after) {
		t.Error("mutating DefaultSources() changed a later result")
	}
}

// fixtureFS is the vendored data from the working tree with a fixture
// classification table that classes every debounce pattern off, and the
// given overlay.
func fixtureFS(t *testing.T, overlay string) fstest.MapFS {
	t.Helper()
	fsys := fstest.MapFS{"rules/overlay/overlay.json": {Data: []byte(overlay)}}
	for _, p := range []string{"rules.lock.json", "rules/brave/clean-urls.json", "rules/brave/query-filter.json", "rules/brave/debounce.json"} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		fsys[p] = &fstest.MapFile{Data: b}
	}
	var deb []struct {
		Include []string `json:"include"`
	}
	if err := json.Unmarshal(fsys["rules/brave/debounce.json"].Data, &deb); err != nil {
		t.Fatal(err)
	}
	patterns := map[string]map[string]string{}
	for _, d := range deb {
		for _, p := range d.Include {
			patterns[p] = map[string]string{"class": "off", "why": "fixture"}
		}
	}
	b, err := json.Marshal(map[string]any{"schema": 1, "patterns": patterns})
	if err != nil {
		t.Fatal(err)
	}
	fsys["rules/overlay/debounce-classes.json"] = &fstest.MapFile{Data: b}
	return fsys
}

const fixtureOverlay = `[{"id": "x", "action": "strip", "include": ["*://*.example.com/*"], "params": ["x"], "ref": "test"}]`

func TestReadDefaultsFixture(t *testing.T) {
	sets, err := readDefaults(fixtureFS(t, fixtureOverlay))
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 4 || len(sets[2].Rules) != 0 || len(sets[3].Rules) != 1 || sets[3].Rules[0].Action != rule.Strip {
		t.Errorf("sets: %d, debounce rules %d, overlay %+v", len(sets), len(sets[2].Rules), sets[3].Rules)
	}

	for _, tc := range []struct {
		name   string
		mutate func(fstest.MapFS)
		want   string
	}{
		{"vendored bytes differ from lock", func(f fstest.MapFS) {
			f["rules/brave/query-filter.json"].Data = append(f["rules/brave/query-filter.json"].Data, '\n')
		}, "rules/brave/query-filter.json: sha256"},
		{"missing classification table", func(f fstest.MapFS) {
			delete(f, "rules/overlay/debounce-classes.json")
		}, "debounce-classes.json"},
		{"missing overlay", func(f fstest.MapFS) {
			delete(f, "rules/overlay/overlay.json")
		}, "overlay.json"},
		{"lock schema", func(f fstest.MapFS) {
			f["rules.lock.json"].Data = []byte(strings.Replace(string(f["rules.lock.json"].Data), `"schema": 4`, `"schema": 3`, 1))
		}, "schema 3, want 4"},
		{"lock unknown field", func(f fstest.MapFS) {
			f["rules.lock.json"].Data = []byte(strings.Replace(string(f["rules.lock.json"].Data), `"schema": 4`, `"schema": 4, "checked": "x"`, 1))
		}, `unknown field "checked"`},
		{"file not locked", func(f fstest.MapFS) {
			f["rules.lock.json"].Data = []byte(strings.Replace(string(f["rules.lock.json"].Data), `"brave-lists/debounce.json"`, `"brave-lists/debounce2.json"`, 1))
		}, "brave-lists/debounce.json is not locked"},
		{"license not MPL", func(f fstest.MapFS) {
			f["rules.lock.json"].Data = []byte(strings.Replace(string(f["rules.lock.json"].Data), `"spdx": "MPL-2.0"`, `"spdx": "MIT"`, 1))
		}, `spdx is "MIT"`},
		{"unclassified debounce pattern", func(f fstest.MapFS) {
			f["rules/overlay/debounce-classes.json"].Data = []byte(`{"schema": 1, "patterns": {}}`)
		}, "unclassified pattern"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fixtureFS(t, fixtureOverlay)
			tc.mutate(fsys)
			sets, err := readDefaults(fsys)
			if err == nil || sets != nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %d sets, err %v; want error containing %q", len(sets), err, tc.want)
			}
		})
	}
}

func TestDecodeOverlay(t *testing.T) {
	rs, err := decodeOverlay([]byte(`[
		{"id": "u", "action": "unwrap", "tags": ["unwrap"], "include": ["*://*.google.*/url?*"],
		 "unwrap": {"queryParams": ["q", "url"], "allowSameSite": true}, "ref": "corpus x"},
		{"id": "a-keep", "action": "keep", "affiliate": true, "network": "N", "idParam": "tag",
		 "include": ["*://*.amazon.*/*"], "params": ["tag"], "ref": "y"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].Action != Unwrap || !rs[0].Unwrap.AllowSameSite || rs[1].Action != Keep || rs[1].IDParam != "tag" {
		t.Errorf("rules %+v", rs)
	}

	for _, tc := range []struct {
		name, data    string
		ruleID, field string
		msg           string
	}{
		{"unknown field", `[{"id": "a", "action": "strip", "include": ["*://*/*"], "params": ["x"], "ref": "r", "evidence": "e"}]`, "#0", "", `unknown field "evidence"`},
		{"unknown nested field", `[{"id": "a", "action": "unwrap", "include": ["*://*/*"], "unwrap": {"queryParams": ["u"], "base64": true}, "ref": "r"}]`, "#0", "", `unknown field "base64"`},
		{"missing ref", `[{"id": "a", "action": "strip", "include": ["*://*/*"], "params": ["x"]}]`, "a", "ref", "missing"},
		{"empty ref", `[{"id": "a", "action": "strip", "include": ["*://*/*"], "params": ["x"], "ref": ""}]`, "a", "ref", "missing"},
		{"missing ref and id", `[{"action": "strip", "include": ["*://*/*"], "params": ["x"]}]`, "#0", "ref", "missing"},
		{"unknown action", `[{"id": "a", "action": "drop", "include": ["*://*/*"], "ref": "r"}]`, "#0", "", `unknown action "drop"`},
		{"duplicate key", `[{"id": "a", "action": "strip", "include": ["*://*/*"], "params": ["x"], "params": ["y"], "ref": "r"}]`, "", overlayPath, `duplicate key "params"`},
		{"null", `null`, "", overlayPath, "have null"},
		{"object", `{}`, "", overlayPath, "cannot unmarshal"},
		{"trailing data", `[] []`, "", overlayPath, "invalid character"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs, err := decodeOverlay([]byte(tc.data))
			var re *RuleError
			if err == nil || rs != nil || !errors.As(err, &re) {
				t.Fatalf("got %d rules, err %v (%T); want a *RuleError", len(rs), err, err)
			}
			if re.Set != overlaySet || re.RuleID != tc.ruleID || re.Field != tc.field || !strings.Contains(re.Err.Error(), tc.msg) {
				t.Errorf("got %q (rule %q, field %q); want rule %q, field %q, message containing %q", re, re.RuleID, re.Field, tc.ruleID, tc.field, tc.msg)
			}
		})
	}
}
