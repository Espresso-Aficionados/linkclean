package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/gittest"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/upstream"
)

var syncUpdate = flag.Bool("sync.update", false, "rewrite the want sections of testdata/sync/*.txtar")

// syncTxtar reads testdata/sync/<name>: its comment and its files.
func syncTxtar(t testing.TB, name string) (string, map[string][]byte) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "sync", name))
	if err != nil {
		t.Fatal(err)
	}
	return syncParseTxtar(b)
}

// syncParseTxtar parses the txtar format: a comment, then files, each
// introduced by a line "-- name --" and running to the next such line.
func syncParseTxtar(b []byte) (string, map[string][]byte) {
	files := map[string][]byte{}
	var comment strings.Builder
	cur := ""
	for line := range strings.SplitAfterSeq(string(b), "\n") {
		trimmed := strings.TrimRight(line, "\n")
		if name, ok := strings.CutPrefix(trimmed, "-- "); ok && strings.HasSuffix(name, " --") {
			cur = strings.TrimSuffix(name, " --")
			files[cur] = []byte{}
			continue
		}
		if cur == "" {
			comment.WriteString(line)
		} else {
			files[cur] = append(files[cur], line...)
		}
	}
	return comment.String(), files
}

func syncFormatTxtar(comment string, files map[string][]byte) []byte {
	var b bytes.Buffer
	b.WriteString(comment)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		fmt.Fprintf(&b, "-- %s --\n", name)
		b.Write(files[name])
	}
	return b.Bytes()
}

// syncCommitTimes are the committer dates of the fixture upstream commits.
var syncCommitTimes = map[string]time.Time{
	"x0": time.Date(2026, 10, 1, 9, 0, 0, 0, time.FixedZone("", -7*3600)),
	"x1": time.Date(2026, 10, 7, 9, 0, 0, 0, time.FixedZone("", -7*3600)),
	"x2": time.Date(2026, 10, 14, 19, 19, 33, 0, time.FixedZone("", -7*3600)),
	"x3": time.Date(2026, 10, 15, 8, 30, 0, 0, time.UTC),
	"pr": time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC),
	"r0": time.Date(2026, 10, 16, 6, 0, 0, 0, time.UTC),
	"r1": time.Date(2026, 10, 16, 7, 0, 0, 0, time.UTC),
}

// syncFix is one fixture: a fake upstream with the history of
// testdata/sync/upstream.txtar and a linkclean copy whose main is synced to
// x0, with the gitlink at x0 and the canonical .gitmodules.
type syncFix struct {
	t     *testing.T
	g     repo.Git
	up    *gittest.Upstream
	l     *gittest.Linkclean
	c     map[string]string            // commit name → id
	files map[string]map[string][]byte // commit name → its tree
}

// syncNewFix builds the fixture from the synthetic upstream files.
func syncNewFix(t *testing.T) *syncFix {
	t.Helper()
	_, tx := syncTxtar(t, "upstream.txtar")
	layers := map[string]map[string][]byte{}
	for name, b := range tx {
		commit, p, _ := strings.Cut(name, "/")
		if layers[commit] == nil {
			layers[commit] = map[string][]byte{}
		}
		layers[commit][p] = b
	}
	return syncBuild(t, layers, gittest.Opts{})
}

// syncBuild makes the upstream history from layers (x0's full tree, then
// the files each later commit changes) and the linkclean copy.
func syncBuild(t *testing.T, layers map[string]map[string][]byte, o gittest.Opts) *syncFix {
	t.Helper()
	f := &syncFix{t: t, g: repo.Exec{Env: gittest.Isolate(t)}, c: map[string]string{}, files: map[string]map[string][]byte{}}
	f.up = gittest.NewUpstream(t, f.g)
	for _, step := range []struct{ name, parent, from string }{
		{"x0", "", ""}, {"x1", "x0", "x0"}, {"x2", "x1", "x1"}, {"x3", "x2", "x2"}, {"pr", "x0", "x0"}, {"r0", "", "x0"},
	} {
		tree := maps.Clone(f.files[step.from])
		if tree == nil {
			tree = map[string][]byte{}
		}
		maps.Copy(tree, layers[step.name])
		f.files[step.name] = tree
		f.c[step.name] = f.up.Commit(f.c[step.parent], tree, syncCommitTimes[step.name], step.name)
	}
	f.up.SetRef("refs/heads/master", f.c["x3"])
	f.up.SetRef("refs/pull/1/head", f.c["pr"])

	f.l = gittest.NewLinkclean(t, f.g, o)
	f.l.SyncedTo(f.up, f.c["x0"])
	f.l.SetGitlink(f.c["x0"])
	f.l.WriteGitmodules(data.CanonicalURL, data.Branch)
	f.l.Commit("main: synced to x0")
	f.l.Push("main")
	return f
}

// branch starts branch name from main.
func (f *syncFix) branch(name string) { f.l.Run("checkout", "--quiet", "-B", name, "main") }

// bump is Dependabot's commit: the gitlink only.
func (f *syncFix) bump(name string) string {
	f.t.Helper()
	f.l.SetGitlink(f.c[name])
	return f.l.Commit("Bump tools/upstream/adblock-lists to " + name)
}

// mainTo moves origin/main to a synced commit at upstream commit name, as
// a merged takeover or another sync would: gitlink and data both move.
func (f *syncFix) mainTo(name string) string {
	f.t.Helper()
	f.branch("main")
	f.l.SyncedTo(f.up, f.c[name])
	f.l.SetGitlink(f.c[name])
	c := f.l.Commit("main: synced to " + name)
	f.l.Push("main")
	return c
}

// rewrite force-pushes upstream master to r0 (x0's bytes, a new root) and
// drops the PR ref; with prune, upstream also stops serving every commit
// master no longer reaches.
func (f *syncFix) rewrite(prune bool) {
	f.t.Helper()
	f.up.SetRef("refs/heads/master", f.c["r0"])
	f.up.Run("update-ref", "-d", "refs/pull/1/head")
	if prune {
		f.up.Prune()
	}
}

// upstream runs `rulegen upstream` with the fixture upstream as the pinned
// URL and returns the clone directory. url overrides it ("" for the
// fixture).
func (f *syncFix) upstream(url string) string {
	f.t.Helper()
	dir := filepath.Join(f.t.TempDir(), "upstream")
	code, out, errs := f.cloneWith(cmp.Or(url, f.up.URL), "-root", f.l.Work.Dir, "-out", dir)
	if code != exitOK {
		f.t.Fatalf("rulegen upstream: exit %d\n%s%s", code, out, errs)
	}
	return dir
}

// syncCloneMu serializes the tests' use of data.CloneURL, the pinned URL
// that only the upstream command reads.
var syncCloneMu sync.Mutex

// cloneWith runs `rulegen upstream args` with data.CloneURL set to url for
// the call only.
func (f *syncFix) cloneWith(url string, args ...string) (int, string, string) {
	f.t.Helper()
	syncCloneMu.Lock()
	defer syncCloneMu.Unlock()
	saved := data.CloneURL
	data.CloneURL = url
	defer func() { data.CloneURL = saved }()
	return f.rulegen(append([]string{"upstream"}, args...)...)
}

// rulegen runs one rulegen command.
func (f *syncFix) rulegen(args ...string) (code int, stdout, stderr string) {
	f.t.Helper()
	e, o, errs := newTestEnv(f.t, nil)
	e.Git = f.g
	code = run(context.Background(), e, args)
	return code, o.String(), errs.String()
}

func (f *syncFix) sync(dir string, args ...string) (int, string, string) {
	f.t.Helper()
	return f.rulegen(append([]string{"sync", "-root", f.l.Work.Dir, "-upstream", dir}, args...)...)
}

func (f *syncFix) verify(dir, c string) (int, string) {
	f.t.Helper()
	code, out, errs := f.sync(dir, "-check", "-content", "-rev", c)
	return code, out + errs
}

// syncCommit runs sync and commits its output as the routine does: the
// §11.1 subject and the Rulegen-Sync trailer.
func (f *syncFix) syncCommit(dir string) string {
	f.t.Helper()
	if code, out, errs := f.sync(dir); code != exitOK {
		f.t.Fatalf("sync: exit %d\n%s%s", code, out, errs)
	}
	g := f.gitlink("HEAD")
	date, err := f.up.Repo().CommitDate(context.Background(), g)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.l.Commit(fmt.Sprintf(data.SyncSubject, g[:data.ShortLen], date[:10]), data.TrailerSync+": "+g)
}

// routineMerge merges origin/main into the branch as the routine does
// (git merge --no-ff, trailer Rulegen-Merge: main).
func (f *syncFix) routineMerge() string {
	f.t.Helper()
	f.l.Run("fetch", "--quiet", "origin")
	f.l.Run("-c", "user.name="+gittest.Name, "-c", "user.email="+gittest.Email, "-c", "commit.gpgSign=false",
		"merge", "--quiet", "--no-ff", "--no-edit", "-m", "Merge origin/main", "-m", data.TrailerMerge+": "+data.MergeTrailerValue, "origin/main")
	return f.l.Run("rev-parse", "HEAD")
}

func (f *syncFix) gitlink(rev string) string {
	f.t.Helper()
	g, ok, err := f.l.Work.Gitlink(context.Background(), rev, data.Submodule)
	if err != nil || !ok {
		f.t.Fatalf("gitlink of %s: %v %v", rev, ok, err)
	}
	return g
}

// snapshot hashes every file of the working tree outside .git, with its
// mode, so a test can tell what a command wrote.
func (f *syncFix) snapshot() map[string]string {
	f.t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(f.l.Work.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(f.l.Work.Dir, p)
		if d.IsDir() && rel == ".git" {
			return filepath.SkipDir
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		v := fi.Mode().String()
		switch {
		case fi.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			v += " " + hex.EncodeToString(sum[:])
		case fi.Mode()&fs.ModeSymlink != 0:
			dst, _ := os.Readlink(p)
			v += " -> " + dst
		}
		m[filepath.ToSlash(rel)] = v
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return m
}

// changed lists the paths whose snapshot entries differ.
func syncChanged(before, after map[string]string) []string {
	var ps []string
	for p, v := range after {
		if before[p] != v {
			ps = append(ps, p)
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			ps = append(ps, p)
		}
	}
	slices.Sort(ps)
	return ps
}

func syncWant(t *testing.T, what string, code, want int, out string) {
	t.Helper()
	if code != want {
		t.Errorf("%s: exit %d, want %d\n%s", what, code, want, out)
	}
}

func syncContains(t *testing.T, what, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("%s: output lacks %q:\n%s", what, w, out)
		}
	}
}

// syncWritesNothing runs sync and checks it exits 1 without touching the
// working tree, and that its output names wants.
func (f *syncFix) syncWritesNothing(t *testing.T, dir string, wants ...string) {
	t.Helper()
	before := f.snapshot()
	code, out, errs := f.sync(dir)
	syncWant(t, "sync", code, exitFail, out+errs)
	syncContains(t, "sync", out+errs, wants...)
	if ch := syncChanged(before, f.snapshot()); len(ch) > 0 {
		t.Errorf("sync wrote %q", ch)
	}
}

// TestSyncFixtures holds the §11.8 rows for upstream, sync and the commit
// verifier (§11.6), one subtest per row, named as the row. The Provenance
// halves of these rows run the root gate in TestSyncE2E.
func TestSyncFixtures(t *testing.T) {
	writeSet := func(t *testing.T, ps []string) {
		t.Helper()
		for _, p := range ps {
			if !data.InWriteSet(p) {
				t.Errorf("wrote %s, outside the sync write set", p)
			}
		}
	}

	t.Run("sync_no_change", func(t *testing.T) {
		t.Parallel()
		f := syncNewFix(t)
		f.branch("dependabot/x1")
		f.bump("x1")
		dir := f.upstream("")
		before := f.snapshot()
		code, out, errs := f.sync(dir)
		syncWant(t, "sync", code, exitOK, out+errs)
		if want := fmt.Sprintf(data.MsgNoChange, f.c["x1"], f.c["x0"]); !strings.Contains(out, want) {
			t.Errorf("sync output %q, want %q", out, want)
		}
		if ch := syncChanged(before, f.snapshot()); len(ch) > 0 {
			t.Errorf("sync wrote %q", ch)
		}
		if l, err := data.ParseLock(f.l.Read(data.LockPath)); err != nil || l.Upstream.Commit != f.c["x0"] {
			t.Errorf("lock commit %q (%v), want x0 %s", l.Upstream.Commit, err, f.c["x0"])
		}
		code, out, errs = f.sync(dir, "-check")
		syncWant(t, "sync -check", code, exitOK, out+errs)
	})

	t.Run("sync_bump_ok", func(t *testing.T) {
		t.Parallel()
		f := syncNewFix(t)
		f.branch("dependabot/x2")
		f.bump("x2")
		dir := f.upstream("")
		code, out, errs := f.sync(dir, "-check")
		syncWant(t, "sync -check before sync", code, exitFail, out+errs)
		before := f.snapshot()
		code, out, errs = f.sync(dir)
		syncWant(t, "sync", code, exitOK, out+errs)
		ch := syncChanged(before, f.snapshot())
		writeSet(t, ch)
		if want := []string{"rules.lock.json", "rules/brave/NOTICE", "rules/brave/clean-urls.json"}; !slices.Equal(ch, want) {
			t.Errorf("sync wrote %q, want %q", ch, want)
		}
		// The lock and NOTICE, byte for byte (testdata/sync/sync_bump_ok.txtar).
		comment, want := syncTxtar(t, "sync_bump_ok.txtar")
		got := map[string][]byte{
			"want/" + data.LockPath:   f.l.Read(data.LockPath),
			"want/" + data.NoticePath: f.l.Read(data.NoticePath),
		}
		if *syncUpdate {
			if err := os.WriteFile(filepath.Join("testdata", "sync", "sync_bump_ok.txtar"), syncFormatTxtar(comment, got), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for p, b := range got {
			if !bytes.Equal(b, want[p]) {
				t.Errorf("%s:\n%s\nwant:\n%s", p, b, want[p])
			}
		}
		l, err := data.ParseLock(f.l.Read(data.LockPath))
		if err != nil {
			t.Fatal(err)
		}
		if l.Upstream.Commit != f.c["x2"] || l.Upstream.CommitDate != "2026-10-14T19:19:33-07:00" {
			t.Errorf("lock commit %s %s, want x2 %s and its committer date", l.Upstream.Commit, l.Upstream.CommitDate, f.c["x2"])
		}
		notice := string(f.l.Read(data.NoticePath))
		syncContains(t, "NOTICE", notice, f.c["x2"])
		for _, s := range data.Files {
			syncContains(t, "NOTICE", notice, l.Upstream.Files[s.Upstream].GitBlobSHA1)
			if b := f.l.Read(s.Vendored); !bytes.Equal(b, f.files["x2"][s.Upstream]) {
				t.Errorf("%s is not upstream@x2's", s.Vendored)
			}
		}
		// Again: nothing to write, and -check passes.
		before = f.snapshot()
		code, out, errs = f.sync(dir)
		syncWant(t, "second sync", code, exitOK, out+errs)
		syncContains(t, "second sync", out, fmt.Sprintf(data.MsgNoChange, f.c["x2"], f.c["x2"]))
		if ch := syncChanged(before, f.snapshot()); len(ch) > 0 {
			t.Errorf("second sync wrote %q", ch)
		}
		code, out, errs = f.sync(dir, "-check")
		syncWant(t, "sync -check", code, exitOK, out+errs)
		// The routine's commit of that output verifies.
		c := f.syncCommit(dir)
		code, msg := f.verify(dir, c)
		syncWant(t, "verify", code, exitOK, msg)
	})

	t.Run("sync_check_detects_edit", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			edit func(f *syncFix)
			to   string // the gitlink; "" for x2
		}{
			{"lock_commit_moved", func(f *syncFix) {
				// x1 has x0's blobs: sync on the bump keeps the lock at x0
				// and writes nothing, so a commit moving the lock to x1 is
				// not its output. -check passes on the result (sync would
				// now change nothing, and x1 is a true source pointer);
				// the verifier, which judges C against C^1, does not.
				f.l.SyncedTo(f.up, f.c["x1"])
			}, "x1"},
			{"lock_counts", func(f *syncFix) {
				l, _ := data.ParseLock(f.l.Read(data.LockPath))
				lf := l.Upstream.Files["brave-lists/clean-urls.json"]
				lf.Counts.Params++
				l.Upstream.Files["brave-lists/clean-urls.json"] = lf
				f.l.Write(data.LockPath, l.Encode())
			}, ""},
			{"vendored_relocked", func(f *syncFix) {
				// A hand edit with the lock and NOTICE rewritten to match.
				b := append(f.l.Read("rules/brave/clean-urls.json"), '\n')
				f.l.Write("rules/brave/clean-urls.json", b)
				l, _ := data.ParseLock(f.l.Read(data.LockPath))
				lf := l.Upstream.Files["brave-lists/clean-urls.json"]
				lf.GitBlobSHA1, lf.SHA256 = repo.BlobID(b), data.SHA256Hex(b)
				l.Upstream.Files["brave-lists/clean-urls.json"] = lf
				f.l.Write(data.LockPath, l.Encode())
				f.l.Write(data.NoticePath, data.Notice(&l))
			}, ""},
			{"notice", func(f *syncFix) {
				f.l.Write(data.NoticePath, append(f.l.Read(data.NoticePath), "extra\n"...))
			}, ""},
			{"extra_file", func(f *syncFix) {
				f.l.Write("rules/brave/extra.json", []byte("[]\n"))
			}, ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				f := syncNewFix(t)
				to := cmp.Or(tc.to, "x2")
				f.branch("dependabot/" + to)
				f.bump(to)
				dir := f.upstream("")
				if code, out, errs := f.sync(dir); code != exitOK {
					t.Fatalf("sync: %d %s%s", code, out, errs)
				}
				tc.edit(f)
				g := f.gitlink("HEAD")
				c := f.l.Commit("rules: brave/adblock-lists@"+g[:12], data.TrailerSync+": "+g)
				code, out, errs := f.sync(dir, "-check")
				if tc.name == "lock_commit_moved" {
					syncWant(t, "sync -check", code, exitOK, out+errs)
				} else {
					syncWant(t, "sync -check", code, exitFail, out+errs)
				}
				code, msg := f.verify(dir, c)
				syncWant(t, "verify", code, exitFail, msg)
				syncContains(t, "verify", msg, "differs from sync's output")
			})
		}
	})

	t.Run("sync_commit_verifies_after_rewrite", func(t *testing.T) {
		t.Parallel()
		setup := func(t *testing.T) (*syncFix, string) {
			f := syncNewFix(t)
			f.branch("dependabot/x2")
			f.bump("x2")
			c := f.syncCommit(f.upstream(""))
			return f, c
		}
		t.Run("master_rewritten", func(t *testing.T) {
			t.Parallel()
			f, c := setup(t)
			f.rewrite(false)
			dir := f.upstream("") // fetches x2 by id: still served
			code, out, errs := f.sync(dir, "-check")
			syncWant(t, "sync -check", code, exitFail, out+errs)
			syncContains(t, "sync -check", out+errs, fmt.Sprintf(msgNotOnMaster, f.c["x2"]))
			code, msg := f.verify(dir, c)
			syncWant(t, "verify", code, exitOK, msg)
			syncContains(t, "verify", msg, "stale after an upstream rewrite")
		})
		t.Run("main_moved_past", func(t *testing.T) {
			t.Parallel()
			f, c := setup(t)
			f.mainTo("x3")
			f.l.Run("checkout", "--quiet", "dependabot/x2")
			f.l.Run("fetch", "--quiet", "origin")
			dir := f.upstream("")
			code, out, errs := f.sync(dir, "-check")
			syncWant(t, "sync -check", code, exitFail, out+errs)
			syncContains(t, "sync -check", out+errs, fmt.Sprintf(data.MsgBackwards, f.c["x3"], f.c["x2"]))
			code, msg := f.verify(dir, c)
			syncWant(t, "verify", code, exitOK, msg)
		})
		t.Run("gitlink_missing", func(t *testing.T) {
			t.Parallel()
			f, c := setup(t)
			f.rewrite(true)
			// A clone made from main's gitlink, as the routine's is.
			f.l.Run("checkout", "--quiet", "main")
			dir := f.upstream("")
			code, msg := f.verify(dir, c)
			syncWant(t, "verify", code, exitUnverifiable, msg)
			syncContains(t, "verify", msg, data.MsgUnverifiableGone)
		})
		t.Run("unreachable", func(t *testing.T) {
			t.Parallel()
			f, c := setup(t)
			dir := f.upstream("file://" + filepath.ToSlash(filepath.Join(t.TempDir(), "down.git")))
			code, msg := f.verify(dir, c)
			syncWant(t, "verify (clone failed)", code, exitUnverifiable, msg)
			syncContains(t, "verify (clone failed)", msg, data.MsgUnverifiableDown)
			// A good clone, then upstream goes away.
			dir = f.upstream("")
			if err := os.Rename(f.up.Dir, f.up.Dir+".gone"); err != nil {
				t.Fatal(err)
			}
			code, msg = f.verify(dir, c)
			syncWant(t, "verify (fetch failed)", code, exitUnverifiable, msg)
			syncContains(t, "verify (fetch failed)", msg, data.MsgUnverifiableDown)
		})
		t.Run("human_commit_too", func(t *testing.T) {
			t.Parallel()
			f, c := setup(t)
			f.l.Write(data.ClassesPath, append(f.l.Read(data.ClassesPath), ' '))
			h := f.l.Commit("classify a pattern")
			f.rewrite(false)
			dir := f.upstream("")
			code, msg := f.verify(dir, c)
			syncWant(t, "verify sync commit", code, exitOK, msg)
			code, msg = f.verify(dir, h)
			syncWant(t, "verify human commit", code, exitFail, msg)
			syncContains(t, "verify human commit", msg, msgNoTrailer)
		})
	})

	t.Run("sync_trailer_with_acks_edit", func(t *testing.T) {
		t.Parallel()
		f := syncNewFix(t)
		f.branch("dependabot/x2")
		f.bump("x2")
		dir := f.upstream("")
		if code, out, errs := f.sync(dir); code != exitOK {
			t.Fatalf("sync: %d %s%s", code, out, errs)
		}
		f.l.Write(data.AcksPath, []byte(`{"schema": 1, "acks": [], "holds": [{"file": "brave-lists/clean-urls.json", "sha256": "`+strings.Repeat("a", 64)+`", "expires": "2026-12-01", "reason": "r", "by": "x"}]}`+"\n"))
		g := f.gitlink("HEAD")
		c := f.l.Commit("rules: brave/adblock-lists@"+g[:12], data.TrailerSync+": "+g)
		code, msg := f.verify(dir, c)
		syncWant(t, "verify", code, exitFail, msg)
		syncContains(t, "verify", msg, "outside the sync write set", data.AcksPath)
		// The sync output alone, with the same trailer, verifies.
		f.l.Run("reset", "--quiet", "--hard", "HEAD^")
		if code, out, errs := f.sync(dir); code != exitOK {
			t.Fatalf("sync: %d %s%s", code, out, errs)
		}
		c = f.l.Commit("rules: brave/adblock-lists@"+g[:12], data.TrailerSync+": "+g)
		code, msg = f.verify(dir, c)
		syncWant(t, "verify clean", code, exitOK, msg)
	})

	t.Run("merge_commit_after_main_moved", func(t *testing.T) {
		t.Parallel()
		f := syncNewFix(t)
		f.branch("dependabot/x2")
		f.bump("x2")
		dir := f.upstream("")
		f.syncCommit(dir)
		overlay := f.l.Read(data.OverlayPath)
		// main@A edits the overlay; the routine merges it into the PR.
		f.branch("main")
		f.l.Write(data.OverlayPath, append(bytes.Clone(overlay), "\n"...))
		a := f.l.Commit("overlay: week 1")
		f.l.Push("main")
		f.l.Run("checkout", "--quiet", "dependabot/x2")
		m := f.routineMerge()
		code, msg := f.verify(dir, m)
		syncWant(t, "verify week-1 merge", code, exitOK, msg)
		// main@B edits the same file again: the merge still verifies.
		f.branch("main")
		f.l.Write(data.OverlayPath, append(bytes.Clone(overlay), "\n\n"...))
		f.l.Commit("overlay: week 2")
		f.l.Push("main")
		f.l.Run("checkout", "--quiet", "dependabot/x2")
		f.l.Run("fetch", "--quiet", "origin")
		code, msg = f.verify(dir, m)
		syncWant(t, "verify after main moved", code, exitOK, msg)

		// The same merge with one hand edit in its tree.
		f.l.Run("reset", "--quiet", "--hard", m+"^1")
		f.l.Run("-c", "user.name=x", "-c", "user.email=x@example.invalid", "merge", "--quiet", "--no-ff", "--no-commit", a)
		f.l.Write(data.ClassesPath, append(f.l.Read(data.ClassesPath), ' '))
		h := f.l.Commit("Merge origin/main", data.TrailerMerge+": main")
		if ps := strings.Fields(f.l.Run("rev-list", "--parents", "-1", h)); len(ps) != 3 {
			t.Fatalf("hand-edited merge has parents %q", ps)
		}
		code, msg = f.verify(dir, h)
		syncWant(t, "verify hand-edited merge", code, exitFail, msg)
		syncContains(t, "verify hand-edited merge", msg, "is not the merge of its parents")

		// A clean merge whose second parent is not on origin/main.
		f.l.Run("checkout", "--quiet", "-b", "side", a)
		f.l.Write("rules/side.txt", []byte("side\n"))
		side := f.l.Commit("side")
		f.l.Run("checkout", "--quiet", "dependabot/x2")
		f.l.Run("reset", "--quiet", "--hard", m+"^1")
		f.l.Run("-c", "user.name=x", "-c", "user.email=x@example.invalid", "merge", "--quiet", "--no-ff", "--no-edit",
			"-m", "Merge origin/main", "-m", data.TrailerMerge+": main", side)
		code, msg = f.verify(dir, f.l.Run("rev-parse", "HEAD"))
		syncWant(t, "verify merge of a side branch", code, exitFail, msg)
		syncContains(t, "verify merge of a side branch", msg, "is not on origin/main")

		// The routine's merge with a wrong trailer value, or a non-merge
		// commit carrying the merge trailer.
		f.l.Run("reset", "--quiet", "--hard", m+"^1")
		f.l.Write(data.ClassesPath, append(f.l.Read(data.ClassesPath), ' '))
		code, msg = f.verify(dir, f.l.Commit("edit", data.TrailerMerge+": main"))
		syncWant(t, "verify non-merge", code, exitFail, msg)
		syncContains(t, "verify non-merge", msg, fmt.Sprintf(msgParents, 1, 2))
	})

	t.Run("sync_write_set_refusal", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			edit func(t *testing.T, f *syncFix)
			want string
		}{
			{"brave_symlink_to_overlay", func(t *testing.T, f *syncFix) {
				f.l.Remove("rules/brave")
				if err := os.Symlink("overlay", filepath.Join(f.l.Work.Dir, "rules", "brave")); err != nil {
					t.Fatal(err)
				}
			}, "rules/brave is a symlink"},
			{"rules_symlink", func(t *testing.T, f *syncFix) {
				if err := os.Rename(filepath.Join(f.l.Work.Dir, "rules"), filepath.Join(f.l.Work.Dir, "elsewhere")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("elsewhere", filepath.Join(f.l.Work.Dir, "rules")); err != nil {
					t.Fatal(err)
				}
			}, "rules is a symlink"},
			{"target_symlink", func(t *testing.T, f *syncFix) {
				f.l.Remove("rules/brave/clean-urls.json")
				if err := os.Symlink("../overlay/overlay.json", filepath.Join(f.l.Work.Dir, "rules", "brave", "clean-urls.json")); err != nil {
					t.Fatal(err)
				}
			}, "rules/brave/clean-urls.json is not a regular file"},
			{"lock_symlink", func(t *testing.T, f *syncFix) {
				f.l.Remove(data.LockPath)
				if err := os.Symlink(data.OverlayPath, filepath.Join(f.l.Work.Dir, data.LockPath)); err != nil {
					t.Fatal(err)
				}
			}, "rules.lock.json is not a regular file"},
			{"extra_symlink", func(t *testing.T, f *syncFix) {
				if err := os.Symlink("../overlay", filepath.Join(f.l.Work.Dir, "rules", "brave", "more")); err != nil {
					t.Fatal(err)
				}
			}, "rules/brave/more is not a regular file"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				f := syncNewFix(t)
				f.branch("dependabot/x2")
				f.bump("x2")
				dir := f.upstream("")
				tc.edit(t, f)
				f.syncWritesNothing(t, dir, tc.want)
				code, out, errs := f.sync(dir, "-check")
				syncWant(t, "sync -check", code, exitFail, out+errs)
			})
		}
		// An extra regular file is not refused: sync owns rules/brave and
		// removes it (go:embed would otherwise ship a stray *.json).
		f := syncNewFix(t)
		f.l.Write("rules/brave/stray.json", []byte("[]\n"))
		dir := f.upstream("")
		before := f.snapshot()
		code, out, errs := f.sync(dir)
		syncWant(t, "sync with a stray file", code, exitOK, out+errs)
		if ch := syncChanged(before, f.snapshot()); !slices.Equal(ch, []string{"rules/brave/stray.json"}) {
			t.Errorf("sync changed %q, want only the stray file removed", ch)
		}
	})

	t.Run("sync_gitlink_not_ancestor", func(t *testing.T) {
		t.Parallel()
		f := syncNewFix(t)
		f.branch("dependabot/pr")
		f.bump("pr")
		dir := f.upstream("")
		if st := syncStatus(t, dir); st != data.StatusOK {
			t.Fatalf("STATUS %q: the PR head is served by id", st)
		}
		f.syncWritesNothing(t, dir, fmt.Sprintf(msgNotOnMaster, f.c["pr"]))
	})

	t.Run("sync_upstream_history_rewritten", func(t *testing.T) {
		t.Parallel()
		f := syncNewFix(t)
		f.branch("dependabot/x2")
		f.bump("x2")
		f.rewrite(false)
		dir := f.upstream("")
		f.syncWritesNothing(t, dir, fmt.Sprintf(msgNotOnMaster, f.c["x2"]))
		code, out, errs := f.sync(dir, "-check")
		syncWant(t, "sync -check", code, exitFail, out+errs)
	})

	t.Run("sync_after_upstream_rewrite", func(t *testing.T) {
		t.Parallel()
		f := syncNewFix(t)
		f.rewrite(true) // x0 (main's gitlink and the lock's commit) leaves master
		f.branch("rewrite")
		f.bump("r0")
		dir := f.upstream("")
		before := f.snapshot()
		code, out, errs := f.sync(dir)
		syncWant(t, "sync", code, exitOK, out+errs)
		syncContains(t, "sync", out, fmt.Sprintf(msgHistoryRewritten, f.c["x0"]), fmt.Sprintf(msgLockRewrite, f.c["x0"], f.c["r0"]))
		if ch := syncChanged(before, f.snapshot()); !slices.Equal(ch, []string{"rules.lock.json", "rules/brave/NOTICE"}) {
			t.Errorf("sync changed %q, want only the lock and the NOTICE", ch)
		}
		l, err := data.ParseLock(f.l.Read(data.LockPath))
		if err != nil || l.Upstream.Commit != f.c["r0"] || l.Upstream.CommitDate != "2026-10-16T06:00:00Z" {
			t.Errorf("lock commit %s %s (%v), want r0 %s", l.Upstream.Commit, l.Upstream.CommitDate, err, f.c["r0"])
		}
		code, out, errs = f.sync(dir, "-check")
		syncWant(t, "sync -check", code, exitOK, out+errs)
	})

	t.Run("gitlink_moved_backwards", func(t *testing.T) {
		t.Parallel()
		f := syncNewFix(t)
		f.mainTo("x2")
		f.branch("back")
		f.l.SyncedTo(f.up, f.c["x1"])
		f.l.SetGitlink(f.c["x1"])
		f.l.Commit("move back to x1")
		dir := f.upstream("")
		f.syncWritesNothing(t, dir, fmt.Sprintf(data.MsgBackwards, f.c["x2"], f.c["x1"]))
	})

	t.Run("sync_fetch_fails", func(t *testing.T) {
		t.Parallel()
		f := syncNewFix(t)
		f.branch("dependabot/x2")
		f.bump("x2")
		dir := filepath.Join(t.TempDir(), "upstream")
		code, out, errs := f.cloneWith("file://"+filepath.ToSlash(filepath.Join(t.TempDir(), "down.git")), "-root", f.l.Work.Dir, "-out", dir)
		syncWant(t, "upstream", code, exitOK, out+errs)
		if st := syncStatus(t, dir); !strings.HasPrefix(st, data.StatusUnreachablePrefix) {
			t.Errorf("STATUS %q, want unreachable", st)
		}
		f.syncWritesNothing(t, dir, data.MsgUnreachable)
		// Upstream answers but no longer serves the gitlink.
		f.rewrite(true)
		dir = f.upstream("")
		if st := syncStatus(t, dir); st != data.StatusGitlinkMissing {
			t.Errorf("STATUS %q, want %s", st, data.StatusGitlinkMissing)
		}
		f.syncWritesNothing(t, dir, fmt.Sprintf(msgGitlinkMissing, f.c["x2"]))
	})

	t.Run("upstream_clone_shape", func(t *testing.T) {
		t.Parallel()
		f := syncNewFix(t)
		f.branch("dependabot/x2")
		f.bump("x2")
		// .gitmodules names another URL; the clone ignores it.
		fork := filepath.Join(t.TempDir(), "fork.git")
		f.l.WriteGitmodules("file://"+filepath.ToSlash(fork), data.Branch)
		f.l.Commit("point the submodule at a fork")
		dir := f.upstream("")
		syncCheckClone(t, f, dir, fork)
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		if !slices.Equal(names, []string{".git", data.StatusFile}) {
			t.Errorf("clone directory holds %q, want only .git and STATUS (no worktree)", names)
		}
		cl := repo.Repo{Git: f.g, Dir: dir}
		if bare, _ := cl.Out(context.Background(), "rev-parse", "--is-bare-repository"); bare != "false" {
			t.Errorf("clone is bare: %q", bare)
		}
		// sync reads the four blobs on demand.
		f.l.WriteGitmodules(data.CanonicalURL, data.Branch)
		f.l.Commit("restore .gitmodules")
		if code, out, errs := f.sync(dir); code != exitOK {
			t.Fatalf("sync: %d %s%s", code, out, errs)
		}
		if n := len(syncObjectTypes(t, f, dir)["blob"]); n != len(data.Files) {
			t.Errorf("after sync the clone holds %d blobs, want %d", n, len(data.Files))
		}
		// An existing non-empty -out is refused.
		code, out, errs := f.cloneWith(f.up.URL, "-root", f.l.Work.Dir, "-out", dir)
		syncWant(t, "upstream into a non-empty directory", code, exitFail, out+errs)
	})

	t.Run("gitmodules_changed", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name   string
			url    func(fork string) string
			branch string
			want   string
		}{
			{"fork", func(fork string) string { return "file://" + filepath.ToSlash(fork) }, data.Branch, "url = "},
			{"branch_main", func(string) string { return data.CanonicalURL }, "main", `branch = "main"`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				f := syncNewFix(t)
				f.branch("dependabot/x2")
				f.bump("x2")
				fork := filepath.Join(t.TempDir(), "fork.git")
				f.l.WriteGitmodules(tc.url(fork), tc.branch)
				f.l.Commit("edit .gitmodules")
				dir := f.upstream("")
				syncCheckClone(t, f, dir, fork)
				f.syncWritesNothing(t, dir, msgGitmodulesMain, tc.want)
			})
		}
		// The mapping is still checked when origin/main has no .gitmodules
		// (the PR that adds the submodule), and passes when it is right.
		f := syncNewFix(t)
		f.branch("main")
		f.l.Remove(data.GitmodulesPath)
		f.l.Commit("no .gitmodules on main")
		f.l.Push("main")
		f.branch("add-submodule")
		f.l.WriteGitmodules(data.CanonicalURL+".git", data.Branch)
		f.l.Commit("add the submodule")
		dir := f.upstream("")
		code, out, errs := f.sync(dir, "-check")
		syncWant(t, "sync -check, .gitmodules added", code, exitOK, out+errs)
		f.l.WriteGitmodules(data.CanonicalURL, "main")
		f.l.Commit("branch main")
		f.syncWritesNothing(t, dir, `branch = "main"`)
	})

	// The verifier parts of the routine rows (§11.9 step 2).
	t.Run("two_open_prs_older_human_edited", func(t *testing.T) {
		t.Parallel()
		f := syncNewFix(t)
		f.branch("dependabot/x2")
		f.bump("x2")
		dir := f.upstream("")
		s := f.syncCommit(dir)
		f.l.Write(data.ClassesPath, append(f.l.Read(data.ClassesPath), ' '))
		h := f.l.Commit("classify a debounce pattern")
		// The newer PR: Dependabot's commit only.
		f.branch("dependabot/x3")
		f.bump("x3")
		dir = f.upstream("")
		code, msg := f.verify(dir, s)
		syncWant(t, "older PR, sync commit", code, exitOK, msg)
		code, msg = f.verify(dir, h)
		syncWant(t, "older PR, human commit", code, exitFail, msg)
		// A human commit with a forged trailer is still a human edit.
		f.l.Run("checkout", "--quiet", "dependabot/x2")
		f.l.Write(data.ClassesPath, append(f.l.Read(data.ClassesPath), ' '))
		forged := f.l.Commit("classify another", data.TrailerSync+": "+f.c["x2"])
		code, msg = f.verify(dir, forged)
		syncWant(t, "forged trailer", code, exitFail, msg)
		syncContains(t, "forged trailer", msg, data.ClassesPath)
		// With only routine commits on the older PR, every non-Dependabot
		// commit verifies (the routine closes it "superseded").
		f.l.Run("reset", "--quiet", "--hard", s)
		commits := strings.Fields(f.l.Run("rev-list", "--reverse", "main..HEAD"))
		if len(commits) != 2 {
			t.Fatalf("older PR holds %q, want the bump and the sync commit", commits)
		}
		code, msg = f.verify(dir, commits[1])
		syncWant(t, "only routine commits", code, exitOK, msg)
	})

	t.Run("human_edited_handoff", func(t *testing.T) {
		t.Parallel()
		f := syncNewFix(t)
		f.branch("dependabot/x2")
		f.bump("x2")
		dir := f.upstream("")
		s := f.syncCommit(dir)
		f.l.Write(data.ClassesPath, append(f.l.Read(data.ClassesPath), ' '))
		h := f.l.Commit("classify a debounce pattern")
		// The routine's clone is made from main's gitlink.
		f.l.Run("checkout", "--quiet", "main")
		dir = f.upstream("")
		got := map[string]int{}
		for _, c := range []string{s, h} {
			got[c], _ = f.verify(dir, c)
		}
		if got[s] != exitOK || got[h] != exitFail {
			t.Errorf("classes: sync commit %d, human commit %d; want 0 and 1", got[s], got[h])
		}
		// One week later, after upstream moved on, the same classes.
		next := maps.Clone(f.files["x3"])
		next["brave-lists/other.txt"] = []byte("a week later\n")
		later := f.up.Commit(f.c["x3"], next, syncCommitTimes["x3"].Add(7*24*time.Hour), "a week later")
		f.up.SetRef("refs/heads/master", later)
		dir = f.upstream("")
		for _, c := range []string{s, h} {
			got[c], _ = f.verify(dir, c)
		}
		if got[s] != exitOK || got[h] != exitFail {
			t.Errorf("a week later: sync commit %d, human commit %d; want 0 and 1", got[s], got[h])
		}
	})

	t.Run("rewrite_with_open_pr_closes", func(t *testing.T) {
		t.Parallel()
		f := syncNewFix(t)
		f.branch("dependabot/x2")
		f.bump("x2")
		s := f.syncCommit(f.upstream(""))
		f.rewrite(false)
		dir := f.upstream("")
		code, out, errs := f.sync(dir)
		syncWant(t, "sync", code, exitFail, out+errs)
		syncContains(t, "sync", out+errs, fmt.Sprintf(msgNotOnMaster, f.c["x2"]))
		code, msg := f.verify(dir, s)
		syncWant(t, "verify sync commit", code, exitOK, msg) // not human-edited: closed
	})

	t.Run("takeover_merged_conflict_closes", func(t *testing.T) {
		t.Parallel()
		f := syncNewFix(t)
		f.branch("dependabot/x3")
		f.bump("x3")
		s := f.syncCommit(f.upstream(""))
		// A takeover merged since: main's gitlink at x2, an ancestor of
		// x3, and its lock changed.
		f.mainTo("x2")
		f.l.Run("checkout", "--quiet", "dependabot/x3")
		dir := f.upstream("")
		code, msg := f.verify(dir, s)
		syncWant(t, "verify sync commit", code, exitOK, msg)
		code, out, errs := f.sync(dir, "-check")
		syncWant(t, "sync -check (x2 is an ancestor of x3: no backwards move)", code, exitOK, out+errs)
		_, clean, err := f.l.Work.MergeTree(context.Background(), "HEAD", "refs/remotes/origin/main")
		if err != nil || clean {
			t.Errorf("freshness merge: clean %v, %v; want a conflict", clean, err)
		}
	})

	t.Run("takeover_merged_past_gitlink_closes", func(t *testing.T) {
		t.Parallel()
		f := syncNewFix(t)
		f.branch("dependabot/x2")
		f.bump("x2")
		s := f.syncCommit(f.upstream(""))
		f.mainTo("x3")
		f.l.Run("checkout", "--quiet", "dependabot/x2")
		dir := f.upstream("")
		f.syncWritesNothing(t, dir, fmt.Sprintf(data.MsgBackwards, f.c["x3"], f.c["x2"]))
		code, msg := f.verify(dir, s)
		syncWant(t, "verify sync commit", code, exitOK, msg)
	})
}

// syncCheckClone checks the clone made by `rulegen upstream`: origin is
// the pinned URL (the fixture's), STATUS is ok, it holds commits and trees
// only, and the fork .gitmodules names was never contacted.
func syncCheckClone(t *testing.T, f *syncFix, dir, fork string) {
	t.Helper()
	cl := repo.Repo{Git: f.g, Dir: dir}
	if url, err := cl.Out(context.Background(), "config", "remote.origin.url"); err != nil || url != f.up.URL {
		t.Errorf("remote.origin.url = %q (%v), want the pinned %q", url, err, f.up.URL)
	}
	if st := syncStatus(t, dir); st != data.StatusOK {
		t.Errorf("STATUS %q", st)
	}
	if got, err := cl.Out(context.Background(), "rev-parse", data.MasterRef); err != nil || got != f.c["x3"] {
		t.Errorf("%s = %q (%v), want x3", data.MasterRef, got, err)
	}
	types := syncObjectTypes(t, f, dir)
	for ty := range types {
		if ty != "commit" && ty != "tree" {
			t.Errorf("clone holds %d %s objects", len(types[ty]), ty)
		}
	}
	if _, err := os.Stat(fork); !os.IsNotExist(err) {
		t.Errorf("the fork was touched: %v", err)
	}
}

func syncStatus(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, data.StatusFile))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(b, []byte("\n")) != 1 || !bytes.HasSuffix(b, []byte("\n")) {
		t.Errorf("STATUS is not one line: %q", b)
	}
	return strings.TrimSuffix(string(b), "\n")
}

// syncObjectTypes lists the clone's objects by type, without fetching.
func syncObjectTypes(t *testing.T, f *syncFix, dir string) map[string][]string {
	t.Helper()
	out, err := (repo.Repo{Git: f.g, Dir: dir}).Out(context.Background(), "cat-file", "--batch-all-objects", "--batch-check=%(objecttype) %(objectname)")
	if err != nil {
		t.Fatal(err)
	}
	m := map[string][]string{}
	for line := range strings.SplitSeq(out, "\n") {
		if ty, id, ok := strings.Cut(line, " "); ok {
			m[ty] = append(m[ty], id)
		}
	}
	return m
}

// TestVerifyTrailers covers the verifier's dispatch on trailers: only one
// Rulegen-Sync naming C's own full gitlink, or one Rulegen-Merge: main,
// can make a commit routine.
func TestVerifyTrailers(t *testing.T) {
	f := syncNewFix(t)
	f.branch("dependabot/x2")
	f.bump("x2")
	dir := f.upstream("")
	if code, out, errs := f.sync(dir); code != exitOK {
		t.Fatalf("sync: %d %s%s", code, out, errs)
	}
	f.l.Run("add", "--all")
	g := f.c["x2"]
	for _, tc := range []struct {
		name     string
		trailers []string
		code     int
		want     string
	}{
		{"ok", []string{data.TrailerSync + ": " + g}, exitOK, "verified"},
		{"none", nil, exitFail, msgNoTrailer},
		{"other_commit", []string{data.TrailerSync + ": " + f.c["x1"]}, exitFail, fmt.Sprintf(msgTrailerNotG, f.c["x1"], g)},
		{"short_id", []string{data.TrailerSync + ": " + g[:12]}, exitFail, "but the gitlink is"},
		{"twice", []string{data.TrailerSync + ": " + g, data.TrailerSync + ": " + g}, exitFail, fmt.Sprintf(msgTrailerTwice, 2, data.TrailerSync)},
		{"both", []string{data.TrailerSync + ": " + g, data.TrailerMerge + ": main"}, exitFail, msgBothTrailers},
		{"merge_value", []string{data.TrailerMerge + ": master"}, exitFail, fmt.Sprintf(msgMergeValue, "master", "main")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := f.l.Run("write-tree")
			msg := "rules: sync"
			if len(tc.trailers) > 0 {
				msg += "\n\n" + strings.Join(tc.trailers, "\n")
			}
			c := f.l.Run("commit-tree", tree, "-p", "HEAD", "-m", msg)
			code, out := f.verify(dir, c)
			syncWant(t, "verify", code, tc.code, out)
			syncContains(t, "verify", out, tc.want)
		})
	}
}

// TestUpstreamCommand covers the upstream command's edges.
func TestUpstreamCommand(t *testing.T) {
	f := syncNewFix(t)
	code, _, errs := f.rulegen("upstream", "-root", f.l.Work.Dir)
	syncWant(t, "no -out", code, exitUsage, errs)
	// No gitlink in HEAD: exit 1, nothing cloned.
	f.branch("no-gitlink")
	f.l.Run("rm", "--quiet", "--cached", data.Submodule)
	f.l.Commit("drop the gitlink")
	dir := filepath.Join(t.TempDir(), "up")
	code, out, errs := f.cloneWith(f.up.URL, "-root", f.l.Work.Dir, "-out", dir)
	syncWant(t, "no gitlink", code, exitFail, out+errs)
	syncContains(t, "no gitlink", errs, fmt.Sprintf(msgNoGitlink, data.Submodule, "HEAD"))
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("upstream without a gitlink created %s", dir)
	}
	code, out, errs = f.sync(dir)
	syncWant(t, "sync without a gitlink", code, exitFail, out+errs)
}

// TestSyncFlags covers sync's usage errors.
func TestSyncFlags(t *testing.T) {
	e, _, _ := newTestEnv(t, nil)
	for _, args := range [][]string{
		{"-root", "r"},
		{"-root", "r", "-upstream", "u", "-content"},
		{"-root", "r", "-upstream", "u", "-check", "-content"},
		{"-root", "r", "-upstream", "u", "-check", "-rev", "HEAD"},
		{"-root", "r", "-upstream", "u", "-content", "-rev", "HEAD"},
	} {
		if code := run(context.Background(), e, append([]string{"sync"}, args...)); code != exitUsage {
			t.Errorf("sync %q: exit %d, want %d", args, code, exitUsage)
		}
	}
}

// TestSyncCompute pins the churn rule's choice of the locked commit.
func TestSyncCompute(t *testing.T) {
	f := syncNewFix(t)
	ctx := context.Background()
	f.branch("dependabot/x3")
	f.bump("x3")
	dir := f.upstream("")
	clone, _, err := upstream.Open(f.g, dir)
	if err != nil {
		t.Fatal(err)
	}
	lockAt := func(name string) *data.Lock {
		out, err := syncCompute(ctx, clone, f.c[name], nil)
		if err != nil {
			t.Fatal(err)
		}
		return &out.Lock
	}
	for _, tc := range []struct {
		name     string
		g        string
		prev     *data.Lock
		want     string
		wantNote string
	}{
		{"no_lock", "x1", nil, "x1", ""},
		{"same_blobs_keeps_x", "x1", lockAt("x0"), "x0", ""},
		{"changed_blobs_moves", "x2", lockAt("x1"), "x2", ""},
		{"same_commit", "x2", lockAt("x2"), "x2", ""},
		{"lock_commit_not_ancestor", "x1", func() *data.Lock {
			l := lockAt("x0")
			l.Upstream.Commit = f.c["pr"]
			return l
		}(), "x1", "is not an ancestor"},
		{"lock_commit_untrue", "x2", func() *data.Lock {
			// The lock names x1 but carries x2's blobs.
			l := lockAt("x2")
			l.Upstream.Commit = f.c["x1"]
			return l
		}(), "x2", "does not hold the locked blobs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := syncCompute(ctx, clone, f.c[tc.g], tc.prev)
			if err != nil {
				t.Fatal(err)
			}
			if out.Lock.Upstream.Commit != f.c[tc.want] {
				t.Errorf("commit %s, want %s", out.Lock.Upstream.Commit, tc.want)
			}
			if notes := strings.Join(out.Notes, "\n"); tc.wantNote == "" && notes != "" || !strings.Contains(notes, tc.wantNote) {
				t.Errorf("notes %q, want %q", notes, tc.wantNote)
			}
			if _, err := data.ParseLock(out.Files[data.LockPath]); err != nil {
				t.Errorf("lock does not parse: %v", err)
			}
		})
	}
	// A vendored path missing at G is an error naming it.
	missing := maps.Clone(f.files["x0"])
	delete(missing, "brave-lists/debounce.json")
	m := f.up.Commit(f.c["x3"], missing, syncCommitTimes["x3"], "drop debounce")
	f.up.SetRef("refs/heads/master", m)
	if st := clone.FetchCommit(ctx, m); st != data.StatusOK {
		t.Fatalf("fetch: %s", st)
	}
	if _, err := syncCompute(ctx, clone, m, nil); err == nil || !strings.Contains(err.Error(), "brave-lists/debounce.json is missing") {
		t.Errorf("missing path: %v", err)
	}
}
