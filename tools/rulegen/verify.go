package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/upstream"
)

// The routine-commit verifier (`sync -check -content -rev C`). The routine
// pushes as the maintainer, so authorship cannot tell its commits from a
// human's; content does. A non-Dependabot commit is routine if one of two
// rules verifies it, chosen by its trailer:
//
//   - Rulegen-Sync: <G>, by content: G is C's gitlink, C changes only the
//     sync write set and the gitlink relative to its parent, and its
//     rules/brave/* and lock are exactly what sync produces for G from the
//     upstream clone. The ancestor-of-master, backwards-move and
//     .gitmodules checks are skipped: they gate merging, not authorship.
//     Exit 0 verified, 1 not routine, 2 unverifiable (upstream unreachable,
//     or G no longer served).
//   - Rulegen-Merge: main, by merge-tree: C has two parents, the second is
//     on the local origin/main, and C's tree is the clean ort merge of the
//     two. Exit 0 or 1.
//
// Any other commit exits 1. Errors exit 1 too, so a commit that cannot be
// verified for a local reason is never taken for the routine's.

const (
	msgVerified       = "verified: %s is a routine sync commit for upstream@%s"                                                              // C, G
	msgVerifiedStale  = "verified: %s is a routine sync commit for upstream@%s (stale after an upstream rewrite: %s is no longer on master)" // C, G, G
	msgVerifiedMerge  = "verified: %s is a routine merge of main (%s)"                                                                       // C, parent2
	msgNotRoutine     = "not a routine commit: %s: %s"                                                                                       // C, reason
	msgNoTrailer      = "no " + data.TrailerSync + " or " + data.TrailerMerge + " trailer"                                                   //
	msgBothTrailers   = "both " + data.TrailerSync + " and " + data.TrailerMerge + " trailers"                                               //
	msgTrailerTwice   = "%d %s trailers"                                                                                                     // n, key
	msgTrailerNotG    = data.TrailerSync + " names %q, but the gitlink is %s"                                                                // value, G
	msgParents        = "%d parents, want %d"                                                                                                // got, want
	msgOutsideSet     = "changes paths outside the sync write set and the gitlink: %s"                                                       // paths
	msgContentDiffers = "differs from sync's output for upstream@%s: %s"                                                                     // G, paths
	msgMergeValue     = data.TrailerMerge + " is %q, want %q"                                                                                // value, "main"
	msgParent2Off     = "second parent %s is not on origin/main"                                                                             // parent2
	msgMergeConflict  = "merging %s and %s conflicts"                                                                                        // parent1, parent2
	msgMergeTree      = "tree %s is not the merge of its parents (%s)"                                                                       // C's tree, merge tree
)

func verifyCommit(ctx context.Context, e *Env, root, dir, rev string) int {
	r := e.Repo(root)
	no := func(c, format string, a ...any) int {
		fmt.Fprintf(e.Stdout, msgNotRoutine+"\n", c, fmt.Sprintf(format, a...))
		return exitFail
	}
	c, ok, err := r.RevParse(ctx, rev+"^{commit}")
	if err != nil || !ok {
		return no(rev, "not a commit (%v)", err)
	}
	ts, err := r.Trailers(ctx, c)
	if err != nil {
		return no(c, "%v", err)
	}
	var syncVals, mergeVals []string
	for _, kv := range ts {
		switch kv[0] {
		case data.TrailerSync:
			syncVals = append(syncVals, kv[1])
		case data.TrailerMerge:
			mergeVals = append(mergeVals, kv[1])
		}
	}
	switch {
	case len(syncVals) > 0 && len(mergeVals) > 0:
		return no(c, msgBothTrailers)
	case len(syncVals) > 1:
		return no(c, msgTrailerTwice, len(syncVals), data.TrailerSync)
	case len(mergeVals) > 1:
		return no(c, msgTrailerTwice, len(mergeVals), data.TrailerMerge)
	case len(syncVals) == 1:
		code, msg := verifySync(ctx, r, e, dir, c, syncVals[0])
		if code == exitFail {
			return no(c, "%s", msg)
		}
		fmt.Fprintln(e.Stdout, msg)
		return code
	case len(mergeVals) == 1:
		msg, ok := verifyMerge(ctx, r, c, mergeVals[0])
		if !ok {
			return no(c, "%s", msg)
		}
		fmt.Fprintln(e.Stdout, msg)
		return exitOK
	}
	return no(c, msgNoTrailer)
}

// verifySync is the Rulegen-Sync content rule for commit c whose trailer
// names value. It returns the exit code and its message.
func verifySync(ctx context.Context, r repo.Repo, e *Env, dir, c, value string) (int, string) {
	ps, err := r.Parents(ctx, c)
	if err != nil {
		return exitFail, err.Error()
	}
	if len(ps) != 1 {
		return exitFail, fmt.Sprintf(msgParents, len(ps), 1)
	}
	g, ok, err := r.Gitlink(ctx, c, data.Submodule)
	if err != nil {
		return exitFail, err.Error()
	}
	if !ok {
		return exitFail, fmt.Sprintf(msgNoGitlink, data.Submodule, c)
	}
	if value != g {
		return exitFail, fmt.Sprintf(msgTrailerNotG, value, g)
	}
	names, err := r.DiffNames(ctx, ps[0], c)
	if err != nil {
		return exitFail, err.Error()
	}
	var outside []string
	for _, p := range names {
		if !data.InWriteSet(p) && p != data.Submodule {
			outside = append(outside, p)
		}
	}
	if len(outside) > 0 {
		return exitFail, fmt.Sprintf(msgOutsideSet, strings.Join(outside, ", "))
	}

	clone, status, err := upstream.Open(e.Git, dir)
	if err != nil {
		return exitFail, err.Error()
	}
	if strings.HasPrefix(status, data.StatusUnreachablePrefix) {
		// The clone itself failed (or upstream was down when it was made).
		return exitUnverifiable, data.MsgUnverifiableDown
	}
	// G is fetched here, so the clone may have been made from any gitlink.
	switch st := clone.FetchCommit(ctx, g); {
	case st == data.StatusGitlinkMissing:
		return exitUnverifiable, data.MsgUnverifiableGone
	case st != data.StatusOK:
		return exitUnverifiable, data.MsgUnverifiableDown
	}
	prevLock, _, err := r.Show(ctx, ps[0], data.LockPath)
	if err != nil {
		return exitFail, err.Error()
	}
	out, err := syncCompute(ctx, clone, g, syncLenientLock(prevLock))
	if _, ok := errors.AsType[*upstream.FetchError](err); ok {
		return exitUnverifiable, data.MsgUnverifiableDown
	}
	if err != nil {
		return exitFail, err.Error()
	}
	if diff, err := verifyTree(ctx, r, c, out.Files); err != nil {
		return exitFail, err.Error()
	} else if len(diff) > 0 {
		return exitFail, fmt.Sprintf(msgContentDiffers, g, strings.Join(diff, ", "))
	}
	if on, err := clone.OnMaster(ctx, g); err != nil {
		return exitFail, err.Error()
	} else if !on {
		return exitOK, fmt.Sprintf(msgVerifiedStale, c, g, g)
	}
	return exitOK, fmt.Sprintf(msgVerified, c, g)
}

// verifyTree compares c's write set with want and lists the paths that
// differ: rules/brave must hold exactly want's files there and the lock
// must equal want's, each a regular non-executable blob.
func verifyTree(ctx context.Context, r repo.Repo, c string, want map[string][]byte) ([]string, error) {
	es, err := r.LsTree(ctx, c, syncDir)
	if err != nil {
		return nil, err
	}
	le, err := r.LsTree(ctx, c, data.LockPath)
	if err != nil {
		return nil, err
	}
	have := map[string]repo.Entry{}
	for _, x := range append(es, le...) {
		have[x.Path] = x
	}
	var diff []string
	for p, b := range want {
		x, ok := have[p]
		if !ok || x.Mode != "100644" || x.Type != "blob" || x.Object != repo.BlobID(b) {
			diff = append(diff, p)
		}
	}
	for p := range have {
		if _, ok := want[p]; !ok {
			diff = append(diff, p)
		}
	}
	slices.Sort(diff)
	return diff, nil
}

// verifyMerge is the Rulegen-Merge merge-tree rule for commit c.
func verifyMerge(ctx context.Context, r repo.Repo, c, value string) (string, bool) {
	if value != data.MergeTrailerValue {
		return fmt.Sprintf(msgMergeValue, value, data.MergeTrailerValue), false
	}
	ps, err := r.Parents(ctx, c)
	if err != nil {
		return err.Error(), false
	}
	if len(ps) != 2 {
		return fmt.Sprintf(msgParents, len(ps), 2), false
	}
	main, ok, err := r.RevParse(ctx, "refs/remotes/origin/main^{commit}")
	if err != nil {
		return err.Error(), false
	}
	if !ok {
		return msgNoOriginMain, false
	}
	if on, err := r.IsAncestor(ctx, ps[1], main); err != nil {
		return err.Error(), false
	} else if !on {
		return fmt.Sprintf(msgParent2Off, ps[1]), false
	}
	tree, clean, err := r.MergeTree(ctx, ps[0], ps[1])
	if err != nil {
		return err.Error(), false
	}
	if !clean {
		return fmt.Sprintf(msgMergeConflict, ps[0], ps[1]), false
	}
	have, err := r.Tree(ctx, c)
	if err != nil {
		return err.Error(), false
	}
	if have != tree {
		return fmt.Sprintf(msgMergeTree, have, tree), false
	}
	return fmt.Sprintf(msgVerifiedMerge, c, ps[1]), true
}
