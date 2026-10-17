package gittest

import (
	"bytes"
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
)

var when = time.Date(2026, 10, 14, 19, 19, 33, 0, time.FixedZone("", -7*3600))

func newGit(t *testing.T) repo.Git { return repo.Exec{Env: Isolate(t)} }

func TestIsolate(t *testing.T) {
	g := newGit(t)
	ctx := context.Background()
	if out, err := g.Run(ctx, "", nil, "config", "--global", "--list"); err == nil && len(bytes.TrimSpace(out)) > 0 {
		t.Errorf("global config visible: %s", out)
	}
	// Only file:// is allowed, and the check precedes any connection.
	_, err := g.Run(ctx, t.TempDir(), nil, "clone", "--quiet", "https://github.com/brave/adblock-lists", "x")
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("https clone: %v, want a transport refusal", err)
	}
}

func TestUpstream(t *testing.T) {
	g := newGit(t)
	ctx := context.Background()
	u := NewUpstream(t, g)
	files := RealData(t)
	c1 := u.Commit("", files, when, "one")
	u.SetRef("refs/heads/master", c1)
	ur := u.Repo()
	if d, err := ur.CommitDate(ctx, c1); err != nil || d != "2026-10-14T19:19:33-07:00" {
		t.Errorf("CommitDate = %q, %v", d, err)
	}
	for _, s := range data.Files {
		b, ok, err := ur.Show(ctx, c1, s.Upstream)
		if err != nil || !ok || !bytes.Equal(b, files[s.Upstream]) {
			t.Errorf("%s: ok=%v err=%v, bytes equal %v", s.Upstream, ok, err, bytes.Equal(b, files[s.Upstream]))
		}
	}
	// Deterministic: the same tree, parent, date and message give the same id.
	if again := u.Commit("", files, when, "one"); again != c1 {
		t.Errorf("Commit not deterministic: %s vs %s", again, c1)
	}

	next := maps.Clone(files)
	next["brave-lists/other.txt"] = []byte("x\n")
	c2 := u.Commit(c1, next, when.Add(time.Hour), "two")
	pr := u.Commit(c1, map[string][]byte{"brave-lists/clean-urls.json": []byte("[]")}, when, "pr")
	u.SetRef("refs/heads/master", c2)
	u.SetRef("refs/pull/1/head", pr)
	if ps, err := ur.Parents(ctx, c2); err != nil || len(ps) != 1 || ps[0] != c1 {
		t.Errorf("Parents(c2) = %v, %v", ps, err)
	}

	dir := u.Clone(t, data.StatusOK)
	cl := repo.Repo{Git: g, Dir: dir}
	if b, err := os.ReadFile(filepath.Join(dir, data.StatusFile)); err != nil || string(b) != "ok\n" {
		t.Errorf("STATUS = %q, %v", b, err)
	}
	if got, err := cl.Out(ctx, "rev-parse", data.MasterRef); err != nil || got != c2 {
		t.Errorf("%s = %q, %v; want %s", data.MasterRef, got, err, c2)
	}
	if _, err := os.Stat(filepath.Join(dir, "brave-lists")); !os.IsNotExist(err) {
		t.Errorf("clone has a worktree: %v", err)
	}
	types, err := cl.Out(ctx, "cat-file", "--batch-all-objects", "--batch-check=%(objecttype)")
	if err != nil {
		t.Fatal(err)
	}
	for ty := range strings.SplitSeq(types, "\n") {
		if ty != "commit" && ty != "tree" {
			t.Errorf("blobless clone holds a %s", ty)
		}
	}
	// An unmerged PR head is served by id (allowAnySHA1InWant)...
	if err := cl.Fetch(ctx, "origin", pr); err != nil {
		t.Errorf("fetch PR head: %v", err)
	}
	if ok, err := cl.IsAncestor(ctx, pr, data.MasterRef); err != nil || ok {
		t.Errorf("PR head ancestor of master: %v, %v", ok, err)
	}
	// ...and a blob is fetched on demand.
	if b, ok, err := cl.Show(ctx, c2, "brave-lists/other.txt"); err != nil || !ok || string(b) != "x\n" {
		t.Errorf("lazy blob: %q %v %v", b, ok, err)
	}

	// A rewrite plus Prune: the dropped commit is no longer served.
	rw := u.Commit("", files, when.Add(2*time.Hour), "rewritten")
	u.SetRef("refs/heads/master", rw)
	u.SetRef("refs/pull/1/head", rw)
	u.Prune()
	fresh := repo.Repo{Git: g, Dir: u.Clone(t, data.StatusOK)}
	err = fresh.Fetch(ctx, "origin", c2)
	if err == nil {
		t.Error("pruned commit still served")
	}
	if ok, err := fresh.IsAncestor(ctx, c1, data.MasterRef); err == nil && ok {
		t.Error("old root still on master")
	}
}

func TestLinkclean(t *testing.T) {
	g := newGit(t)
	ctx := context.Background()
	l := NewLinkclean(t, g, Opts{})
	w := l.Work
	files, err := w.LsTree(ctx, "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range files {
		if e.Path != data.LockPath && !strings.HasPrefix(e.Path, "rules/") {
			t.Errorf("non-FullModule copy has %s", e.Path)
		}
	}
	if b, ok, err := w.Show(ctx, "HEAD", data.LockPath); err != nil || !ok || !bytes.Equal(b, readRoot(t, data.LockPath)) {
		t.Errorf("lock not copied: %v %v", ok, err)
	}
	if got, err := w.Out(ctx, "rev-parse", "refs/remotes/origin/main"); err != nil || got != l.Run("rev-parse", "HEAD") {
		t.Errorf("origin/main = %q, %v", got, err)
	}

	u := NewUpstream(t, g)
	up := RealData(t)
	up["brave-lists/clean-urls.json"] = []byte(`[{"include": ["*://*/*"], "exclude": [], "params": ["a", "b"]}]`)
	c := u.Commit("", up, when, "c")
	u.SetRef("refs/heads/master", c)

	l.SetGitlink(c)
	l.WriteGitmodules(data.CanonicalURL, data.Branch)
	l.SyncedTo(u, c)
	head := l.Commit("rules: sync", data.TrailerSync+": "+c)
	if got, ok, err := w.Gitlink(ctx, head, data.Submodule); err != nil || !ok || got != c {
		t.Errorf("Gitlink = %q %v %v, want %s", got, ok, err, c)
	}
	gm, _, _ := w.Show(ctx, head, data.GitmodulesPath)
	if err := data.CheckMapping(gm); err != nil {
		t.Errorf("WriteGitmodules: %v", err)
	}
	lb, _, _ := w.Show(ctx, head, data.LockPath)
	lock, err := data.ParseLock(lb)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Upstream.Commit != c || lock.Upstream.CommitDate != "2026-10-14T19:19:33-07:00" {
		t.Errorf("lock commit %s %s", lock.Upstream.Commit, lock.Upstream.CommitDate)
	}
	if got := lock.Upstream.Files["brave-lists/clean-urls.json"].Counts; got != (data.Counts{Rules: 1, Params: 2, GlobalParams: 2}) {
		t.Errorf("counts %+v", got)
	}
	nb, _, _ := w.Show(ctx, head, data.NoticePath)
	if !bytes.Equal(nb, data.Notice(&lock)) {
		t.Error("NOTICE not regenerated")
	}
	if ts, err := w.Trailers(ctx, head); err != nil || len(ts) != 1 || ts[0] != [2]string{data.TrailerSync, c} {
		t.Errorf("Trailers = %v, %v", ts, err)
	}
	// Further commits keep the unpopulated gitlink.
	l.Write("rules/acks.json", []byte("{}\n"))
	next := l.Commit("acks")
	if _, ok, _ := w.Gitlink(ctx, next, data.Submodule); !ok {
		t.Error("gitlink dropped by a later commit")
	}
	l.Remove("rules/acks.json")
	gone := l.Commit("rm")
	if _, ok, _ := w.Show(ctx, gone, "rules/acks.json"); ok {
		t.Error("Remove did not remove")
	}
	l.Push("HEAD:refs/heads/dependabot/x")
	if got := run(t, g, l.Origin, nil, "rev-parse", "refs/heads/dependabot/x"); got != gone {
		t.Errorf("pushed %s, want %s", got, gone)
	}
}

func TestGoTest(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the root module")
	}
	l := NewLinkclean(t, newGit(t), Opts{FullModule: true})
	files, err := l.Work.LsTree(context.Background(), "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range files {
		if strings.HasPrefix(e.Path, "tools/") || strings.HasPrefix(e.Path, ".github/") {
			t.Errorf("FullModule copied %s", e.Path)
		}
	}
	const run = "^TestRules$/^Provenance$"
	t.Setenv("LINKCLEAN_BASE_DIR", filepath.Join(t.TempDir(), "absent")) // must not leak into the run
	out, ok := l.GoTest(t, run, nil)
	if !ok || !strings.Contains(out, "--- PASS: TestRules/Provenance") {
		t.Errorf("go test failed:\n%s", out)
	}
	l.Write("rules/brave/clean-urls.json", []byte("[]"))
	if out, ok := l.GoTest(t, run, nil); ok || !strings.Contains(out, "--- FAIL: TestRules/Provenance") {
		t.Errorf("go test passed on a hand-edited vendored file:\n%s", out)
	}
}

func readRoot(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(Root(t), filepath.FromSlash(p)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
