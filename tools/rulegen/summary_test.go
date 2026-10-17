package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/gittest"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
)

var sumUpdate = flag.Bool("update-summary", false, "rewrite testdata/summary/*.golden.md")

const sumTestdata = "testdata/summary"

// sumUtilsCC stands in for brave-core's utils.cc (synthetic text, not
// brave-core's).
const sumUtilsCC = `// fixture
namespace {
static constexpr auto kConditionalQueryStringTrackers =
    MakeMap({
        {"a_param", "/a"},
    });

static constexpr auto kExemptedHostnames =
    MakeSet({"exempt.example"});
}  // namespace

bool Use() { return kExemptedHostnames.count("x") == 1; }
`

// sumParseTxtar splits a txtar archive into its comment and files.
func sumParseTxtar(t testing.TB, name string) (string, map[string][]byte) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(sumTestdata, name))
	if err != nil {
		t.Fatal(err)
	}
	var comment strings.Builder
	files := map[string][]byte{}
	cur := ""
	for _, l := range strings.SplitAfter(string(b), "\n") {
		s := strings.TrimRight(l, "\n")
		if strings.HasPrefix(s, "-- ") && strings.HasSuffix(s, " --") && len(s) > 6 {
			cur = strings.TrimSpace(s[3 : len(s)-3])
			files[cur] = []byte{}
			continue
		}
		if cur == "" {
			comment.WriteString(l)
		} else {
			files[cur] = append(files[cur], l...)
		}
	}
	return comment.String(), files
}

// sumWorld is a linkclean copy (the full module, so the engine runs) with a
// local upstream: base is a commit synced to the first upstream commit, and
// a fixture edits upstream and the copy to make head.
type sumWorld struct {
	t    *testing.T
	g    repo.Git
	l    *gittest.Linkclean
	u    *gittest.Upstream
	up   map[string][]byte // the next upstream commit's tree
	tip  string            // upstream master
	day  int
	base string // the base commit
	g0   string // the first upstream commit: base's gitlink and lock commit

	mu          sync.Mutex
	watchBody   string
	watchStatus int
	watchURLs   []string
}

// sumNewWorld builds the world over the real data, or over
// testdata/summary/synthetic.txtar.
func sumNewWorld(t *testing.T, synthetic bool) *sumWorld {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the root module")
	}
	g := repo.Exec{Env: gittest.Isolate(t)}
	w := &sumWorld{t: t, g: g, watchBody: sumUtilsCC, watchStatus: http.StatusOK}
	w.l = gittest.NewLinkclean(t, g, gittest.Opts{FullModule: true})
	w.u = gittest.NewUpstream(t, g)
	if synthetic {
		w.up = map[string][]byte{}
		es, err := w.l.Work.LsTree(context.Background(), "HEAD", "testdata/corpus")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			w.l.Remove(e.Path)
		}
		_, files := sumParseTxtar(t, "synthetic.txtar")
		w.apply(files)
	} else {
		w.up = gittest.RealData(t)
		w.up["brave-lists/other.txt"] = []byte("another list\n")
	}
	w.l.Write(data.WatchPath, sumWatchJSON(t, sumUtilsCC))
	w.g0 = w.upCommit("initial")
	w.sync(w.g0)
	w.base = w.l.Commit("base")
	return w
}

// apply writes txtar files: upstream/<path> into the next upstream commit,
// tree/<path> into the working tree.
func (w *sumWorld) apply(files map[string][]byte) {
	w.t.Helper()
	for name, b := range files {
		switch {
		case strings.HasPrefix(name, "upstream/"):
			w.up[strings.TrimPrefix(name, "upstream/")] = b
		case strings.HasPrefix(name, "tree/"):
			w.l.Write(strings.TrimPrefix(name, "tree/"), b)
		default:
			w.t.Fatalf("txtar file %q is neither upstream/ nor tree/", name)
		}
	}
}

// upCommit commits w.up on upstream master, a day after the previous one.
func (w *sumWorld) upCommit(msg string) string {
	w.t.Helper()
	w.day++
	when := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC).AddDate(0, 0, w.day)
	w.tip = w.u.Commit(w.tip, maps.Clone(w.up), when, msg)
	w.u.SetRef("refs/heads/master", w.tip)
	return w.tip
}

// sync writes what rulegen sync writes for g and moves the gitlink there.
func (w *sumWorld) sync(g string) {
	w.t.Helper()
	w.l.SyncedTo(w.u, g)
	w.l.SetGitlink(g)
}

// edit replaces an upstream JSON file with f applied to its decoded value.
func (w *sumWorld) edit(up string, f func(v any) any) {
	w.t.Helper()
	var v any
	if err := json.Unmarshal(w.up[up], &v); err != nil {
		w.t.Fatalf("%s: %v", up, err)
	}
	v = f(v)
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		w.t.Fatal(err)
	}
	w.up[up] = b.Bytes()
}

// rules returns the elements of a decoded rule file.
func sumRules(t testing.TB, v any) []map[string]any {
	t.Helper()
	a, ok := v.([]any)
	if !ok {
		t.Fatalf("not an array: %T", v)
	}
	out := make([]map[string]any, len(a))
	for i, e := range a {
		out[i] = e.(map[string]any)
	}
	return out
}

// sumFind returns the index of the first rule whose include holds pattern
// and that satisfies ok (nil: any).
func sumFind(t testing.TB, v any, pattern string, ok func(map[string]any) bool) int {
	t.Helper()
	for i, r := range sumRules(t, v) {
		if slices.Contains(sumStrings(r["include"]), pattern) && (ok == nil || ok(r)) {
			return i
		}
	}
	t.Fatalf("no rule includes %q", pattern)
	return -1
}

func sumAppend(r map[string]any, key string, vals ...any) {
	a, _ := r[key].([]any)
	r[key] = append(a, vals...)
}

func sumRemove(t testing.TB, r map[string]any, key, val string) {
	t.Helper()
	a, _ := r[key].([]any)
	i := slices.IndexFunc(a, func(x any) bool { return x == val })
	if i < 0 {
		t.Fatalf("%q not in %s", val, key)
	}
	r[key] = slices.Delete(slices.Clone(a), i, i+1)
}

func (w *sumWorld) commit(msg string) string {
	w.t.Helper()
	return w.l.Commit(msg)
}

// RoundTrip serves the fake brave-core file for any request.
func (w *sumWorld) RoundTrip(r *http.Request) (*http.Response, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.watchURLs = append(w.watchURLs, r.URL.String())
	return &http.Response{
		StatusCode: w.watchStatus,
		Status:     fmt.Sprintf("%d %s", w.watchStatus, http.StatusText(w.watchStatus)),
		Body:       io.NopCloser(strings.NewReader(w.watchBody)),
		Header:     http.Header{},
		Request:    r,
	}, nil
}

// sumGo is the go command for the module copies: no network, no workspace,
// the local toolchain.
func sumGo(ctx context.Context, dir string, env []string, args ...string) ([]byte, []byte, error) {
	return goCommand(ctx, dir, append([]string{"GOPROXY=off", "GOFLAGS=-mod=mod", "GOWORK=off", "GOTOOLCHAIN=local"}, env...), args...)
}

// summary runs rulegen summary -root <copy> -out <file> args... and returns
// the output and the exit code.
func (w *sumWorld) summary(args ...string) (string, int) {
	w.t.Helper()
	e, _, stderr := newTestEnv(w.t, nil)
	e.HTTP = &http.Client{Transport: w}
	e.Go = sumGo
	out := filepath.Join(w.t.TempDir(), "out", "summary.md")
	code := run(context.Background(), e, append([]string{"summary", "-root", w.l.Work.Dir, "-out", out}, args...))
	if code != exitOK {
		return stderr.String(), code
	}
	b, err := os.ReadFile(out)
	if err != nil {
		w.t.Fatal(err)
	}
	return string(b), code
}

// mustSummary runs summary and requires exit 0 and the marker.
func (w *sumWorld) mustSummary(args ...string) string {
	w.t.Helper()
	out, code := w.summary(args...)
	if code != exitOK {
		w.t.Fatalf("summary exit %d: %s", code, out)
	}
	if !strings.HasPrefix(out, data.SummaryMarker+"\n") {
		w.t.Fatalf("summary does not start with the marker:\n%s", out)
	}
	if strings.Contains(out, "engine data unavailable") {
		w.t.Fatalf("TestSummaryData failed:\n%s", out)
	}
	return out
}

func sumWatchJSON(t testing.TB, src string) []byte {
	t.Helper()
	wf := sumWatchFile{Source: "fixture", Blocks: map[string]string{}}
	for _, id := range sumWatchBlocks {
		b, ok := sumWatchBlock([]byte(src), id)
		if !ok {
			t.Fatalf("fixture lacks %s", id)
		}
		wf.Blocks[id] = data.SHA256Hex(b)
	}
	b, err := json.MarshalIndent(wf, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func sumWant(t testing.TB, out string, wants ...string) {
	t.Helper()
	for _, s := range wants {
		if !strings.Contains(out, s) {
			t.Errorf("summary lacks %q:\n%s", s, out)
		}
	}
}

func sumNotWant(t testing.TB, out string, nots ...string) {
	t.Helper()
	for _, s := range nots {
		if strings.Contains(out, s) {
			t.Errorf("summary has %q:\n%s", s, out)
		}
	}
}

// sumSection returns the text of one ### section.
func sumSection(t testing.TB, out, title string) string {
	t.Helper()
	_, rest, ok := strings.Cut(out, "\n### "+title+"\n")
	if !ok {
		t.Fatalf("no section %q:\n%s", title, out)
	}
	sec, _, _ := strings.Cut(rest, "\n### ")
	return sec
}

func sumGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(sumTestdata, name+".golden.md")
	if *sumUpdate {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update-summary)", err)
	}
	if got != string(want) {
		t.Errorf("summary differs from %s (run with -update-summary to accept):\n%s", path, sumLineDiff(string(want), got))
	}
}

// sumLineDiff lists the lines only in want (-) or only in got (+).
func sumLineDiff(want, got string) string {
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	var b strings.Builder
	for _, l := range wl {
		if !slices.Contains(gl, l) {
			fmt.Fprintf(&b, "- %s\n", l)
		}
	}
	for _, l := range gl {
		if !slices.Contains(wl, l) {
			fmt.Fprintf(&b, "+ %s\n", l)
		}
	}
	return b.String()
}

func sumSHA(b []byte) string { return data.SHA256Hex(b) }

// TestSummaryFixtures runs the §11.8 summary rows, and the summary halves of
// the gate and sync rows, against a linkclean copy and a local upstream.
func TestSummaryFixtures(t *testing.T) {
	const cleanURLs, queryFilter, debounce = "brave-lists/clean-urls.json", "brave-lists/query-filter.json", "brave-lists/debounce.json"

	t.Run("summary_golden", func(t *testing.T) {
		t.Parallel()
		w := sumNewWorld(t, true)
		_, files := sumParseTxtar(t, "summary_golden.txtar")
		w.apply(files)
		w.sync(w.upCommit("mixed"))
		w.commit("head")
		out := w.mustSummary("-base", w.base)
		// The parts the row names, so a golden accepted by mistake still fails.
		sumWant(t, out, "**broke**", "improved", "| `news-1` | changed |",
			"- ⚠★ (`*://*.shop.example/*`, `id`) — risky: never-strip",
			"- ★⚑ (`global`, `irpid`) — affiliate-class candidate",
			"(`global`, `fbclid`): clean-urls → query-filter")
		sumGolden(t, "summary_golden", out)
	})

	t.Run("sync_bump_ok", func(t *testing.T) {
		t.Parallel()
		w := sumNewWorld(t, true)
		_, files := sumParseTxtar(t, "sync_bump_ok.txtar")
		w.apply(files)
		g := w.upCommit("bump")
		w.sync(g)
		w.commit("rules: sync")
		up := w.u.Clone(t, data.StatusOK)
		out := w.mustSummary("-base", w.base, "-upstream", up)
		sumWant(t, out, fmt.Sprintf("- Synced: the vendored bytes are upstream@%s's", g[:12]))
		sumGolden(t, "sync_bump_ok", out)
	})

	t.Run("summary_no_op_bump", func(t *testing.T) {
		t.Parallel()
		w := sumNewWorld(t, false)
		w.up["brave-lists/other.txt"] = []byte("changed\n")
		g := w.upCommit("another list only")
		w.l.SetGitlink(g) // sync writes nothing
		w.commit("bump")
		out := w.mustSummary("-base", w.base, "-upstream", w.u.Clone(t, data.StatusOK))
		sumWant(t, out, "**"+fmt.Sprintf(data.SumNoDataChange, g[:12])+"**",
			"- Compare: "+fmt.Sprintf(data.SumCompareURL, w.g0, g),
			fmt.Sprintf("- Last synced: `%s` → `%s`", w.g0[:12], w.g0[:12]))
		sumNotWant(t, out, data.SumNotSynced, "Changed vendored files", "Hold entries")
	})

	t.Run("summary_not_synced", func(t *testing.T) {
		t.Parallel()
		w := sumNewWorld(t, false)
		w.edit(cleanURLs, func(v any) any {
			sumAppend(sumRules(t, v)[0], "params", "new_tracker")
			return v
		})
		g := w.upCommit("clean-urls change")
		w.l.SetGitlink(g) // Dependabot's bump, not yet synced
		w.commit("bump")
		out := w.mustSummary("-base", w.base, "-upstream", w.u.Clone(t, data.StatusOK))
		sumWant(t, out, "**"+data.SumNotSynced+"** (differs from upstream@"+g[:12]+": rules/brave/clean-urls.json)")
		sumNotWant(t, out, "No data change")
	})

	t.Run("truncated_json", func(t *testing.T) {
		t.Parallel()
		w := sumNewWorld(t, false)
		w.up[cleanURLs] = w.up[cleanURLs][:1000]
		w.sync(w.upCommit("truncated"))
		w.commit("sync")
		out := w.mustSummary("-base", w.base)
		sumWant(t, out, "- note: head rules/brave/clean-urls.json: "+fmt.Sprintf(data.SumParseError, 1000))
		sumNotWant(t, out, "base rules/brave/clean-urls.json")
	})

	t.Run("keep_override_stale", func(t *testing.T) {
		t.Parallel()
		w := sumNewWorld(t, false)
		w.edit(cleanURLs, func(v any) any {
			sumRemove(t, sumRules(t, v)[sumFind(t, v, "*://www.wsj.com/*", nil)], "params", "st")
			return v
		})
		w.sync(w.upCommit("drop wsj st"))
		w.commit("sync")
		out := w.mustSummary("-base", w.base)
		sumWant(t, sumSection(t, out, "Overlay hygiene"), "- **new** stale keep-override: `wsj-gift-st` keeps `st`")
	})

	t.Run("rule_id_changed", func(t *testing.T) {
		t.Parallel()
		w := sumNewWorld(t, false)
		var first string
		w.edit(cleanURLs, func(v any) any {
			a := v.([]any)
			first = sumStrings(a[len(a)-1].(map[string]any)["include"])[0]
			return a[:len(a)-1]
		})
		w.sync(w.upCommit("drop a rule"))
		w.commit("sync")
		sec := sumSection(t, w.mustSummary("-base", w.base), "Rule IDs")
		sumWant(t, sec, "- `brave/clean-urls:"+first, "**"+data.SumDisableRules+"**")
		sumNotWant(t, sec, "Removed (0)")
	})

	t.Run("watch_block_changed", func(t *testing.T) {
		t.Parallel()
		w := sumNewWorld(t, false)
		w.commit("head")
		out := w.mustSummary("-base", w.base)
		sumWant(t, sumSection(t, out, "Watch"), "matches "+data.WatchPath, "unchanged")
		sumNotWant(t, out, data.SumWatchChanged)
		if w.watchURLs[0] != sumWatchURL || len(w.watchURLs) != 1 {
			t.Errorf("fetched %q, want one GET of %s", w.watchURLs, sumWatchURL)
		}

		w.watchBody = strings.Replace(sumUtilsCC, `{"a_param", "/a"},`, `{"a_param", "/a"}, {"b_param", "/b"},`, 1)
		sec := sumSection(t, w.mustSummary("-base", w.base), "Watch")
		sumWant(t, sec, "**"+data.SumWatchChanged+"**", "- `"+data.SumWatchConditional+"`: ", "- `"+data.SumWatchExempt+"`: unchanged")

		w.watchStatus = http.StatusInternalServerError
		out, code := w.summary("-base", w.base)
		if code != exitOK {
			t.Fatalf("fetch error: exit %d", code)
		}
		sumWant(t, sumSection(t, out, "Watch"), "note: brave-core fetch failed", "500")
		sumNotWant(t, out, data.SumWatchChanged)
	})

	t.Run("affiliate_candidate_added", func(t *testing.T) {
		t.Parallel()
		w := sumNewWorld(t, false)
		w.edit(cleanURLs, func(v any) any {
			sumAppend(sumRules(t, v)[sumFind(t, v, "*://*/*", nil)], "params", "irpid")
			return v
		})
		w.sync(w.upCommit("irpid"))
		w.commit("sync")
		sumWant(t, sumSection(t, w.mustSummary("-base", w.base), "Pairs"), "- ★⚑ (`global`, `irpid`) — affiliate-class candidate")
	})

	t.Run("risky_scoped_new_host", func(t *testing.T) {
		t.Parallel()
		w := sumNewWorld(t, false)
		w.edit(cleanURLs, func(v any) any {
			return append(v.([]any), map[string]any{"include": []any{"*://*.newhost.example/*"}, "exclude": []any{}, "params": []any{"t"}})
		})
		w.sync(w.upCommit("t on a new host"))
		w.commit("sync")
		sec := sumSection(t, w.mustSummary("-base", w.base), "Pairs")
		sumWant(t, sec, "Added (1):", "- ⚠ (`*://*.newhost.example/*`, `t`) — risky: never-strip")
	})

	t.Run("debounce_param_changed", func(t *testing.T) {
		t.Parallel()
		w := sumNewWorld(t, false)
		var idx int
		w.edit(debounce, func(v any) any {
			for i, r := range sumRules(t, v) {
				if r["param"] == "ued" {
					r["param"], idx = "dest", i
					return v
				}
			}
			t.Fatal("no debounce rule with param ued")
			return v
		})
		w.sync(w.upCommit("ued → dest"))
		w.commit("sync")
		sec := sumSection(t, w.mustSummary("-base", w.base), "Debounce")
		sumWant(t, sec, fmt.Sprintf("- rule %d (`redirect`, `dest`) has compiled patterns and changed param `ued` → `dest`", idx))
	})

	t.Run("debounce_stale_class", func(t *testing.T) {
		t.Parallel()
		w := sumNewWorld(t, false)
		var gone string
		w.edit(debounce, func(v any) any {
			count := map[string]int{}
			rules := sumRules(t, v)
			for _, r := range rules {
				for _, p := range sumStrings(r["include"]) {
					count[p]++
				}
			}
			for _, r := range rules {
				inc := sumStrings(r["include"])
				if p := inc[len(inc)-1]; len(inc) > 1 && count[p] == 1 {
					gone = p
					sumRemove(t, r, "include", p)
					return v
				}
			}
			t.Fatal("no removable pattern")
			return v
		})
		w.sync(w.upCommit("drop a pattern"))
		w.commit("sync")
		sec := sumSection(t, w.mustSummary("-base", w.base), "Debounce")
		sumWant(t, sec, "- stale classification: `"+gone+"`", "removed `"+gone+"`")
	})

	t.Run("debounce_unclassified_pattern", func(t *testing.T) {
		t.Parallel()
		w := sumNewWorld(t, false)
		const p = "*://www.newnet.example/click?*"
		var idx int
		w.edit(debounce, func(v any) any {
			idx = sumFind(t, v, "*://pvn.mediamarkt.de/trck/eclick/*", func(r map[string]any) bool { return r["action"] == "redirect" })
			sumAppend(sumRules(t, v)[idx], "include", p)
			return v
		})
		w.sync(w.upCommit("newnet"))
		w.commit("sync")
		sec := sumSection(t, w.mustSummary("-base", w.base), "Debounce")
		sumWant(t, sec,
			fmt.Sprintf("- `%s` (unclassified, rule %d): guessed affiliate ⚑, because rule %d already holds affiliate-classed patterns", p, idx, idx),
			fmt.Sprintf(`%q: {"class": "affiliate", "network": "<network>", "why": `, p))
	})

	t.Run("debounce_shared_pattern_uncompiled_rule", func(t *testing.T) {
		t.Parallel()
		w := sumNewWorld(t, false)
		const p = "*://pvn.mediamarkt.de/trck/eclick/*"
		w.edit(debounce, func(v any) any {
			i := sumFind(t, v, p, func(r map[string]any) bool { return r["action"] == "redirect" })
			sumRemove(t, sumRules(t, v)[i], "include", p)
			return v
		})
		w.sync(w.upCommit("drop pvn from the redirect rule"))
		w.commit("sync")
		out := w.mustSummary("-base", w.base)
		sec := sumSection(t, out, "Debounce")
		sumWant(t, sec, fmt.Sprintf("- `%s` (classed affiliate, but no rule holding it compiles in v1, rule ", p),
			"guessed off, because rule ",
			fmt.Sprintf(`%q: {"class": "off", "why": "action \"base64,redirect\" not supported in v1"}`, p))
		// Base, current data, needs no stub.
		w2 := sumNewWorld(t, false)
		w2.commit("head")
		sumNotWant(t, w2.mustSummary("-base", w2.base), "Classification stubs")
	})

	t.Run("params_moved_between_files", func(t *testing.T) {
		t.Parallel()
		w := sumNewWorld(t, false)
		var moved []string
		var qfGlobal map[string]any
		w.edit(queryFilter, func(v any) any {
			qfGlobal = sumRules(t, v)[sumFind(t, v, "*://*/*", nil)]
			return v
		})
		w.edit(cleanURLs, func(v any) any {
			r := sumRules(t, v)[sumFind(t, v, "*://*/*", nil)]
			have := sumStrings(qfGlobal["params"])
			for _, p := range sumStrings(r["params"]) {
				if len(moved) < 5 && !slices.Contains(have, p) && data.Canon(p) == p {
					moved = append(moved, p)
				}
			}
			for _, p := range moved {
				sumRemove(t, r, "params", p)
			}
			return v
		})
		w.edit(queryFilter, func(v any) any {
			r := sumRules(t, v)[sumFind(t, v, "*://*/*", nil)]
			for _, p := range moved {
				sumAppend(r, "params", p)
			}
			return v
		})
		w.sync(w.upCommit("move"))
		w.commit("sync")
		sec := sumSection(t, w.mustSummary("-base", w.base), "Counts")
		sumWant(t, sec, "Params moved between the two files (not removals):")
		for _, p := range moved {
			sumWant(t, sec, "(`global`, `"+p+"`): clean-urls → query-filter")
		}
		if u := regexp.MustCompile(`(?m)^\| union.*$`).FindString(sec); strings.Contains(u, "→") {
			t.Errorf("union counts changed: %s", u)
		}
	})

	// Holds: X is clean-urls.json with one more param, Y with two.
	holdWorld := func(t *testing.T) (w *sumWorld, x, y []byte) {
		w = sumNewWorld(t, false)
		orig := w.up[cleanURLs]
		w.edit(cleanURLs, func(v any) any { sumAppend(sumRules(t, v)[0], "params", "held_change"); return v })
		x = w.up[cleanURLs]
		w.edit(cleanURLs, func(v any) any { sumAppend(sumRules(t, v)[0], "params", "changed_again"); return v })
		y = w.up[cleanURLs]
		w.up[cleanURLs] = orig
		return w, x, y
	}
	holdJSON := func(sha, expires string) []byte {
		return fmt.Appendf(nil, `{"schema": 1, "acks": [], "holds": [{"file": "brave-lists/clean-urls.json", "sha256": %q, "expires": %q, "reason": "waiting on brave/adblock-lists#1", "by": "fixture"}]}`+"\n", sha, expires)
	}

	t.Run("hold_on_branch", func(t *testing.T) {
		t.Parallel()
		w, x, _ := holdWorld(t)
		w.up[cleanURLs] = x
		w.sync(w.upCommit("X"))
		w.l.Write(data.AcksPath, holdJSON(sumSHA(x), "2026-11-15"))
		w.commit("hold X")
		out := w.mustSummary("-base", w.base)
		sumWant(t, sumSection(t, out, "Header"),
			"- **Held**: "+fmt.Sprintf(data.MsgHeld, cleanURLs, sumSHA(x)[:12], "waiting on brave/adblock-lists#1", "2026-11-15"),
			fmt.Sprintf(`{"file": %q, "sha256": %q}`, cleanURLs, sumSHA(x)))
		sumNotWant(t, out, data.SumHoldLapsed)
	})

	t.Run("hold_on_main", func(t *testing.T) {
		// The hold lands on main after the PR was synced; the routine's
		// freshness merge brings it into the PR head.
		t.Parallel()
		w, x, _ := holdWorld(t)
		w.l.Run("checkout", "--quiet", "-b", "pr")
		w.up[cleanURLs] = x
		w.sync(w.upCommit("X"))
		w.commit("sync X")
		w.l.Run("checkout", "--quiet", "main")
		w.l.Write(data.AcksPath, holdJSON(sumSHA(x), "2026-11-15"))
		w.base = w.commit("hold X on main")
		w.l.Run("checkout", "--quiet", "pr")
		w.l.Run("merge", "--quiet", "--no-ff", "--no-edit", "main")
		out := w.mustSummary("-base", w.base)
		sumWant(t, sumSection(t, out, "Header"), "- **Held**: "+fmt.Sprintf(data.MsgHeld, cleanURLs, sumSHA(x)[:12], "waiting on brave/adblock-lists#1", "2026-11-15"))
	})

	t.Run("hold_lapsed_bytes_changed", func(t *testing.T) {
		t.Parallel()
		w, x, y := holdWorld(t)
		w.l.Write(data.AcksPath, holdJSON(sumSHA(x), "2026-11-15"))
		w.base = w.commit("hold X on main")
		w.up[cleanURLs] = y
		w.sync(w.upCommit("Y"))
		w.commit("sync Y")
		out := w.mustSummary("-base", w.base)
		sumWant(t, out, "- **"+data.SumHoldLapsed+"**: "+fmt.Sprintf(data.SumHoldBytesChanged, sumSHA(x)[:12], sumSHA(y)[:12]))
		sumNotWant(t, out, "**Held**")
	})

	t.Run("hold_expired", func(t *testing.T) {
		t.Parallel()
		w, x, _ := holdWorld(t)
		w.l.Write(data.AcksPath, holdJSON(sumSHA(x), "2026-10-19")) // the clock is 2026-10-20
		w.base = w.commit("hold X on main")
		w.up[cleanURLs] = x
		w.sync(w.upCommit("X"))
		w.commit("sync X")
		out := w.mustSummary("-base", w.base)
		sumWant(t, out, "- **"+data.SumHoldLapsed+"**: "+data.SumHoldExpired)
		sumNotWant(t, out, "**Held**")
	})

	t.Run("hold_stale_after_merge", func(t *testing.T) {
		t.Parallel()
		w, x, y := holdWorld(t)
		w.up[cleanURLs] = y
		w.sync(w.upCommit("Y"))
		w.l.Write(data.AcksPath, holdJSON(sumSHA(x), "2026-11-15"))
		w.base = w.commit("Y on main, hold X kept")
		w.up["brave-lists/other.txt"] = []byte("moved on\n")
		w.l.SetGitlink(w.upCommit("another list"))
		w.commit("no-op bump")
		out := w.mustSummary("-base", w.base)
		sumWant(t, out, "- "+data.SumHoldStale+": `"+cleanURLs+"` `"+sumSHA(x)[:12]+"`")
		sumNotWant(t, out, data.SumHoldLapsed, "**Held**")
	})

	t.Run("ack_on_branch_then_superseded", func(t *testing.T) {
		t.Parallel()
		w := sumNewWorld(t, false)
		w.edit(cleanURLs, func(v any) any { sumAppend(sumRules(t, v)[0], "params", "first_change"); return v })
		old := sumSHA(w.up[cleanURLs])
		w.sync(w.upCommit("first"))
		w.l.Write(data.AcksPath, fmt.Appendf(nil, `{"schema": 1, "acks": [{"check": "counts", "metric": "union.global_params", "min": 150, "vendored_sha256": %q, "expires": "2026-12-01", "reason": "upstream deduplicated", "by": "fixture"}], "holds": []}`+"\n", old))
		w.commit("ack on the branch")
		sumWant(t, sumSection(t, w.mustSummary("-base", w.base), "Acks"), "No lapsed or expired entries.")

		w.edit(cleanURLs, func(v any) any { sumAppend(sumRules(t, v)[0], "params", "upstream_next_week"); return v })
		w.sync(w.upCommit("next week"))
		w.commit("later PR, synced")
		sumWant(t, sumSection(t, w.mustSummary("-base", w.base), "Acks"), "- "+fmt.Sprintf(data.SumAckLapsed, old)+": acks[0] counts `union.global_params`")
	})

	t.Run("sync_after_upstream_rewrite", func(t *testing.T) {
		t.Parallel()
		w := sumNewWorld(t, false)
		b := w.g0 // base's gitlink and lock commit
		// Upstream rewrites master: a new root with the same files.
		w.tip = ""
		w.day += 10
		g := w.upCommit("rewritten")
		w.u.Prune()
		w.sync(g) // the lock's commit moves to G'; the bytes stay
		w.commit("sync after the rewrite")
		out := w.mustSummary("-base", w.base, "-upstream", w.u.Clone(t, data.StatusOK))
		sumWant(t, out, "- **"+fmt.Sprintf(data.SumHistoryRewritten, b[:12])+"**",
			"**"+fmt.Sprintf(data.SumNoDataChange, g[:12])+"**")

		// Without the clone the line is omitted.
		sumNotWant(t, w.mustSummary("-base", w.base), "history rewritten")
		// Before the rewrite, base's gitlink is on master.
		w2 := sumNewWorld(t, false)
		w2.commit("head")
		sumNotWant(t, w2.mustSummary("-base", w2.base, "-upstream", w2.u.Clone(t, data.StatusOK)), "history rewritten")
	})
}

// TestSummaryUsage covers the flag and input errors.
func TestSummaryUsage(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"summary"}, exitUsage, "-root is required"},
		{[]string{"summary", "-root", ".", "-base", "x"}, exitUsage, "-out is required"},
		{[]string{"summary", "-root", t.TempDir(), "-base", "--upload-pack=x", "-out", "-"}, exitFail, "neither a directory nor a ref"},
		{[]string{"summary", "-root", t.TempDir(), "-base", "no-such-ref", "-out", "-"}, exitFail, "rulegen summary:"},
	} {
		e, _, stderr := newTestEnv(t, nil)
		if code := run(ctx, e, tt.args); code != tt.code || !strings.Contains(stderr.String(), tt.msg) {
			t.Errorf("%q: exit %d, stderr %q; want %d, %q", tt.args, code, stderr, tt.code, tt.msg)
		}
	}
}

// TestSummaryNoEngine: when TestSummaryData cannot run, summary still writes
// every section it computes itself and a note for the rest.
func TestSummaryNoEngine(t *testing.T) {
	g := repo.Exec{Env: gittest.Isolate(t)}
	l := gittest.NewLinkclean(t, g, gittest.Opts{})
	e, _, _ := newTestEnv(t, nil)
	e.Go = func(context.Context, string, []string, ...string) ([]byte, []byte, error) {
		return []byte("FAIL\tbuild failed\n"), nil, fmt.Errorf("exit status 1")
	}
	out := filepath.Join(t.TempDir(), "s.md")
	if code := run(context.Background(), e, []string{"summary", "-root", l.Work.Dir, "-base", "HEAD", "-out", out, "-validate"}); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	sumWant(t, s, data.SummaryMarker+"\n", "### Counts", "| clean-urls | 51", "note: engine data unavailable", "build failed",
		"### Preflight", "**failed** (exit status 1)", "note: "+data.WatchPath+" unreadable")
}

func TestSumWatchBlock(t *testing.T) {
	b, ok := sumWatchBlock([]byte(sumUtilsCC), data.SumWatchConditional)
	want := "static constexpr auto kConditionalQueryStringTrackers =\n    MakeMap({\n        {\"a_param\", \"/a\"},\n    });\n"
	if !ok || string(b) != want {
		t.Errorf("conditional block %q, %v", b, ok)
	}
	b, ok = sumWatchBlock([]byte(sumUtilsCC), data.SumWatchExempt)
	if want := "static constexpr auto kExemptedHostnames =\n    MakeSet({\"exempt.example\"});\n"; !ok || string(b) != want {
		t.Errorf("exempt block %q, %v", b, ok)
	}
	for _, src := range []string{"", "x = kExemptedHostnames;\n", "auto kExemptedHostnamesX = 1;\n", "auto kExemptedHostnames =\n {\n"} {
		if _, ok := sumWatchBlock([]byte(src), data.SumWatchExempt); ok {
			t.Errorf("found a block in %q", src)
		}
	}
	// The committed watch.json is well-formed and records both blocks.
	var w sumWatchFile
	b, err := os.ReadFile("watch.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(w.Source, "https://raw.githubusercontent.com/brave/brave-core/") || !strings.HasSuffix(w.Source, "/"+data.SumWatchSource) {
		t.Errorf("watch.json source %q", w.Source)
	}
	for _, id := range sumWatchBlocks {
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(w.Blocks[id]) {
			t.Errorf("watch.json %s = %q", id, w.Blocks[id])
		}
	}
	if len(w.Blocks) != len(sumWatchBlocks) {
		t.Errorf("watch.json blocks %v", w.Blocks)
	}
}

func TestSumHeuristic(t *testing.T) {
	for name, want := range map[string]bool{
		"affid": true, "irpid": true, "impradid": true, "cjevent": true, "awc": true, "sscid": true,
		"ranmid": true, "xrefx": true, "refcode": true, "partnerid": true, "clickid": true, "campaign": true,
		"mkevt": true, "tag": true, "linkcode": true,
		"url": false, "tags": false, "utm_source": false, "click": false, "id": false, "": false,
	} {
		if got := sumAffiliateName(name); got != want {
			t.Errorf("sumAffiliateName(%q) = %v", name, got)
		}
	}
	if !sumAffiliateHost("go.partnerize.example") || sumAffiliateHost("www.newnet.example") {
		t.Error("sumAffiliateHost")
	}
	for _, tt := range []struct {
		g, s string
		want bool
	}{{"a*b*c", "abc", true}, {"a*b*c", "aXbYc", true}, {"a*a", "a", false}, {"*", "", true}, {"x", "x", true}, {"*ref*", "ref", true}} {
		if got := sumGlob(tt.g, tt.s); got != tt.want {
			t.Errorf("sumGlob(%q, %q) = %v", tt.g, tt.s, got)
		}
	}
}

// TestSumNeverStripCopy checks sumNeverStrip against the root gate's list.
func TestSumNeverStripCopy(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(gittest.Root(t), "rules_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?s)var neverStrip = strings.Fields\\(`([^`]*)`\\)").FindSubmatch(b)
	if m == nil {
		t.Fatal("rules_test.go: no neverStrip list")
	}
	if got := strings.Fields(string(m[1])); !slices.Equal(got, sumNeverStrip) {
		t.Errorf("root neverStrip %q, rulegen %q", got, sumNeverStrip)
	}
}
