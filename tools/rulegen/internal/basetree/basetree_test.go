package basetree_test

import (
	"bytes"
	"context"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/basetree"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/gittest"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
)

// tree lists every file under dir as slash path → content.
func tree(t *testing.T, dir string) map[string][]byte {
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

func TestWrite(t *testing.T) {
	ctx := context.Background()
	g := repo.Exec{Env: gittest.Isolate(t)}
	l := gittest.NewLinkclean(t, g, gittest.Opts{})
	before := l.Run("rev-parse", "HEAD") // no .gitmodules, no gitlink

	// A base after the submodule PR, with files outside the output set.
	const sha = "8846788d578bf89696ad4ec5cdff845dfb170f1a"
	l.SetGitlink(sha)
	l.WriteGitmodules(data.CanonicalURL, data.Branch)
	l.Write(data.AcksPath, []byte(`{"schema": 1, "acks": [], "holds": []}`+"\n"))
	l.Write("rules/overlay/nested/deep.json", []byte("[]\n"))
	l.Write("rules.lock.json.bak", []byte("x"))
	l.Write("rulesx/y", []byte("x"))
	l.Write("testdata/corpus/x.json", []byte("x"))
	l.Write("go.mod", []byte("module x\n"))
	after := l.Commit("submodule")
	// The working tree then moves on; Write must read the commit only.
	l.Write(data.LockPath, []byte("changed"))
	l.Remove(data.AcksPath)

	want := map[string][]byte{}
	for _, e := range must(l.Work.LsTree(ctx, after, "")) {
		if e.Path == data.LockPath || e.Path == data.GitmodulesPath || strings.HasPrefix(e.Path, "rules/") {
			want[e.Path] = must(l.Work.Blob(ctx, e.Object))
		}
	}
	want[data.GitlinkFile] = []byte(sha + "\n")
	for _, p := range []string{data.AcksPath, "rules/overlay/nested/deep.json", data.GitmodulesPath, data.NoticePath, data.OverlayPath, data.ClassesPath} {
		if _, ok := want[p]; !ok {
			t.Fatalf("fixture lacks %s", p)
		}
	}

	out := filepath.Join(t.TempDir(), "a", "base")
	if err := basetree.Write(ctx, l.Work, after, out); err != nil {
		t.Fatal(err)
	}
	got := tree(t, out)
	if !maps.EqualFunc(got, want, bytes.Equal) {
		t.Errorf("Write wrote %v, want %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
	}
	if ps := must(basetree.Paths(ctx, l.Work, after)); !slices.Equal(ps, slices.Sorted(maps.Keys(want))) {
		t.Errorf("Paths = %v", ps)
	}
	if err := basetree.Write(ctx, l.Work, after, out); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("Write into an existing directory: %v", err)
	}

	// Before the submodule PR there is neither .gitmodules nor GITLINK.
	out2 := filepath.Join(t.TempDir(), "base")
	if err := basetree.Write(ctx, l.Work, before, out2); err != nil {
		t.Fatal(err)
	}
	got2 := tree(t, out2)
	for _, p := range []string{data.GitmodulesPath, data.GitlinkFile} {
		if _, ok := got2[p]; ok {
			t.Errorf("base before the submodule PR has %s", p)
		}
	}
	if !bytes.Equal(got2[data.LockPath], must(os.ReadFile(filepath.Join(gittest.Root(t), data.LockPath)))) {
		t.Error("base lock differs from the copied lock")
	}

	if err := basetree.Write(ctx, l.Work, "refs/heads/nope", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Error("Write of a bad rev succeeded")
	}
}

func TestWriteRefusesSpecialEntries(t *testing.T) {
	ctx := context.Background()
	g := repo.Exec{Env: gittest.Isolate(t)}
	u := gittest.NewUpstream(t, g)
	when := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	c := u.Commit("", map[string][]byte{"rules/a.json": []byte("[]"), data.LockPath: []byte("{}")}, when, "ok")
	r := u.Repo()
	if ps := must(basetree.Paths(ctx, r, c)); !slices.Equal(ps, []string{data.LockPath, "rules/a.json"}) {
		t.Errorf("Paths = %v", ps)
	}

	// A symlink under rules/ (mode 120000), made with plumbing.
	target := must(g.Run(ctx, u.Dir, []byte("../rules.lock.json"), "hash-object", "-w", "--stdin"))
	rules := must(g.Run(ctx, u.Dir, []byte("120000 blob "+strings.TrimSpace(string(target))+"\tevil\x00"), "mktree", "-z"))
	root := must(g.Run(ctx, u.Dir, []byte("040000 tree "+strings.TrimSpace(string(rules))+"\trules\x00"), "mktree", "-z"))
	commit := strings.TrimSpace(string(must(g.Run(ctx, u.Dir, nil, "commit-tree", "-m", "symlink", strings.TrimSpace(string(root))))))
	err := basetree.Write(ctx, r, commit, filepath.Join(t.TempDir(), "x"))
	if err == nil || !strings.Contains(err.Error(), "mode 120000") {
		t.Errorf("symlink under rules/: %v", err)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
