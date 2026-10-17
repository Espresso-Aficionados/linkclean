// Package upstream makes and reads the clone of brave/adblock-lists that
// rulegen's commands and the provenance gate share: a blobless,
// --no-checkout, single-branch clone of master holding commits and trees
// only, plus a one-line STATUS file that records whether upstream answered
// and served the gitlink. Blobs are fetched on demand, one per vendored
// file, from the clone's origin, which is always the pinned URL.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
)

// Make clones url into dir, which must be absent or an empty directory,
// fetches the gitlink commit g, and writes dir/STATUS. Network failures are
// recorded, not returned: status is data.StatusOK, data.StatusGitlinkMissing
// (master was fetched but upstream does not serve g) or
// data.StatusUnreachablePrefix plus git's error. err is a local failure
// (dir not empty, STATUS not writable), in which case nothing is recorded.
func Make(ctx context.Context, g repo.Git, url, dir, gitlink string) (status string, err error) {
	if err := checkEmpty(dir); err != nil {
		return "", err
	}
	_, cerr := g.Run(ctx, "", nil, "clone", "--quiet", "--filter=blob:none", "--no-checkout",
		"--single-branch", "--branch", data.Branch, "--", url, dir)
	if cerr != nil {
		status = data.StatusUnreachablePrefix + oneLine(cerr)
	} else {
		status = Clone{Repo: repo.Repo{Git: g, Dir: dir}}.FetchCommit(ctx, gitlink)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, data.StatusFile), []byte(status+"\n"), 0o644); err != nil {
		return "", err
	}
	return status, nil
}

func checkEmpty(dir string) error {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(ents) > 0 {
		return fmt.Errorf("%s exists and is not empty", dir)
	}
	return nil
}

// oneLine flattens a git error to one line for STATUS.
func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}

// Clone is a clone made by Make.
type Clone struct{ repo.Repo }

// Open returns the clone at dir. It fails unless dir holds a STATUS file;
// a clone whose STATUS is unreachable may have no repository at all.
func Open(g repo.Git, dir string) (Clone, string, error) {
	b, err := os.ReadFile(filepath.Join(dir, data.StatusFile))
	if err != nil {
		return Clone{}, "", fmt.Errorf("%s is not an upstream clone made by rulegen upstream: %w", dir, err)
	}
	status, _, _ := strings.Cut(string(b), "\n")
	return Clone{Repo: repo.Repo{Git: g, Dir: dir}}, strings.TrimRight(status, "\r"), nil
}

// FetchCommit fetches commit sha from origin and classifies the result as
// Make's status does. A failed fetch is told apart by `git ls-remote
// origin`: if upstream answers that, it does not serve sha.
func (c Clone) FetchCommit(ctx context.Context, sha string) string {
	ferr := c.Fetch(ctx, "origin", sha)
	if ferr == nil {
		if ok, err := c.HasCommit(ctx, sha); err != nil || !ok {
			return data.StatusGitlinkMissing // fetched, but not a commit
		}
		return data.StatusOK
	}
	if _, err := c.Bytes(ctx, "ls-remote", "--quiet", "origin", "refs/heads/"+data.Branch); err == nil {
		return data.StatusGitlinkMissing
	}
	return data.StatusUnreachablePrefix + oneLine(ferr)
}

// HasCommit reports whether sha is a full commit id present in the clone.
func (c Clone) HasCommit(ctx context.Context, sha string) (bool, error) {
	if !isCommitID(sha) {
		return false, nil
	}
	_, err := c.Bytes(ctx, "cat-file", "-e", sha+"^{commit}")
	if err == nil {
		return true, nil
	}
	if repo.ExitCode(err) > 0 {
		return false, nil
	}
	return false, err
}

// OnMaster reports whether sha is present and an ancestor of upstream
// master.
func (c Clone) OnMaster(ctx context.Context, sha string) (bool, error) {
	return c.Descends(ctx, sha, data.MasterRef)
}

// Descends reports whether a is present and an ancestor of (or equal to) b.
func (c Clone) Descends(ctx context.Context, a, b string) (bool, error) {
	if ok, err := c.HasCommit(ctx, a); err != nil || !ok {
		return false, err
	}
	return c.IsAncestor(ctx, a, b)
}

// BlobIDs returns the blob id of every vendored file at commit rev, keyed
// by upstream path, read from trees only (nothing is fetched). A path rev
// lacks is absent from the map.
func (c Clone) BlobIDs(ctx context.Context, rev string) (map[string]string, error) {
	ids := map[string]string{}
	for _, s := range data.Files {
		e, err := c.LsTree(ctx, rev, s.Upstream)
		if err != nil {
			return nil, err
		}
		for _, x := range e {
			if x.Path == s.Upstream && x.Type == "blob" {
				ids[s.Upstream] = x.Object
			}
		}
	}
	return ids, nil
}

// File is one vendored file read from upstream.
type File struct {
	Spec   data.Spec
	BlobID string
	Bytes  []byte
}

// MissingError is a vendored path the commit does not have.
type MissingError struct{ Commit, Path string }

func (e *MissingError) Error() string {
	return fmt.Sprintf("%s is missing at upstream@%s", e.Path, e.Commit)
}

// FetchError is a blob that could not be read, normally because the lazy
// fetch from upstream failed.
type FetchError struct {
	Path string
	Err  error
}

func (e *FetchError) Error() string {
	return fmt.Sprintf("reading %s from upstream: %v", e.Path, e.Err)
}
func (e *FetchError) Unwrap() error { return e.Err }

// ReadFiles reads the four vendored files at commit g, in lock order: the
// blob id from g's tree, the bytes by `git cat-file blob` (fetched on demand
// from origin), and a check that the bytes hash to the id. All four come
// from the one commit g.
func (c Clone) ReadFiles(ctx context.Context, g string) ([]File, error) {
	ids, err := c.BlobIDs(ctx, g)
	if err != nil {
		return nil, err
	}
	out := make([]File, 0, len(data.Files))
	for _, s := range data.Files {
		id, ok := ids[s.Upstream]
		if !ok {
			return nil, &MissingError{Commit: g, Path: s.Upstream}
		}
		b, err := c.Blob(ctx, id)
		if err != nil {
			return nil, &FetchError{Path: s.Upstream, Err: err}
		}
		if got := repo.BlobID(b); got != id {
			return nil, fmt.Errorf("%s at upstream@%s: bytes hash to %s, tree says %s", s.Upstream, g, got, id)
		}
		out = append(out, File{Spec: s, BlobID: id, Bytes: b})
	}
	return out, nil
}

func isCommitID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}
