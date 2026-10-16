package matchpattern

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"testing"
)

// Every include and exclude in the vendored Brave lists, and every key of the
// debounce classification table, is inside the subset without the label.*
// extension (§4.5.1 "build time").
func TestVendoredPatternsParse(t *testing.T) {
	n := 0
	for _, f := range []string{"clean-urls.json", "query-filter.json", "debounce.json"} {
		var rules []struct{ Include, Exclude []string }
		readJSON(t, "../../rules/brave/"+f, &rules)
		for i, r := range rules {
			for _, p := range append(r.Include, r.Exclude...) {
				if _, err := Parse(p, false); err != nil {
					t.Errorf("%s #%d: %v", f, i, err)
				}
				n++
			}
		}
	}
	var classes struct{ Patterns map[string]json.RawMessage }
	readJSON(t, "../../rules/overlay/debounce-classes.json", &classes)
	if len(classes.Patterns) == 0 {
		t.Error("debounce-classes.json: no patterns")
	}
	for p := range classes.Patterns {
		if _, err := Parse(p, false); err != nil {
			t.Errorf("debounce-classes.json: %v", err)
		}
		n++
	}
	t.Logf("%d pattern instances parsed", n)
}

// Overlay patterns may use label.*. The overlay lands separately (WP-C); the
// test skips until it exists.
func TestOverlayPatternsParse(t *testing.T) {
	b, err := os.ReadFile("../../rules/overlay/overlay.json")
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("rules/overlay/overlay.json not present")
	}
	if err != nil {
		t.Fatal(err)
	}
	var rules []struct {
		ID               string
		Include, Exclude []string
	}
	if err := json.Unmarshal(b, &rules); err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		for _, p := range append(r.Include, r.Exclude...) {
			if _, err := Parse(p, true); err != nil {
				t.Errorf("overlay %s: %v", r.ID, err)
			}
		}
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// The ShareASale and AvantLink debounce includes match their wrapper shapes
// and nothing on look-alike hosts.
func TestVendoredShapes(t *testing.T) {
	var rules []struct{ Include []string }
	readJSON(t, "../../rules/brave/debounce.json", &rules)
	have := map[string]bool{}
	for _, r := range rules {
		for _, p := range r.Include {
			have[p] = true
		}
	}
	for _, p := range []string{"*://www.tkqlhce.com/click-", "*://t.lever-analytics.com/email-link?", "*://*.avantlink.com/click.php?*"} {
		if !have[p] {
			t.Errorf("debounce.json no longer has %s", p)
		}
	}
	runMatchCases(t, []matchCase{
		{"*://*.avantlink.com/click.php?*", "https://www.avantlink.com/click.php?tt=cl&pw=1&url=x", "", true},
		{"*://*.avantlink.com/click.php?*", "https://avantlink.com.evil.example/click.php?url=x", "", false},
		{"*://*.avantlink.com/click.php?*", "https://www.avantlink.com/click.php", "", false},
	})
}
