package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/upstream"
)

func init() { register("sync", runSync) }

// sync's messages. Those the root provenance gate also prints are spelled
// as it spells them (msgGitlinkMissing, msgNotOnMaster,
// msgHistoryRewritten), so the routine can match either.
const (
	msgNoOriginMain     = "no local origin/main; fetch it first (git fetch origin main)"                                                           //
	msgGitmodulesMain   = ".gitmodules differs from origin/main's"                                                                                 //
	msgGitmodulesMap    = ".gitmodules does not map %s to %s with branch = master: %v"                                                             // path, URL, problem
	msgGitlinkMissing   = "upstream does not serve the gitlink %s (gitlink-missing)"                                                               // G
	msgNotInClone       = "the upstream clone has no commit %s; run rulegen upstream for HEAD"                                                     // G
	msgNotOnMaster      = "gitlink %s is not an ancestor of upstream master"                                                                       // G
	msgHistoryRewritten = "upstream history rewritten: %s is no longer on master"                                                                  // B
	msgCloneStatus      = "upstream clone: STATUS %q"                                                                                              // line
	msgRefused          = "refusing to write the sync write set: %s"                                                                               // problem
	msgLockRewrite      = "lock commit %s is not an ancestor of %s (upstream history rewritten?); rewriting only the lock's commit and the NOTICE" // X, G
	msgLockUntrue       = "lock commit %s does not hold the locked blobs; locking %s"                                                              // X, G
	msgInSync           = "in sync: the sync write set is sync's output for upstream@%s"                                                           // G
)

// runSync is `rulegen sync -root R -upstream DIR [-check [-content -rev C]]`.
// Without -check it writes rules/brave/*, rules/brave/NOTICE and
// rules.lock.json for the gitlink of R's HEAD, from the clone DIR made by
// rulegen upstream. -check writes nothing and exits 0 iff sync would change
// nothing. -check -content -rev C verifies a routine commit (verify.go).
func runSync(ctx context.Context, e *Env, args []string) int {
	fs := newFlags(e, "sync")
	root := fs.String("root", "", "linkclean checkout")
	upDir := fs.String("upstream", "", "upstream clone made by rulegen upstream")
	check := fs.Bool("check", false, "write nothing; exit 0 iff sync would change nothing")
	content := fs.Bool("content", false, "with -check -rev: verify commit C as a routine commit (exit 0 routine, 1 not, 2 unverifiable)")
	rev := fs.String("rev", "", "with -check -content: the commit to verify")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if !requireFlags(fs, "root", "upstream") {
		return exitUsage
	}
	if *content && (!*check || *rev == "") || *rev != "" && !*content {
		fmt.Fprintln(e.Stderr, "rulegen sync: -content needs -check and -rev, and -rev needs -content")
		return exitUsage
	}
	dir, err := filepath.Abs(*upDir)
	if err != nil {
		fmt.Fprintf(e.Stderr, "rulegen sync: %v\n", err)
		return exitUsage
	}
	if *content {
		return verifyCommit(ctx, e, *root, dir, *rev)
	}
	return syncTree(ctx, e, *root, dir, *check)
}

// syncTree runs sync steps 1–5 (or -check) on the working tree at root.
func syncTree(ctx context.Context, e *Env, root, dir string, check bool) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(e.Stderr, "rulegen sync: "+format+"\n", a...)
		return exitFail
	}
	r := e.Repo(root)
	// Step 1: the gitlink of HEAD's tree, never a submodule worktree.
	g, ok, err := r.Gitlink(ctx, "HEAD", data.Submodule)
	if err != nil {
		return fail("%v", err)
	}
	if !ok {
		return fail(msgNoGitlink, data.Submodule, "HEAD")
	}
	clone, status, err := upstream.Open(e.Git, dir)
	if err != nil {
		return fail("%v", err)
	}
	// Step 2: the origin checks.
	problems, notes, err := syncOriginChecks(ctx, r, clone, status, g)
	if err != nil {
		return fail("%v", err)
	}
	for _, n := range notes {
		fmt.Fprintf(e.Stdout, "note: %s\n", n)
	}
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintf(e.Stderr, "rulegen sync: %s\n", p)
		}
		return exitFail
	}

	rt, err := os.OpenRoot(root)
	if err != nil {
		return fail("%v", err)
	}
	defer rt.Close()
	prev, _ := rt.ReadFile(data.LockPath)
	// Steps 3–4: read the four files at G and decide the locked commit.
	out, err := syncCompute(ctx, clone, g, syncLenientLock(prev))
	if err != nil {
		return fail("%v", err)
	}
	for _, n := range out.Notes {
		fmt.Fprintf(e.Stdout, "note: %s\n", n)
	}
	// Step 5, behind the write-set guard.
	d := syncInspect(rt, out.Files)
	if len(d.refuse) > 0 {
		for _, p := range d.refuse {
			fmt.Fprintf(e.Stderr, "rulegen sync: "+msgRefused+"\n", p)
		}
		return exitFail
	}
	if d.empty() {
		if check {
			fmt.Fprintf(e.Stdout, msgInSync+"\n", g)
		} else {
			fmt.Fprintf(e.Stdout, data.MsgNoChange+"\n", g, out.Lock.Upstream.Commit)
		}
		return exitOK
	}
	if check {
		for _, p := range d.write {
			fmt.Fprintf(e.Stdout, "differs: %s\n", p)
		}
		for _, p := range d.remove {
			fmt.Fprintf(e.Stdout, "not sync output: %s\n", p)
		}
		fmt.Fprintf(e.Stderr, "rulegen sync -check: the sync write set is not sync's output for upstream@%s\n", g)
		return exitFail
	}
	if err := syncApply(rt, out.Files, d); err != nil {
		return fail("%v", err)
	}
	for _, p := range d.write {
		fmt.Fprintf(e.Stdout, "wrote %s\n", p)
	}
	for _, p := range d.remove {
		fmt.Fprintf(e.Stdout, "removed %s\n", p)
	}
	fmt.Fprintf(e.Stdout, "synced: upstream@%s, lock commit %s (%s)\n", g, out.Lock.Upstream.Commit, out.Lock.Upstream.CommitDate)
	return exitOK
}

// syncOriginChecks are sync step 2, the checks rules-provenance makes:
// .gitmodules, the clone's STATUS, G on upstream master, and no backwards
// move from origin/main's gitlink. It reads the local origin/main and never
// fetches it. problems fail sync; notes are informational.
func syncOriginChecks(ctx context.Context, r repo.Repo, c upstream.Clone, status, g string) (problems, notes []string, err error) {
	bad := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
	main, ok, err := r.RevParse(ctx, "refs/remotes/origin/main^{commit}")
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		bad(msgNoOriginMain)
	}

	gm, ok, err := r.Show(ctx, "HEAD", data.GitmodulesPath)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		bad(msgGitmodulesMap, data.Submodule, data.CanonicalURL, "HEAD has no "+data.GitmodulesPath)
	} else if err := data.CheckMapping(gm); err != nil {
		bad(msgGitmodulesMap, data.Submodule, data.CanonicalURL, err)
	}
	if main != "" {
		// When origin/main has none (the PR that adds the submodule), only
		// the mapping is checked.
		mgm, mok, err := r.Show(ctx, main, data.GitmodulesPath)
		if err != nil {
			return nil, nil, err
		}
		if mok && (!ok || !bytes.Equal(gm, mgm)) {
			bad(msgGitmodulesMain)
		}
	}

	switch {
	case status == data.StatusOK:
	case status == data.StatusGitlinkMissing:
		bad(msgGitlinkMissing, g)
		return problems, notes, nil
	case strings.HasPrefix(status, data.StatusUnreachablePrefix):
		bad("%s (%s)", data.MsgUnreachable, strings.TrimPrefix(status, data.StatusUnreachablePrefix))
		return problems, notes, nil
	default:
		bad(msgCloneStatus, status)
		return problems, notes, nil
	}
	if ok, err := c.HasCommit(ctx, g); err != nil {
		return nil, nil, err
	} else if !ok {
		bad(msgNotInClone, g)
		return problems, notes, nil
	}
	if ok, err := c.OnMaster(ctx, g); err != nil {
		return nil, nil, err
	} else if !ok {
		bad(msgNotOnMaster, g)
	}
	if main == "" {
		return problems, notes, nil
	}
	b, ok, err := r.Gitlink(ctx, main, data.Submodule)
	if err != nil || !ok || b == g {
		return problems, notes, err
	}
	if fwd, err := c.Descends(ctx, b, g); err != nil {
		return nil, nil, err
	} else if !fwd {
		onMaster, err := c.OnMaster(ctx, b)
		if err != nil {
			return nil, nil, err
		}
		if onMaster {
			bad(data.MsgBackwards, b, g)
		} else {
			notes = append(notes, fmt.Sprintf(msgHistoryRewritten, b))
		}
	}
	return problems, notes, nil
}

// syncOut is sync's output for one gitlink: the bytes of every path in the
// write set, and the lock they carry.
type syncOut struct {
	Lock  data.Lock
	Files map[string][]byte // repository path → bytes
	Notes []string
}

// syncLenientLock decodes a lock without validation, or returns nil; sync
// needs only its commit and blob ids, and must still run when the lock is
// broken.
func syncLenientLock(b []byte) *data.Lock {
	var l data.Lock
	if b == nil || json.Unmarshal(b, &l) != nil {
		return nil
	}
	return &l
}

// syncCompute is sync steps 3–5 for gitlink g given the lock before the
// sync (nil if absent or unreadable): the four files from the one commit g,
// and the lock and NOTICE. The churn rule keeps the lock's commit X when
// g's blobs are the locked ones, X is an ancestor of g and X holds those
// blobs too; when X has left g's history (an upstream rewrite), or does not
// hold the locked blobs, the lock moves to g.
func syncCompute(ctx context.Context, c upstream.Clone, g string, prev *data.Lock) (*syncOut, error) {
	files, err := c.ReadFiles(ctx, g)
	if err != nil {
		return nil, err
	}
	out := &syncOut{Files: map[string][]byte{}}
	commit := g
	if prev != nil && prev.Upstream.Commit != g {
		x := prev.Upstream.Commit
		locked := map[string]string{}
		for _, f := range files {
			locked[f.Spec.Upstream] = f.BlobID
		}
		same := true
		for up, id := range locked {
			if prev.Upstream.Files[up].GitBlobSHA1 != id {
				same = false
			}
		}
		if same {
			anc, err := c.Descends(ctx, x, g)
			if err != nil {
				return nil, err
			}
			switch {
			case !anc:
				out.Notes = append(out.Notes, fmt.Sprintf(msgLockRewrite, x, g))
			default:
				xids, err := c.BlobIDs(ctx, x)
				if err != nil {
					return nil, err
				}
				if maps.Equal(xids, locked) {
					commit = x
				} else {
					out.Notes = append(out.Notes, fmt.Sprintf(msgLockUntrue, x, g))
				}
			}
		}
	}
	date, err := c.CommitDate(ctx, commit)
	if err != nil {
		return nil, err
	}
	out.Lock = data.Lock{Schema: 4, Upstream: data.LockUpstream{
		Repo: data.RepoPath, Submodule: data.Submodule, Branch: data.Branch,
		Commit: commit, CommitDate: date, Files: map[string]data.LockFile{},
	}}
	for _, f := range files {
		lf := data.LockFile{Vendored: f.Spec.Vendored, GitBlobSHA1: f.BlobID, SHA256: data.SHA256Hex(f.Bytes), Counts: data.CountsOf(f.Spec.Kind, f.Bytes)}
		if f.Spec.Kind == data.KindLicense {
			lf.SPDX = data.SPDXOf(f.Bytes)
		}
		out.Lock.Upstream.Files[f.Spec.Upstream] = lf
		out.Files[f.Spec.Vendored] = f.Bytes
	}
	out.Files[data.NoticePath] = data.Notice(&out.Lock)
	out.Files[data.LockPath] = out.Lock.Encode()
	return out, nil
}

// syncPaths is the order sync writes in: the vendored files, the NOTICE,
// then the lock.
func syncPaths() []string {
	var ps []string
	for _, s := range data.Files {
		ps = append(ps, s.Vendored)
	}
	return append(ps, data.NoticePath, data.LockPath)
}

const syncDir = "rules/brave"

// syncDiff is what sync would change in the working tree.
type syncDiff struct {
	mkdir  []string // missing directories, parents first
	write  []string // write-set paths whose bytes or mode differ
	remove []string // regular files in rules/brave that sync does not write
	refuse []string // reasons sync must not write at all
}

func (d *syncDiff) empty() bool { return len(d.mkdir)+len(d.write)+len(d.remove) == 0 }

// syncInspect compares the working tree with want, the write-set guard
// included: rules and rules/brave must be real directories and every
// target a regular file (or absent), never a symlink, so no write can land
// outside the write set (a symlink rules/brave → overlay would otherwise
// redirect every write). It reads only; nothing is written until the whole
// set has been checked.
func syncInspect(rt *os.Root, want map[string][]byte) syncDiff {
	var d syncDiff
	for _, s := range syncPaths() {
		if !data.InWriteSet(s) {
			d.refuse = append(d.refuse, s+" is outside the sync write set")
		}
	}
	for p := range want {
		if !slices.Contains(syncPaths(), p) {
			d.refuse = append(d.refuse, p+" is not a sync output")
		}
	}
	dirOK := true // rules/brave exists as a real directory
	for _, dir := range []string{"rules", syncDir} {
		if !dirOK {
			d.mkdir = append(d.mkdir, dir) // its parent is missing too
			continue
		}
		fi, err := rt.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			d.mkdir = append(d.mkdir, dir)
			dirOK = false
		case err != nil:
			d.refuse = append(d.refuse, err.Error())
			return d
		case fi.Mode()&fs.ModeSymlink != 0:
			d.refuse = append(d.refuse, dir+" is a symlink")
			return d
		case !fi.IsDir():
			d.refuse = append(d.refuse, dir+" is not a directory")
			return d
		}
	}
	if len(d.refuse) > 0 {
		return d
	}
	for _, p := range syncPaths() {
		if !dirOK && p != data.LockPath {
			d.write = append(d.write, p)
			continue
		}
		fi, err := rt.Lstat(filepath.FromSlash(p))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			d.write = append(d.write, p)
			continue
		case err != nil:
			d.refuse = append(d.refuse, err.Error())
			continue
		case !fi.Mode().IsRegular():
			d.refuse = append(d.refuse, fmt.Sprintf("%s is not a regular file (%s)", p, fi.Mode().Type()))
			continue
		}
		have, err := rt.ReadFile(filepath.FromSlash(p))
		if err != nil {
			d.refuse = append(d.refuse, err.Error())
			continue
		}
		if !bytes.Equal(have, want[p]) || fi.Mode().Perm()&0o111 != 0 {
			d.write = append(d.write, p)
		}
	}
	if !dirOK {
		return d
	}
	f, err := rt.Open(syncDir)
	if err != nil {
		d.refuse = append(d.refuse, err.Error())
		return d
	}
	ents, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		d.refuse = append(d.refuse, err.Error())
		return d
	}
	for _, ent := range ents {
		p := path.Join(syncDir, ent.Name())
		if _, ok := want[p]; ok {
			continue
		}
		if !ent.Type().IsRegular() {
			d.refuse = append(d.refuse, fmt.Sprintf("%s is not a regular file (%s)", p, ent.Type()))
			continue
		}
		d.remove = append(d.remove, p)
	}
	slices.Sort(d.remove)
	return d
}

// syncApply makes the changes syncInspect found, each file through a
// temporary file in the same directory and a rename.
func syncApply(rt *os.Root, want map[string][]byte, d syncDiff) error {
	for _, dir := range d.mkdir {
		if err := rt.Mkdir(dir, 0o755); err != nil {
			return err
		}
	}
	for _, p := range d.write {
		name := filepath.FromSlash(p)
		tmp := filepath.Join(filepath.Dir(name), ".rulegen-sync-"+rand.Text())
		f, err := rt.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		_, werr := io.Copy(f, bytes.NewReader(want[p]))
		if err := cmp.Or(werr, f.Chmod(0o644), f.Close()); err != nil {
			rt.Remove(tmp)
			return err
		}
		if err := rt.Rename(tmp, name); err != nil {
			rt.Remove(tmp)
			return err
		}
	}
	for _, p := range d.remove {
		if err := rt.Remove(filepath.FromSlash(p)); err != nil {
			return err
		}
	}
	return nil
}
