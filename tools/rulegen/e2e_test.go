package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/basetree"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/gittest"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
)

// Messages only the root provenance gate prints (rules_provenance_test.go).
const (
	syncRootGitmodulesChanged = ".gitmodules differs from base's"
	syncRootLockNotAncestor   = "lock commit %s is neither the gitlink %s nor an ancestor of it" // X, G
)

// syncRealLayers is the upstream history of upstream.txtar over the real
// vendored files, so the root gate builds: x2 and x3 change clean-urls.json
// by trailing newlines.
func syncRealLayers(t *testing.T) map[string]map[string][]byte {
	t.Helper()
	x0 := gittest.RealData(t)
	x0["brave-lists/other.txt"] = []byte("another list, v0\n")
	clean := x0["brave-lists/clean-urls.json"]
	return map[string]map[string][]byte{
		"x0": x0,
		"x1": {"brave-lists/other.txt": []byte("another list, v1\n")},
		"x2": {"brave-lists/clean-urls.json": append(append([]byte(nil), clean...), '\n')},
		"x3": {"brave-lists/clean-urls.json": append(append([]byte(nil), clean...), "\n\n"...)},
		"pr": {"README": []byte("an unmerged pull request\n")},
		"r0": {"brave-lists/other.txt": []byte("another list, rewritten\n")},
	}
}

// provenance runs the root gate TestRules/Provenance on the copy, with
// LINKCLEAN_BASE_DIR written by basetree from origin/main (as basedir does
// for a pull request) and LINKCLEAN_UPSTREAM_DIR set to upDir.
func (f *syncFix) provenance(t *testing.T, upDir string) (string, bool) {
	t.Helper()
	base := filepath.Join(t.TempDir(), "base")
	if err := basetree.Write(context.Background(), f.l.Work, "refs/remotes/origin/main", base); err != nil {
		t.Fatal(err)
	}
	return f.l.GoTest(t, "^TestRules$/^Provenance$", map[string]string{
		"LINKCLEAN_BASE_DIR":     base,
		"LINKCLEAN_UPSTREAM_DIR": upDir,
	})
}

func syncWantGate(t *testing.T, out string, ok, pass bool, wants ...string) {
	t.Helper()
	verdict := "--- FAIL: TestRules/Provenance"
	if pass {
		verdict = "--- PASS: TestRules/Provenance"
	}
	if ok != pass || !strings.Contains(out, verdict) {
		t.Errorf("Provenance: ok=%v, want %s:\n%s", ok, verdict, out)
		return
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("Provenance output lacks %q:\n%s", w, out)
		}
	}
}

// TestSyncE2E is the Provenance half of the sync, gitlink, gitmodules and
// provenance rows of §11.8: the root gate, run by `go test` on a full copy
// of the module with the base directory and the upstream clone that CI
// passes it.
func TestSyncE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and tests the root module")
	}
	layers := syncRealLayers(t)
	newFix := func(t *testing.T) *syncFix {
		t.Helper()
		return syncBuild(t, layers, gittest.Opts{FullModule: true})
	}
	// synced moves the gitlink to name with its bytes, lock and NOTICE, as
	// a bump plus the routine's sync commit leave the branch.
	synced := func(f *syncFix, name string) {
		f.l.SyncedTo(f.up, f.c[name])
		f.bump(name)
	}

	t.Run("sync_no_change", func(t *testing.T) {
		t.Parallel()
		f := newFix(t)
		f.branch("dependabot/x1")
		f.bump("x1")
		dir := f.upstream("")
		code, out, errs := f.sync(dir)
		syncWant(t, "sync", code, exitOK, out+errs)
		syncContains(t, "sync", out, fmt.Sprintf(data.MsgNoChange, f.c["x1"], f.c["x0"]))
		out, ok := f.provenance(t, dir)
		syncWantGate(t, out, ok, true)
	})

	t.Run("sync_bump_ok", func(t *testing.T) {
		t.Parallel()
		f := newFix(t)
		f.branch("dependabot/x2")
		f.bump("x2")
		dir := f.upstream("")
		f.syncCommit(dir)
		out, ok := f.provenance(t, dir)
		syncWantGate(t, out, ok, true)
	})

	t.Run("provenance_unsynced", func(t *testing.T) {
		t.Parallel()
		f := newFix(t)
		f.branch("dependabot/x2")
		f.bump("x2")
		dir := f.upstream("")
		out, ok := f.provenance(t, dir)
		syncWantGate(t, out, ok, false, fmt.Sprintf(data.MsgUnsynced, f.c["x2"]))
		f.syncCommit(dir)
		out, ok = f.provenance(t, dir)
		syncWantGate(t, out, ok, true)
	})

	t.Run("sync_gitlink_not_ancestor", func(t *testing.T) {
		t.Parallel()
		f := newFix(t)
		f.branch("dependabot/pr")
		synced(f, "pr")
		dir := f.upstream("")
		out, ok := f.provenance(t, dir)
		syncWantGate(t, out, ok, false, fmt.Sprintf(msgNotOnMaster, f.c["pr"]))
	})

	t.Run("sync_upstream_history_rewritten", func(t *testing.T) {
		t.Parallel()
		f := newFix(t)
		f.branch("dependabot/x2")
		synced(f, "x2")
		f.rewrite(false)
		dir := f.upstream("")
		out, ok := f.provenance(t, dir)
		syncWantGate(t, out, ok, false, fmt.Sprintf(msgNotOnMaster, f.c["x2"]))
	})

	t.Run("sync_after_upstream_rewrite", func(t *testing.T) {
		t.Parallel()
		f := newFix(t)
		f.rewrite(true)
		f.branch("rewrite")
		f.bump("r0")
		dir := f.upstream("")
		f.syncCommit(dir)
		out, ok := f.provenance(t, dir)
		syncWantGate(t, out, ok, true, fmt.Sprintf(msgHistoryRewritten, f.c["x0"]))
	})

	t.Run("gitlink_moved_backwards", func(t *testing.T) {
		t.Parallel()
		f := newFix(t)
		f.mainTo("x2")
		f.branch("back")
		synced(f, "x1")
		dir := f.upstream("")
		out, ok := f.provenance(t, dir)
		syncWantGate(t, out, ok, false, fmt.Sprintf(data.MsgBackwards, f.c["x2"], f.c["x1"]))
	})

	t.Run("sync_fetch_fails", func(t *testing.T) {
		t.Parallel()
		f := newFix(t)
		f.branch("dependabot/x2")
		synced(f, "x2")
		dir := f.upstream("file://" + filepath.ToSlash(filepath.Join(t.TempDir(), "down.git")))
		out, ok := f.provenance(t, dir)
		syncWantGate(t, out, ok, false, data.MsgUnreachable)
	})

	t.Run("gitmodules_changed", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct{ name, url, branch string }{
			{"fork", "file:///nonexistent/fork/adblock-lists", data.Branch},
			{"branch_main", data.CanonicalURL, "main"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				f := newFix(t)
				f.branch("edit-gitmodules")
				f.l.WriteGitmodules(tc.url, tc.branch)
				f.l.Commit("edit .gitmodules")
				dir := f.upstream("")
				out, ok := f.provenance(t, dir)
				syncWantGate(t, out, ok, false, syncRootGitmodulesChanged)
			})
		}
	})

	t.Run("provenance_unrelated_pr_upstream_down", func(t *testing.T) {
		t.Parallel()
		feature := func(f *syncFix) {
			f.branch("feature")
			f.l.Write("README.md", append(f.l.Read("README.md"), "\nA feature.\n"...))
			f.l.Commit("a feature")
		}
		t.Run("unreachable", func(t *testing.T) {
			t.Parallel()
			f := newFix(t)
			feature(f)
			dir := f.upstream("file://" + filepath.ToSlash(filepath.Join(t.TempDir(), "down.git")))
			out, ok := f.provenance(t, dir)
			syncWantGate(t, out, ok, true, data.MsgInputsUnchanged)
		})
		t.Run("main_gitlink_off_rewritten_master", func(t *testing.T) {
			t.Parallel()
			f := newFix(t)
			f.rewrite(true)
			feature(f)
			dir := f.upstream("")
			if st := syncStatus(t, dir); st != data.StatusGitlinkMissing {
				t.Errorf("STATUS %q, want %s", st, data.StatusGitlinkMissing)
			}
			out, ok := f.provenance(t, dir)
			syncWantGate(t, out, ok, true, data.MsgInputsUnchanged)
		})
	})

	t.Run("provenance_hand_edit", func(t *testing.T) {
		t.Parallel()
		f := newFix(t)
		f.branch("hand-edit")
		// The vendored file edited and the lock and NOTICE rewritten to
		// match, so the local checks pass and only upstream can tell.
		b := append(f.l.Read("rules/brave/clean-urls.json"), ' ')
		f.l.Write("rules/brave/clean-urls.json", b)
		l, err := data.ParseLock(f.l.Read(data.LockPath))
		if err != nil {
			t.Fatal(err)
		}
		lf := l.Upstream.Files["brave-lists/clean-urls.json"]
		lf.GitBlobSHA1, lf.SHA256 = repo.BlobID(b), data.SHA256Hex(b)
		l.Upstream.Files["brave-lists/clean-urls.json"] = lf
		f.l.Write(data.LockPath, l.Encode())
		f.l.Write(data.NoticePath, data.Notice(&l))
		f.l.Commit("hand edit")
		dir := f.upstream("")
		out, ok := f.provenance(t, dir)
		syncWantGate(t, out, ok, false, fmt.Sprintf(data.MsgUnsynced, f.c["x0"]))
		code, sout, serrs := f.sync(dir, "-check")
		syncWant(t, "sync -check", code, exitFail, sout+serrs)
	})

	t.Run("provenance_lock_commit_not_ancestor", func(t *testing.T) {
		t.Parallel()
		f := newFix(t)
		f.branch("dependabot/x1")
		// The lock names the unmerged PR head, which holds the same blobs.
		f.l.SyncedTo(f.up, f.c["pr"])
		f.bump("x1")
		dir := f.upstream("")
		out, ok := f.provenance(t, dir)
		syncWantGate(t, out, ok, false, fmt.Sprintf(syncRootLockNotAncestor, f.c["pr"], f.c["x1"]))
		// sync puts the lock back on a true source pointer.
		f.syncCommit(dir)
		out, ok = f.provenance(t, dir)
		syncWantGate(t, out, ok, true)
	})

}
