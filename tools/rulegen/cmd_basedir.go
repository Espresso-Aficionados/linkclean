package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/basetree"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
)

func init() { register("basedir", runBasedir) }

// runBasedir writes the base output set (basetree) of the commit the CI
// event compares against:
//
//	pull_request  pull_request.base.sha from the payload
//	push          before from the payload; HEAD^ when it is all zeros
//
// Any other event is an error: ci has no other trigger, and the routine
// passes -base origin/main to summary directly. The payload's base covers a
// multi-commit PR branch as a whole, so a change made in an earlier commit
// than the last is still compared against the base branch.
func runBasedir(ctx context.Context, e *Env, args []string) int {
	fs := newFlags(e, "basedir")
	root := fs.String("root", "", "linkclean checkout")
	event := fs.String("event", "", "GitHub event name ($GITHUB_EVENT_NAME)")
	eventPath := fs.String("event-path", "", "event payload file ($GITHUB_EVENT_PATH)")
	out := fs.String("out", "", "output directory; must not exist")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if !requireFlags(fs, "root", "event", "event-path", "out") {
		return exitUsage
	}
	fail := func(err error) int {
		fmt.Fprintf(e.Stderr, "rulegen basedir: %v\n", err)
		return exitFail
	}
	rootDir, err := filepath.Abs(*root)
	if err != nil {
		return fail(err)
	}
	outDir, err := filepath.Abs(*out)
	if err != nil {
		return fail(err)
	}
	payload, err := os.ReadFile(*eventPath)
	if err != nil {
		return fail(err)
	}
	r := e.Repo(rootDir)
	rev, err := basedirRev(ctx, r, *event, payload)
	if err != nil {
		return fail(err)
	}
	if err := basetree.Write(ctx, r, rev, outDir); err != nil {
		return fail(err)
	}
	fmt.Fprintf(e.Stdout, "base %s (%s event)\n", rev, *event)
	return exitOK
}

// basedirRev resolves the base commit of a CI event to a full commit id
// present in r.
func basedirRev(ctx context.Context, r repo.Repo, event string, payload []byte) (string, error) {
	var p struct {
		Before      *string `json:"before"`
		PullRequest *struct {
			Base *struct {
				SHA *string `json:"sha"`
			} `json:"base"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", fmt.Errorf("event payload: %w", err)
	}
	var spec string
	switch event {
	case "pull_request":
		if p.PullRequest == nil || p.PullRequest.Base == nil || p.PullRequest.Base.SHA == nil {
			return "", errors.New("pull_request payload has no pull_request.base.sha")
		}
		spec = *p.PullRequest.Base.SHA
		if !basedirIsCommitID(spec) {
			return "", fmt.Errorf("pull_request.base.sha %q is not a commit id", spec)
		}
	case "push":
		if p.Before == nil {
			return "", errors.New("push payload has no before")
		}
		spec = *p.Before
		if !basedirIsCommitID(spec) {
			return "", fmt.Errorf("push before %q is not a commit id", spec)
		}
		if strings.Trim(spec, "0") == "" {
			spec = "HEAD^" // a new branch: compare against the pushed commit's parent
		}
	default:
		return "", fmt.Errorf("event %q has no base: basedir handles pull_request and push only (the routine passes -base origin/main to summary)", event)
	}
	sha, ok, err := r.RevParse(ctx, spec+"^{commit}")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("base %s is not a commit of %s; fetch it first", spec, r.Dir)
	}
	return sha, nil
}

// basedirIsCommitID reports whether s is a full SHA-1 or SHA-256 object id
// in lowercase hex, so a payload can never pass a revision expression or an
// option to git.
func basedirIsCommitID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}
