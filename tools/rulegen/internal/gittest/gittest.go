// Package gittest builds throwaway git repositories for rulegen's tests: a
// bare stand-in for upstream brave/adblock-lists and a copy of this
// repository with a local bare origin. It runs real git with every user and
// system setting shut out and only file:// transport allowed, so a test can
// never reach the network. It is imported only by tests.
//
// Helpers fail the test they were created with.
package gittest

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
)

// Fixed identity of every fixture commit.
const (
	Name  = "linkclean test"
	Email = "test@example.invalid"
)

// Root returns the linkclean repository root: the directory holding the
// root go.mod, found from this source file.
func Root(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("gittest: no caller information")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", ".."))
	if _, err := os.Stat(filepath.Join(root, data.LockPath)); err != nil {
		t.Fatalf("gittest: repository root not found: %v", err)
	}
	return root
}

// isolation is the environment that keeps git away from the user's and the
// system's configuration and from every transport but file.
func isolation(home string) []string {
	return []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_ALLOW_PROTOCOL=file",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=" + Name,
		"GIT_AUTHOR_EMAIL=" + Email,
		"GIT_COMMITTER_NAME=" + Name,
		"GIT_COMMITTER_EMAIL=" + Email,
	}
}

// Isolate returns environment entries for repo.Exec.Env that isolate git:
// a temporary HOME, no global or system config, only file:// transport and a
// fixed identity.
func Isolate(t testing.TB) []string {
	t.Helper()
	return isolation(t.TempDir())
}

// Process isolates the whole test process, for TestMain: it pins GOCACHE,
// GOMODCACHE and GOPATH to their current values (they default to paths
// under HOME), then sets HOME to a new temporary directory and the Isolate
// variables. cleanup removes the directory.
func Process() (cleanup func(), err error) {
	gp, err := goPaths()
	if err != nil {
		return nil, err
	}
	home, err := os.MkdirTemp("", "rulegen-home-")
	if err != nil {
		return nil, err
	}
	env := slices.Concat(gp, isolation(home))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if err := os.Setenv(k, v); err != nil {
			return nil, err
		}
	}
	return func() { os.RemoveAll(home) }, nil
}

// goPaths returns GOCACHE, GOMODCACHE and GOPATH as KEY=value entries, as
// the go command resolves them for the environment of the first call.
var goPaths = sync.OnceValues(func() ([]string, error) {
	keys := []string{"GOCACHE", "GOMODCACHE", "GOPATH"}
	out, err := exec.Command("go", append([]string{"env"}, keys...)...).Output()
	if err != nil {
		return nil, fmt.Errorf("gittest: go env: %w", err)
	}
	vals := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(vals) != len(keys) {
		return nil, fmt.Errorf("gittest: go env printed %q", out)
	}
	for i, k := range keys {
		vals[i] = k + "=" + vals[i]
	}
	return vals, nil
})

func run(t testing.TB, g repo.Git, dir string, stdin []byte, args ...string) string {
	t.Helper()
	out, err := g.Run(context.Background(), dir, stdin, args...)
	if err != nil {
		t.Fatalf("gittest: %v", err)
	}
	return strings.TrimRight(string(out), "\n")
}

// Upstream is a bare repository standing in for brave/adblock-lists, served
// over file:// with filters and any-SHA wants allowed, as GitHub serves it.
type Upstream struct {
	Dir, URL string
	t        testing.TB
	g        repo.Git
}

// NewUpstream creates an empty upstream with default branch master.
func NewUpstream(t testing.TB, g repo.Git) *Upstream {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "adblock-lists.git")
	run(t, g, "", nil, "init", "--quiet", "--bare", "--initial-branch="+data.Branch, dir)
	u := &Upstream{Dir: dir, URL: "file://" + filepath.ToSlash(dir), t: t, g: g}
	u.Run("config", "uploadpack.allowFilter", "true")
	u.Run("config", "uploadpack.allowAnySHA1InWant", "true")
	return u
}

// Repo returns the upstream as a repo.Repo.
func (u *Upstream) Repo() repo.Repo { return repo.Repo{Git: u.g, Dir: u.Dir} }

// Run runs git in the upstream and returns its output without trailing
// newlines.
func (u *Upstream) Run(args ...string) string {
	u.t.Helper()
	return run(u.t, u.g, u.Dir, nil, args...)
}

// Commit writes a commit whose tree is exactly files (slash-separated paths
// → content) with the given parent ("" for a root commit), author and
// committer date when, and message msg, through plumbing only. It moves no
// ref; see SetRef.
func (u *Upstream) Commit(parent string, files map[string][]byte, when time.Time, msg string) string {
	u.t.Helper()
	tree := u.tree(files, "")
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", tree)
	if parent != "" {
		fmt.Fprintf(&b, "parent %s\n", parent)
	}
	sig := fmt.Sprintf("%s <%s> %d %s", Name, Email, when.Unix(), when.Format("-0700"))
	fmt.Fprintf(&b, "author %s\ncommitter %s\n\n%s\n", sig, sig, strings.TrimRight(msg, "\n"))
	return run(u.t, u.g, u.Dir, []byte(b.String()), "hash-object", "-t", "commit", "-w", "--stdin")
}

// tree writes the tree of the files below dir and returns its id.
func (u *Upstream) tree(files map[string][]byte, dir string) string {
	u.t.Helper()
	blobs := map[string]string{}
	subs := map[string]bool{}
	for p, b := range files {
		rel, ok := strings.CutPrefix(p, dir)
		if !ok {
			continue
		}
		if name, _, nested := strings.Cut(rel, "/"); nested {
			subs[name] = true
		} else {
			blobs[name] = run(u.t, u.g, u.Dir, b, "hash-object", "-w", "--no-filters", "--stdin")
		}
	}
	var in bytes.Buffer
	for _, name := range slices.Sorted(maps.Keys(blobs)) {
		fmt.Fprintf(&in, "100644 blob %s\t%s\x00", blobs[name], name)
	}
	for _, name := range slices.Sorted(maps.Keys(subs)) {
		fmt.Fprintf(&in, "040000 tree %s\t%s\x00", u.tree(files, dir+name+"/"), name)
	}
	return run(u.t, u.g, u.Dir, in.Bytes(), "mktree", "-z")
}

// SetRef points ref (e.g. refs/heads/master, refs/pull/1/head) at sha,
// rewriting history when sha does not descend from the old value.
func (u *Upstream) SetRef(ref, sha string) {
	u.t.Helper()
	u.Run("update-ref", ref, sha)
}

// Prune deletes every object no ref reaches, so upstream stops serving
// commits a rewrite dropped.
func (u *Upstream) Prune() {
	u.t.Helper()
	u.Run("reflog", "expire", "--expire=now", "--all")
	u.Run("gc", "--quiet", "--prune=now")
}

// Clone makes a clone in the layout rulegen upstream produces: a blobless,
// --no-checkout, single-branch clone of master from u.URL, and a STATUS
// file holding status.
func (u *Upstream) Clone(t testing.TB, status string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "upstream")
	run(t, u.g, "", nil, "clone", "--quiet", "--filter=blob:none", "--no-checkout", "--single-branch", "--branch", data.Branch, u.URL, dir)
	if err := os.WriteFile(filepath.Join(dir, data.StatusFile), []byte(status+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// RealData returns the four vendored files of the working tree, keyed by
// upstream path.
func RealData(t testing.TB) map[string][]byte {
	t.Helper()
	root := Root(t)
	m := map[string][]byte{}
	for _, s := range data.Files {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(s.Vendored)))
		if err != nil {
			t.Fatal(err)
		}
		m[s.Upstream] = b
	}
	return m
}

// Linkclean is a copy of this repository: Work, a non-bare repository on
// branch main, and Origin, the path of its bare remote "origin".
type Linkclean struct {
	Origin string
	Work   repo.Repo
	t      testing.TB
	env    []string
}

// Opts configures NewLinkclean.
type Opts struct {
	// FullModule copies every tracked and untracked-but-not-ignored file
	// of the working tree except tools/** and .github/**, so the root
	// module builds and tests (GoTest). Otherwise only rules/** and
	// rules.lock.json are copied.
	FullModule bool
}

// NewLinkclean copies the working tree's files into a new repository,
// commits them as "initial" and pushes main to a new bare origin. The copy
// has no .gitmodules and no gitlink unless the working tree has one.
func NewLinkclean(t testing.TB, g repo.Git, o Opts) *Linkclean {
	t.Helper()
	root := Root(t)
	base := t.TempDir()
	l := &Linkclean{
		Origin: filepath.Join(base, "origin.git"),
		Work:   repo.Repo{Git: g, Dir: filepath.Join(base, "work")},
		t:      t,
		env:    Isolate(t),
	}
	run(t, g, "", nil, "init", "--quiet", "--bare", "--initial-branch=main", l.Origin)
	run(t, g, "", nil, "init", "--quiet", "--initial-branch=main", l.Work.Dir)
	ls := run(t, g, root, nil, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	for p := range strings.SplitSeq(ls, "\x00") {
		if p == "" || !copied(p, o.FullModule) {
			continue
		}
		fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil || !fi.Mode().IsRegular() {
			continue // deleted in the working tree, or a gitlink or symlink
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			t.Fatal(err)
		}
		l.Write(p, b)
	}
	l.Run("remote", "add", "origin", l.Origin)
	l.Commit("initial")
	l.Push("main")
	return l
}

func copied(p string, full bool) bool {
	if !full {
		return p == data.LockPath || strings.HasPrefix(p, "rules/")
	}
	return !strings.HasPrefix(p, "tools/") && !strings.HasPrefix(p, ".github/")
}

// Run runs git in Work and returns its output without trailing newlines.
func (l *Linkclean) Run(args ...string) string {
	l.t.Helper()
	return run(l.t, l.Work.Git, l.Work.Dir, nil, args...)
}

func (l *Linkclean) abs(p string) string {
	if path.IsAbs(p) || path.Clean(p) != p || strings.HasPrefix(p, "../") || p == ".." {
		l.t.Fatalf("gittest: %q is not a clean relative path", p)
	}
	return filepath.Join(l.Work.Dir, filepath.FromSlash(p))
}

// Write writes a file of the working tree (slash-separated path).
func (l *Linkclean) Write(p string, b []byte) {
	l.t.Helper()
	f := l.abs(p)
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		l.t.Fatal(err)
	}
	if err := os.WriteFile(f, b, 0o644); err != nil {
		l.t.Fatal(err)
	}
}

// Read reads a file of the working tree.
func (l *Linkclean) Read(p string) []byte {
	l.t.Helper()
	b, err := os.ReadFile(l.abs(p))
	if err != nil {
		l.t.Fatal(err)
	}
	return b
}

// Remove removes a file or directory tree of the working tree.
func (l *Linkclean) Remove(p string) {
	l.t.Helper()
	if err := os.RemoveAll(l.abs(p)); err != nil {
		l.t.Fatal(err)
	}
}

// SetGitlink stages the gitlink data.Submodule → sha, as Dependabot's bump
// commit does. The path is left an empty directory, as in a clone without
// submodule update, so Commit keeps the gitlink.
func (l *Linkclean) SetGitlink(sha string) {
	l.t.Helper()
	if err := os.MkdirAll(l.abs(data.Submodule), 0o755); err != nil {
		l.t.Fatal(err)
	}
	l.Run("update-index", "--add", "--cacheinfo", "160000,"+sha+","+data.Submodule)
}

// WriteGitmodules writes .gitmodules as `git submodule add -b branch` does,
// mapping data.Submodule to url; an empty branch omits the line.
func (l *Linkclean) WriteGitmodules(url, branch string) {
	l.t.Helper()
	s := fmt.Sprintf("[submodule %q]\n\tpath = %s\n\turl = %s\n", data.Submodule, data.Submodule, url)
	if branch != "" {
		s += "\tbranch = " + branch + "\n"
	}
	l.Write(data.GitmodulesPath, []byte(s))
}

// SyncedTo writes what rulegen sync writes for upstream commit g: the four
// vendored files at g, the lock (commit g, g's committer date) and the
// NOTICE. It does not move the gitlink.
func (l *Linkclean) SyncedTo(u *Upstream, g string) {
	l.t.Helper()
	ctx := context.Background()
	ur := u.Repo()
	date, err := ur.CommitDate(ctx, g)
	if err != nil {
		l.t.Fatal(err)
	}
	lock := data.Lock{Schema: 4, Upstream: data.LockUpstream{
		Repo: data.RepoPath, Submodule: data.Submodule, Branch: data.Branch,
		Commit: g, CommitDate: date, Files: map[string]data.LockFile{},
	}}
	for _, s := range data.Files {
		b, ok, err := ur.Show(ctx, g, s.Upstream)
		if err != nil || !ok {
			l.t.Fatalf("gittest: upstream %s:%s: ok=%v %v", g, s.Upstream, ok, err)
		}
		f := data.LockFile{Vendored: s.Vendored, GitBlobSHA1: repo.BlobID(b), SHA256: data.SHA256Hex(b), Counts: data.CountsOf(s.Kind, b)}
		if s.Kind == data.KindLicense {
			f.SPDX = data.SPDXOf(b)
		}
		lock.Upstream.Files[s.Upstream] = f
		l.Write(s.Vendored, b)
	}
	l.Write(data.LockPath, lock.Encode())
	l.Write(data.NoticePath, data.Notice(&lock))
}

// Commit stages every change of the working tree and commits it (empty
// commits allowed) with message msg and the given trailer lines ("Key:
// value"). It returns the new HEAD.
func (l *Linkclean) Commit(msg string, trailers ...string) string {
	l.t.Helper()
	l.Run("add", "--all")
	if len(trailers) > 0 {
		msg = strings.TrimRight(msg, "\n") + "\n\n" + strings.Join(trailers, "\n")
	}
	l.Run("-c", "user.name="+Name, "-c", "user.email="+Email, "-c", "commit.gpgSign=false",
		"commit", "--quiet", "--allow-empty", "--no-verify", "--cleanup=verbatim", "-m", msg)
	return l.Run("rev-parse", "HEAD")
}

// Push pushes refspec to origin, updating the remote-tracking refs.
func (l *Linkclean) Push(refspec string) {
	l.t.Helper()
	l.Run("push", "--quiet", "origin", refspec)
}

// GoTest runs `go test -count=1 -v -run <run> .` in Work, which needs
// FullModule. The module proxy is off (GOPROXY=off, GOFLAGS=-mod=mod), so
// the root module's dependencies must already be in the module cache; git
// is isolated as by Isolate. env adds variables. ok reports a zero exit; out
// is the combined output.
func (l *Linkclean) GoTest(t testing.TB, run string, env map[string]string) (out string, ok bool) {
	t.Helper()
	gp, err := goPaths()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "test", "-count=1", "-v", "-run", run, ".")
	cmd.Dir = l.Work.Dir
	cmd.Env = append(slices.Concat(os.Environ(), l.env, gp),
		"GOPROXY=off", "GOFLAGS=-mod=mod", "GOWORK=off", "GOTOOLCHAIN=local")
	for _, k := range slices.Sorted(maps.Keys(env)) {
		cmd.Env = append(cmd.Env, k+"="+env[k])
	}
	b, err := cmd.CombinedOutput()
	if _, isExit := err.(*exec.ExitError); err != nil && !isExit {
		t.Fatalf("gittest: go test: %v", err)
	}
	return string(b), err == nil
}
