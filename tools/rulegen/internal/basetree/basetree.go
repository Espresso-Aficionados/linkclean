// Package basetree writes the base output set: every input a gate or
// rulegen summary reads from the base commit, taken from git objects (never
// a checkout). The set is rules/** (the vendored files, the overlay and
// rules/acks.json), rules.lock.json, .gitmodules when base has one, and a
// file GITLINK holding base's gitlink id for the upstream submodule when
// base has one. Everything else (the corpus, known-broken.txt,
// keep-coverage-exempt.txt) is read from the working tree.
package basetree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
)

// file is one member of the output set: a blob of rev, or the GITLINK text.
type file struct {
	path string
	blob string // object id; "" for GITLINK
	text []byte // GITLINK content
}

// set lists the output set of rev, sorted by path.
func set(ctx context.Context, r repo.Repo, rev string) ([]file, error) {
	commit, ok, err := r.RevParse(ctx, rev+"^{commit}")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("basetree: %s is not a commit", rev)
	}
	var fs []file
	entries, err := r.LsTree(ctx, commit, "rules")
	if err != nil {
		return nil, err
	}
	for _, p := range []string{data.LockPath, data.GitmodulesPath} {
		e, err := r.LsTree(ctx, commit, p)
		if err != nil {
			return nil, err
		}
		for _, x := range e {
			if x.Path == p {
				entries = append(entries, x)
			}
		}
	}
	for _, e := range entries {
		if e.Mode != "100644" && e.Mode != "100755" {
			return nil, fmt.Errorf("basetree: %s:%s has mode %s; only regular files are written", rev, e.Path, e.Mode)
		}
		if err := checkPath(e.Path); err != nil {
			return nil, err
		}
		fs = append(fs, file{path: e.Path, blob: e.Object})
	}
	g, ok, err := r.Gitlink(ctx, commit, data.Submodule)
	if err != nil {
		return nil, err
	}
	if ok {
		fs = append(fs, file{path: data.GitlinkFile, text: []byte(g + "\n")})
	}
	slices.SortFunc(fs, func(a, b file) int { return strings.Compare(a.path, b.path) })
	return fs, nil
}

// checkPath rejects a tree path that would not stay inside the output
// directory as written.
func checkPath(p string) error {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "\\") {
		return fmt.Errorf("basetree: unsafe tree path %q", p)
	}
	return nil
}

// Paths lists the slash-separated paths Write writes for rev, sorted.
func Paths(ctx context.Context, r repo.Repo, rev string) ([]string, error) {
	fs, err := set(ctx, r, rev)
	if err != nil {
		return nil, err
	}
	ps := make([]string, len(fs))
	for i, f := range fs {
		ps[i] = f.path
	}
	return ps, nil
}

// Write writes rev's output set into the new directory out, which must not
// exist (its parent is created if needed). Files are written 0644. On error
// out may hold part of the set.
func Write(ctx context.Context, r repo.Repo, rev, out string) error {
	fs, err := set(ctx, r, rev)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(out, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("basetree: %s already exists", out)
		}
		return err
	}
	root, err := os.OpenRoot(out)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, f := range fs {
		b := f.text
		if f.blob != "" {
			if b, err = r.Blob(ctx, f.blob); err != nil {
				return err
			}
		}
		name := filepath.FromSlash(f.path)
		if dir := filepath.Dir(name); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		if err := root.WriteFile(name, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}
