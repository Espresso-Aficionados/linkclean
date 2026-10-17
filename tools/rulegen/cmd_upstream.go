package main

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/upstream"
)

func init() { register("upstream", runUpstream) }

const msgNoGitlink = "no gitlink at %s in %s" // path, rev

// runUpstream is `rulegen upstream -root R -out DIR`: the one clone
// procedure for CI, the routine and developers. It clones the pinned URL
// (data.CloneURL, never .gitmodules) blobless with no worktree, fetches the
// gitlink of R's HEAD, and records the outcome in DIR/STATUS. A network
// failure is recorded, not raised: the exit code is 0 whenever STATUS was
// written.
func runUpstream(ctx context.Context, e *Env, args []string) int {
	fs := newFlags(e, "upstream")
	root := fs.String("root", "", "linkclean checkout")
	out := fs.String("out", "", "directory for the clone (absent or empty)")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if !requireFlags(fs, "root", "out") {
		return exitUsage
	}
	dir, err := filepath.Abs(*out)
	if err != nil {
		fmt.Fprintf(e.Stderr, "rulegen upstream: %v\n", err)
		return exitUsage
	}
	g, ok, err := e.Repo(*root).Gitlink(ctx, "HEAD", data.Submodule)
	if err != nil {
		fmt.Fprintf(e.Stderr, "rulegen upstream: %v\n", err)
		return exitFail
	}
	if !ok {
		fmt.Fprintf(e.Stderr, "rulegen upstream: "+msgNoGitlink+"\n", data.Submodule, "HEAD")
		return exitFail
	}
	status, err := upstream.Make(ctx, e.Git, data.CloneURL, dir, g)
	if err != nil {
		fmt.Fprintf(e.Stderr, "rulegen upstream: %v\n", err)
		return exitFail
	}
	fmt.Fprintf(e.Stdout, "upstream: %s@%s: %s %s\n", data.CloneURL, g, data.StatusFile, status)
	return exitOK
}
