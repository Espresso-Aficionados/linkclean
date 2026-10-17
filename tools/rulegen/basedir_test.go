package main

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/basetree"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/gittest"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
)

// basedirRun runs rulegen basedir on l with an event payload and returns
// the output directory, the exit code and stderr.
func basedirRun(t *testing.T, l *gittest.Linkclean, event, payload string) (out string, code int, stderr string) {
	t.Helper()
	ev := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(ev, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	out = filepath.Join(t.TempDir(), "base")
	e, _, errb := newTestEnv(t, nil)
	code = run(context.Background(), e, []string{"basedir", "-root", l.Work.Dir, "-event", event, "-event-path", ev, "-out", out})
	return out, code, errb.String()
}

// basedirTree lists every file under dir as slash path → content.
func basedirTree(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	m := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		m[filepath.ToSlash(rel)] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// basedirMarked commits a marker into rules/acks.json, so each commit's
// output set is recognizable.
func basedirMarked(l *gittest.Linkclean, mark string) string {
	l.Write(data.AcksPath, []byte(`{"schema": 1, "acks": [], "holds": [], "x": "`+mark+`"}`+"\n"))
	return l.Commit(mark)
}

func basedirMark(t *testing.T, out string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(data.AcksPath)))
	if err != nil {
		t.Fatal(err)
	}
	_, m, _ := strings.Cut(string(b), `"x": "`)
	m, _, _ = strings.Cut(m, `"`)
	return m
}

// TestBasedirFixtures runs the §11.8 basedir rows.
func TestBasedirFixtures(t *testing.T) {
	g := repo.Exec{Env: gittest.Isolate(t)}

	t.Run("push_event_base", func(t *testing.T) {
		l := gittest.NewLinkclean(t, g, gittest.Opts{})
		a := basedirMarked(l, "A")
		basedirMarked(l, "B")
		basedirMarked(l, "C")
		out, code, stderr := basedirRun(t, l, "push", fmt.Sprintf(`{"before": %q, "after": "x"}`, a))
		if code != exitOK || basedirMark(t, out) != "A" {
			t.Errorf("before=A: exit %d (%s), base %q", code, stderr, basedirMark(t, out))
		}
		out, code, stderr = basedirRun(t, l, "push", `{"before": "0000000000000000000000000000000000000000"}`)
		if code != exitOK || basedirMark(t, out) != "B" {
			t.Errorf("all-zero before: exit %d (%s), base %q, want HEAD^ (B)", code, stderr, basedirMark(t, out))
		}
	})

	t.Run("basedir_output_set", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		l := gittest.NewLinkclean(t, g, gittest.Opts{})
		before := basedirMarked(l, "before-submodule")
		const sha = "8846788d578bf89696ad4ec5cdff845dfb170f1a"
		l.SetGitlink(sha)
		l.WriteGitmodules(data.CanonicalURL, data.Branch)
		l.Write("rules/overlay/extra.json", []byte("[]\n"))
		l.Write("testdata/corpus/eval-2026-10/x.json", []byte("[]\n"))
		l.Write("README.md", []byte("x\n"))
		after := basedirMarked(l, "after-submodule")

		out, code, stderr := basedirRun(t, l, "pull_request", fmt.Sprintf(`{"pull_request": {"base": {"sha": %q}, "head": {"sha": "x"}}}`, after))
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, stderr)
		}
		got := basedirTree(t, out)
		want := map[string][]byte{data.GitlinkFile: []byte(sha + "\n")}
		es, err := l.Work.LsTree(ctx, after, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			if e.Path == data.LockPath || e.Path == data.GitmodulesPath || strings.HasPrefix(e.Path, "rules/") {
				b, err := l.Work.Blob(ctx, e.Object)
				if err != nil {
					t.Fatal(err)
				}
				want[e.Path] = b
			}
		}
		if !maps.EqualFunc(got, want, bytes.Equal) {
			t.Errorf("basedir wrote %q, want %q", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
		}
		for _, p := range []string{data.GitmodulesPath, data.GitlinkFile, "rules/overlay/extra.json", data.AcksPath, data.LockPath, data.Files[0].Vendored} {
			if _, ok := got[p]; !ok {
				t.Errorf("output set lacks %s", p)
			}
		}

		out, code, stderr = basedirRun(t, l, "pull_request", fmt.Sprintf(`{"pull_request": {"base": {"sha": %q}}}`, before))
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, stderr)
		}
		got = basedirTree(t, out)
		for _, p := range []string{data.GitmodulesPath, data.GitlinkFile} {
			if _, ok := got[p]; ok {
				t.Errorf("base before the submodule PR: %s written", p)
			}
		}
		if _, ok := got[data.LockPath]; !ok || basedirMark(t, out) != "before-submodule" {
			t.Errorf("base before the submodule PR: wrong set %q", slices.Sorted(maps.Keys(got)))
		}

		// summary -base <dir> and -base <ref> print the same text (CT-1).
		w := sumNewWorld(t, false)
		w.edit("brave-lists/clean-urls.json", func(v any) any {
			sumAppend(sumRules(t, v)[sumFind(t, v, "*://*/*", nil)], "params", "new_tracker")
			return v
		})
		w.sync(w.upCommit("change"))
		w.commit("head")
		dir := filepath.Join(t.TempDir(), "base")
		if err := basetree.Write(context.Background(), w.l.Work, w.base, dir); err != nil {
			t.Fatal(err)
		}
		up := w.u.Clone(t, data.StatusOK)
		byDir := w.mustSummary("-base", dir, "-upstream", up)
		byRef := w.mustSummary("-base", w.base, "-upstream", up)
		if byDir != byRef {
			t.Errorf("-base <dir> and -base <ref> differ:\n%s", sumLineDiff(byDir, byRef))
		}
		sumWant(t, byDir, "`global`, `new_tracker`")
	})

	t.Run("basedir_other_event", func(t *testing.T) {
		l := gittest.NewLinkclean(t, g, gittest.Opts{})
		head := l.Run("rev-parse", "HEAD")
		for _, ev := range []string{"workflow_dispatch", "schedule", "pull_request_target"} {
			payload := fmt.Sprintf(`{"before": %q, "pull_request": {"base": {"sha": %q}}}`, head, head)
			out, code, stderr := basedirRun(t, l, ev, payload)
			if code != exitFail || !strings.Contains(stderr, "has no base") {
				t.Errorf("%s: exit %d, stderr %q; want 1", ev, code, stderr)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Errorf("%s: output written (%v)", ev, err)
			}
		}
	})

	t.Run("pull_request_multi_commit_branch", func(t *testing.T) {
		// The PR branch holds Dependabot's gitlink commit, a sync commit that
		// adds id to the global rule, and a later merge of main. Base from
		// the payload is main, so Risky fails; HEAD^ (the merge's first
		// parent, the sync commit) would let the pair through.
		t.Parallel()
		if testing.Short() {
			t.Skip("builds the root module")
		}
		w := sumNewWorld(t, false)
		w.l.Push("main")
		w.l.Run("checkout", "--quiet", "-b", "dependabot/submodules/x")
		w.edit("brave-lists/clean-urls.json", func(v any) any {
			sumAppend(sumRules(t, v)[sumFind(t, v, "*://*/*", nil)], "params", "id")
			return v
		})
		g := w.upCommit("adds id globally")
		w.l.SetGitlink(g)
		w.commit("Bump tools/upstream/adblock-lists")
		w.l.SyncedTo(w.u, g)
		w.l.Commit(fmt.Sprintf(data.SyncSubject, g[:data.ShortLen], "2026-09-03"), data.TrailerSync+": "+g)
		w.l.Run("checkout", "--quiet", "main")
		w.l.Write("README.md", []byte("main moved\n"))
		mainTip := w.l.Commit("main moves")
		w.l.Run("checkout", "--quiet", "dependabot/submodules/x")
		w.l.Run("-c", "user.name=t", "-c", "user.email=t@example.invalid", "merge", "--quiet", "--no-ff", "--no-edit", "main")

		out, code, stderr := basedirRun(t, w.l, "pull_request", fmt.Sprintf(`{"pull_request": {"base": {"sha": %q}}}`, mainTip))
		if code != exitOK || basedirTree(t, out)[data.GitlinkFile] == nil {
			t.Fatalf("basedir: exit %d: %s", code, stderr)
		}
		res, ok := w.l.GoTest(t, "^TestRules$/^Risky$", map[string]string{"LINKCLEAN_BASE_DIR": out})
		if ok || !strings.Contains(res, "Risky: new global pair (global, id)") {
			t.Errorf("Risky against the payload base: ok=%v\n%s", ok, res)
		}

		parent := filepath.Join(t.TempDir(), "parent")
		if err := basetree.Write(context.Background(), w.l.Work, "HEAD^", parent); err != nil {
			t.Fatal(err)
		}
		if res, ok := w.l.GoTest(t, "^TestRules$/^Risky$", map[string]string{"LINKCLEAN_BASE_DIR": parent}); !ok {
			t.Errorf("Risky against HEAD^ (the hazard the payload base avoids) failed:\n%s", res)
		}
	})
}

func TestBasedirPayloads(t *testing.T) {
	l := gittest.NewLinkclean(t, repo.Exec{Env: gittest.Isolate(t)}, gittest.Opts{})
	head := l.Run("rev-parse", "HEAD")
	for _, tt := range []struct{ event, payload, msg string }{
		{"pull_request", `{}`, "no pull_request.base.sha"},
		{"pull_request", `{"pull_request": {"base": {"sha": "HEAD"}}}`, "not a commit id"},
		{"pull_request", `{"pull_request": {"base": {"sha": "--output=x"}}}`, "not a commit id"},
		{"pull_request", `{"pull_request": {"base": {"sha": "` + strings.Repeat("a", 40) + `"}}}`, "fetch it first"},
		{"push", `{}`, "no before"},
		{"push", `{"before": "main"}`, "not a commit id"},
		{"push", `[`, "event payload"},
		{"push", `{"before": "0000000000000000000000000000000000000000"}`, "fetch it first"}, // HEAD has no parent
	} {
		if _, code, stderr := basedirRun(t, l, tt.event, tt.payload); code != exitFail || !strings.Contains(stderr, tt.msg) {
			t.Errorf("%s %s: exit %d, stderr %q; want 1, %q", tt.event, tt.payload, code, stderr, tt.msg)
		}
	}
	if _, code, stderr := basedirRun(t, l, "pull_request", `{"pull_request": {"base": {"sha": "`+head+`"}}}`); code != exitOK {
		t.Errorf("valid payload: exit %d: %s", code, stderr)
	}
	e, _, stderr := newTestEnv(t, nil)
	if code := run(context.Background(), e, []string{"basedir", "-root", "."}); code != exitUsage || !strings.Contains(stderr.String(), "-event is required") {
		t.Errorf("missing flags: exit %d, %q", code, stderr)
	}
}
