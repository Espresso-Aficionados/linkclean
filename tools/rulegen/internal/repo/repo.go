// Package repo runs git for rulegen. Every command goes through the Git
// interface, so tests can substitute a fake or an isolated environment, and
// every read is by object (ls-tree, cat-file, rev-parse), never by checkout.
package repo

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Git runs one git command in dir with stdin (nil for none) and returns its
// standard output. A non-zero exit is an *ExitError; the output read before
// it is returned too.
type Git interface {
	Run(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error)
}

// ExitError is a git command that exited non-zero.
type ExitError struct {
	Args   []string
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = "exit status " + strconv.Itoa(e.Code)
	}
	return "git " + strings.Join(e.Args, " ") + ": " + msg
}

// ExitCode returns 0 for a nil err, the exit code when err is (or wraps)
// an *ExitError, and -1 otherwise.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := errors.AsType[*ExitError](err); ok {
		return ee.Code
	}
	return -1
}

// Exec runs the git binary from PATH. The command inherits the process
// environment plus Env, then GIT_TERMINAL_PROMPT=0 (never wait for
// credentials) and LC_ALL=C (stable messages).
type Exec struct{ Env []string }

func (x Exec) Run(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), x.Env...), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok && ee.Exited() {
		return stdout.Bytes(), &ExitError{Args: args, Code: ee.ExitCode(), Stderr: stderr.String()}
	}
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return stdout.Bytes(), nil
}

// Repo is a git repository at Dir, bare or not.
type Repo struct {
	Git Git
	Dir string
}

// Entry is one ls-tree line.
type Entry struct {
	Mode, Type, Object, Path string
}

// Bytes runs git in the repository and returns its raw output.
func (r Repo) Bytes(ctx context.Context, args ...string) ([]byte, error) {
	return r.Git.Run(ctx, r.Dir, nil, args...)
}

// Out runs git in the repository and returns its output without trailing
// newlines.
func (r Repo) Out(ctx context.Context, args ...string) (string, error) {
	b, err := r.Bytes(ctx, args...)
	return strings.TrimRight(string(b), "\n"), err
}

// RevParse resolves spec to an object id. ok is false when spec names no
// object (rev-parse --verify exits 1). A spec without ':' is peeled with
// ^{object}, so a well-formed id of an object the repository lacks is not ok.
func (r Repo) RevParse(ctx context.Context, spec string) (sha string, ok bool, err error) {
	arg := spec
	if !strings.Contains(spec, ":") {
		arg += "^{object}"
	}
	out, err := r.Out(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", arg)
	if ExitCode(err) == 1 {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return out, true, nil
}

// Gitlink returns the commit id recorded at path in rev's tree. ok is false
// when path is absent or is not a gitlink (mode 160000).
func (r Repo) Gitlink(ctx context.Context, rev, path string) (sha string, ok bool, err error) {
	e, ok, err := r.entry(ctx, rev, path)
	if err != nil || !ok || e.Mode != "160000" {
		return "", false, err
	}
	return e.Object, true, nil
}

// Show returns the blob at path in rev's tree. ok is false when path is
// absent; a path that is not a blob (a tree or a gitlink) is an error.
func (r Repo) Show(ctx context.Context, rev, path string) (data []byte, ok bool, err error) {
	e, ok, err := r.entry(ctx, rev, path)
	if err != nil || !ok {
		return nil, false, err
	}
	if e.Type != "blob" {
		return nil, false, fmt.Errorf("%s:%s is a %s, not a blob", rev, path, e.Type)
	}
	data, err = r.Blob(ctx, e.Object)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// Blob returns the content of blob id.
func (r Repo) Blob(ctx context.Context, id string) ([]byte, error) {
	return r.Bytes(ctx, "cat-file", "blob", id)
}

// entry is the ls-tree entry for exactly path in rev.
func (r Repo) entry(ctx context.Context, rev, path string) (Entry, bool, error) {
	b, err := r.Bytes(ctx, "--literal-pathspecs", "ls-tree", "-z", "--full-tree", "--end-of-options", rev, "--", path)
	if err != nil {
		return Entry{}, false, err
	}
	es, err := parseLsTree(b)
	if err != nil {
		return Entry{}, false, err
	}
	for _, e := range es {
		if e.Path == path {
			return e, true, nil
		}
	}
	return Entry{}, false, nil
}

// LsTree lists every non-tree entry of rev at or below prefix ("" for the
// whole tree), recursively and by full path.
func (r Repo) LsTree(ctx context.Context, rev, prefix string) ([]Entry, error) {
	args := []string{"--literal-pathspecs", "ls-tree", "-r", "-z", "--full-tree", "--end-of-options", rev}
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix != "" {
		args = append(args, "--", prefix)
	}
	b, err := r.Bytes(ctx, args...)
	if err != nil {
		return nil, err
	}
	es, err := parseLsTree(b)
	if err != nil {
		return nil, err
	}
	out := es[:0]
	for _, e := range es {
		if prefix == "" || e.Path == prefix || strings.HasPrefix(e.Path, prefix+"/") {
			out = append(out, e)
		}
	}
	return out, nil
}

func parseLsTree(b []byte) ([]Entry, error) {
	var es []Entry
	for rec := range strings.SplitSeq(string(b), "\x00") {
		if rec == "" {
			continue
		}
		meta, path, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			return nil, fmt.Errorf("ls-tree: malformed entry %q", rec)
		}
		es = append(es, Entry{Mode: f[0], Type: f[1], Object: f[2], Path: path})
	}
	return es, nil
}

// IsAncestor reports whether commit a is an ancestor of (or equal to) b.
func (r Repo) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	_, err := r.Bytes(ctx, "merge-base", "--is-ancestor", "--end-of-options", a, b)
	switch ExitCode(err) {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, err
}

// Parents returns rev's parent commit ids, first parent first.
func (r Repo) Parents(ctx context.Context, rev string) ([]string, error) {
	out, err := r.Out(ctx, "rev-list", "--parents", "--max-count=1", "--end-of-options", rev, "--")
	if err != nil {
		return nil, err
	}
	f := strings.Fields(out)
	if len(f) == 0 {
		return nil, fmt.Errorf("rev-list %s: no commit", rev)
	}
	return f[1:], nil
}

// Tree returns the id of rev's root tree.
func (r Repo) Tree(ctx context.Context, rev string) (string, error) {
	return r.Out(ctx, "rev-parse", "--verify", "--end-of-options", rev+"^{tree}")
}

// CommitDate returns rev's committer date in strict ISO 8601 (%cI), with
// the committer's own offset.
func (r Repo) CommitDate(ctx context.Context, rev string) (string, error) {
	return r.Out(ctx, "log", "-1", "--no-walk", "--no-show-signature", "--format=%cI", "--end-of-options", rev, "--")
}

// Trailers returns the trailers of rev's message as (key, value) pairs, in
// order, with continuation lines unfolded.
func (r Repo) Trailers(ctx context.Context, rev string) ([][2]string, error) {
	out, err := r.Out(ctx, "log", "-1", "--no-walk", "--no-show-signature", "--format=%(trailers:only,unfold)", "--end-of-options", rev, "--")
	if err != nil {
		return nil, err
	}
	var ts [][2]string
	for line := range strings.SplitSeq(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		ts = append(ts, [2]string{strings.TrimSpace(k), strings.TrimSpace(v)})
	}
	return ts, nil
}

// DiffNames lists the paths that differ between the trees of a and b, a
// rename counting as a deletion plus an addition.
func (r Repo) DiffNames(ctx context.Context, a, b string) ([]string, error) {
	out, err := r.Bytes(ctx, "diff-tree", "-r", "-z", "--name-only", "--no-renames", "--end-of-options", a, b, "--")
	if err != nil {
		return nil, err
	}
	var names []string
	for p := range strings.SplitSeq(string(out), "\x00") {
		if p != "" {
			names = append(names, p)
		}
	}
	return names, nil
}

// MergeTree merges commits a and b without touching the index or a
// worktree (git merge-tree --write-tree, the ort strategy that git merge
// uses) and returns the resulting tree. clean is false on a conflict.
func (r Repo) MergeTree(ctx context.Context, a, b string) (tree string, clean bool, err error) {
	out, err := r.Out(ctx, "merge-tree", "--write-tree", "--no-messages", "--end-of-options", a, b)
	code := ExitCode(err)
	if code != 0 && code != 1 {
		return "", false, err
	}
	tree, _, _ = strings.Cut(out, "\n")
	if tree == "" {
		return "", false, fmt.Errorf("merge-tree %s %s: no tree in output", a, b)
	}
	return tree, code == 0, nil
}

// Fetch runs git fetch remote refspecs....
func (r Repo) Fetch(ctx context.Context, remote string, refspecs ...string) error {
	_, err := r.Bytes(ctx, append([]string{"fetch", "--quiet", remote}, refspecs...)...)
	return err
}

// BlobID is the git object id (SHA-1) of data as a blob, as git hash-object
// computes it.
func BlobID(data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
