package main

import (
	"cmp"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
)

// relVersion is a strict release version vX.Y.Z: no leading zeros, no
// pre-release or build suffix.
type relVersion struct{ Major, Minor, Patch int }

func (v relVersion) String() string { return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch) }

func (v relVersion) compare(w relVersion) int {
	return cmp.Or(cmp.Compare(v.Major, w.Major), cmp.Compare(v.Minor, w.Minor), cmp.Compare(v.Patch, w.Patch))
}

// relBump names a release increment.
type relBump int

const (
	relPatch relBump = iota
	relMinor
)

func (b relBump) String() string {
	if b == relMinor {
		return "minor"
	}
	return "patch"
}

func relParseBump(s string) (relBump, bool) {
	switch s {
	case "patch":
		return relPatch, true
	case "minor":
		return relMinor, true
	}
	return 0, false
}

// next returns v bumped by b. linkclean stays on v0 (§13), so there is no
// major bump.
func (v relVersion) next(b relBump) relVersion {
	if b == relMinor {
		return relVersion{v.Major, v.Minor + 1, 0}
	}
	return relVersion{v.Major, v.Minor, v.Patch + 1}
}

// relParseVersion parses a strict vX.Y.Z tag name.
func relParseVersion(s string) (relVersion, bool) {
	rest, ok := strings.CutPrefix(s, "v")
	if !ok {
		return relVersion{}, false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return relVersion{}, false
	}
	var n [3]int
	for i, p := range parts {
		if p == "" || len(p) > 9 || len(p) > 1 && p[0] == '0' {
			return relVersion{}, false
		}
		for j := 0; j < len(p); j++ {
			if p[j] < '0' || p[j] > '9' {
				return relVersion{}, false
			}
		}
		n[i], _ = strconv.Atoi(p)
	}
	return relVersion{n[0], n[1], n[2]}, true
}

// relLatestTag returns the highest strict vX.Y.Z tag whose commit is reachable
// from rev (git tag --merged), and whether there is one.
func relLatestTag(ctx context.Context, r repo.Repo, rev string) (relVersion, bool, error) {
	out, err := r.Out(ctx, "tag", "--list", "--merged", rev, "v*")
	if err != nil {
		return relVersion{}, false, err
	}
	var best relVersion
	found := false
	for name := range strings.SplitSeq(out, "\n") {
		v, ok := relParseVersion(strings.TrimSpace(name))
		if ok && (!found || v.compare(best) > 0) {
			best, found = v, true
		}
	}
	return best, found, nil
}
