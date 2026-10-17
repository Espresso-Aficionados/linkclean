package repo_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/gittest"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
)

func TestBlobID(t *testing.T) {
	// git hash-object of the empty blob and of "hello\n".
	for in, want := range map[string]string{
		"":        "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391",
		"hello\n": "ce013625030ba8dba906f756967f9e9ca394464a",
	} {
		if got := repo.BlobID([]byte(in)); got != want {
			t.Errorf("BlobID(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestRepo(t *testing.T) {
	ctx := context.Background()
	g := repo.Exec{Env: gittest.Isolate(t)}
	u := gittest.NewUpstream(t, g)
	when := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	files := map[string][]byte{"a/x.json": []byte("x\n"), "a/b/y": []byte("y\n"), "ab": []byte("ab\n"), "a.txt": []byte("t\n")}
	c1 := u.Commit("", files, when, "one\n\nRulegen-Sync: 0123\nRulegen-Merge:   main  ")
	files2 := map[string][]byte{"a/x.json": []byte("x2\n"), "a/b/y": []byte("y\n"), "new": []byte("n\n")}
	c2 := u.Commit(c1, files2, when.Add(time.Hour), "two")
	side := u.Commit(c1, map[string][]byte{"a/x.json": []byte("x\n"), "a/b/y": []byte("y\n"), "ab": []byte("ab\n"), "a.txt": []byte("t\n"), "side": []byte("s\n")}, when, "side")
	conflict := u.Commit(c1, map[string][]byte{"a/x.json": []byte("other\n")}, when, "conflict")
	u.SetRef("refs/heads/master", c2)
	r := u.Repo()

	if got, err := r.Out(ctx, "rev-parse", "refs/heads/master"); err != nil || got != c2 {
		t.Errorf("Out = %q, %v", got, err)
	}

	for spec, want := range map[string]bool{
		"refs/heads/master":     true,
		c1:                      true,
		c1 + ":a/x.json":        true,
		c1 + ":a/nope":          false,
		"refs/heads/nope":       false,
		strings.Repeat("1", 40): false, // well-formed, absent
	} {
		sha, ok, err := r.RevParse(ctx, spec)
		if err != nil || ok != want || ok && len(sha) != 40 {
			t.Errorf("RevParse(%q) = %q, %v, %v; want ok=%v", spec, sha, ok, err, want)
		}
	}
	if sha, _, _ := r.RevParse(ctx, c1+":a/x.json"); sha != repo.BlobID([]byte("x\n")) {
		t.Errorf("RevParse blob = %s", sha)
	}

	if b, ok, err := r.Show(ctx, c1, "a/x.json"); err != nil || !ok || string(b) != "x\n" {
		t.Errorf("Show = %q, %v, %v", b, ok, err)
	}
	if _, ok, err := r.Show(ctx, c1, "a/nope"); err != nil || ok {
		t.Errorf("Show absent: %v, %v", ok, err)
	}
	if _, ok, err := r.Show(ctx, c1, "a"); err == nil || ok {
		t.Errorf("Show tree: %v, %v; want an error", ok, err)
	}
	if _, _, err := r.Show(ctx, "refs/heads/nope", "a/x.json"); err == nil {
		t.Error("Show of a bad rev succeeded")
	}
	if _, ok, err := r.Gitlink(ctx, c1, "a/x.json"); err != nil || ok {
		t.Errorf("Gitlink of a blob: %v, %v", ok, err)
	}

	ls, err := r.LsTree(ctx, c1, "a")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range ls {
		paths = append(paths, e.Path)
		if e.Mode != "100644" || e.Type != "blob" || len(e.Object) != 40 {
			t.Errorf("entry %+v", e)
		}
	}
	if want := []string{"a/b/y", "a/x.json"}; !slices.Equal(paths, want) {
		t.Errorf("LsTree(a) = %v, want %v (not ab or a.txt)", paths, want)
	}
	if all, err := r.LsTree(ctx, c1, ""); err != nil || len(all) != 4 {
		t.Errorf("LsTree all = %v, %v", all, err)
	}

	for _, tt := range []struct {
		a, b string
		want bool
	}{{c1, c2, true}, {c2, c2, true}, {c2, c1, false}, {side, c2, false}} {
		if got, err := r.IsAncestor(ctx, tt.a, tt.b); err != nil || got != tt.want {
			t.Errorf("IsAncestor(%.7s, %.7s) = %v, %v", tt.a, tt.b, got, err)
		}
	}
	if _, err := r.IsAncestor(ctx, strings.Repeat("1", 40), c2); err == nil {
		t.Error("IsAncestor of an absent commit: no error")
	}

	if ps, err := r.Parents(ctx, c1); err != nil || len(ps) != 0 {
		t.Errorf("Parents(root) = %v, %v", ps, err)
	}
	if tree, err := r.Tree(ctx, c1); err != nil || len(tree) != 40 {
		t.Errorf("Tree = %q, %v", tree, err)
	}
	if d, err := r.CommitDate(ctx, c2); err != nil || d != "2026-10-01T13:00:00Z" && d != "2026-10-01T13:00:00+00:00" {
		t.Errorf("CommitDate = %q, %v", d, err)
	}
	if ts, err := r.Trailers(ctx, c1); err != nil || !slices.Equal(ts, [][2]string{{"Rulegen-Sync", "0123"}, {"Rulegen-Merge", "main"}}) {
		t.Errorf("Trailers = %q, %v", ts, err)
	}
	if ts, err := r.Trailers(ctx, c2); err != nil || len(ts) != 0 {
		t.Errorf("Trailers(no trailers) = %q, %v", ts, err)
	}
	if names, err := r.DiffNames(ctx, c1, c2); err != nil || !slices.Equal(names, []string{"a.txt", "a/x.json", "ab", "new"}) {
		t.Errorf("DiffNames = %q, %v", names, err)
	}

	tree, clean, err := r.MergeTree(ctx, c2, side)
	if err != nil || !clean {
		t.Fatalf("MergeTree clean: %v, %v", clean, err)
	}
	want := u.Commit("", map[string][]byte{"a/x.json": []byte("x2\n"), "a/b/y": []byte("y\n"), "new": []byte("n\n"), "side": []byte("s\n")}, when, "w")
	if wt, _ := r.Tree(ctx, want); wt != tree {
		t.Errorf("MergeTree tree %s, want %s", tree, wt)
	}
	if tree, clean, err := r.MergeTree(ctx, c2, conflict); err != nil || clean || len(tree) != 40 {
		t.Errorf("MergeTree conflict: %q, %v, %v", tree, clean, err)
	}
	if _, _, err := r.MergeTree(ctx, c2, "refs/heads/nope"); err == nil {
		t.Error("MergeTree of a bad rev: no error")
	}

	_, err = r.Bytes(ctx, "rev-parse", "--verify", "refs/heads/nope")
	var ee *repo.ExitError
	if !errors.As(err, &ee) || ee.Code != 128 || repo.ExitCode(err) != 128 || !strings.Contains(err.Error(), "git rev-parse --verify refs/heads/nope") {
		t.Errorf("exit error = %#v", err)
	}
	if repo.ExitCode(errors.New("x")) != -1 || repo.ExitCode(nil) != 0 {
		t.Error("ExitCode of a non-exit error")
	}

	// Fetch and Gitlink, through a linkclean copy whose gitlink names c2.
	l := gittest.NewLinkclean(t, g, gittest.Opts{})
	l.SetGitlink(c2)
	head := l.Commit("bump")
	if sha, ok, err := l.Work.Gitlink(ctx, head, "tools/upstream/adblock-lists"); err != nil || !ok || sha != c2 {
		t.Errorf("Gitlink = %q, %v, %v", sha, ok, err)
	}
	if _, ok, err := l.Work.Show(ctx, head, "tools/upstream/adblock-lists"); err == nil || ok {
		t.Errorf("Show of a gitlink: %v, %v; want an error", ok, err)
	}
	l.Run("remote", "add", "up", u.URL)
	if err := l.Work.Fetch(ctx, "up", c1); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := l.Work.RevParse(ctx, c1); !ok {
		t.Error("fetched commit missing")
	}
	if err := l.Work.Fetch(ctx, "up", strings.Repeat("1", 40)); err == nil {
		t.Error("fetch of an absent commit succeeded")
	}
}
