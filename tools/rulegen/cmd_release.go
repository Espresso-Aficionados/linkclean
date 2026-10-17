package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/forge"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
)

// rulegen release tags linkclean.
//
//	release -root .. -pr <n>                   data-only patch release (the routine)
//	release -root .. -manual -bump minor|patch  every other release (the maintainer)
//
// Both modes fetch origin's main and tags first, require a green push run
// of the ci workflow on the commit being tagged, take L = the highest vX.Y.Z
// tag reachable from it, and classify every path changed since L. The
// tag and the release are created through the GitHub REST API (token from
// GH_TOKEN), and the module proxy is asked for the new version.

func init() { register("release", runRelease) }

const (
	relDefaultRepo = "Espresso-Aficionados/linkclean"
	relModule      = "github.com/Espresso-Aficionados/linkclean"
	relWorkflow    = "ci.yml"
	relMainRef     = "refs/remotes/origin/main"
	relProxy       = "https://proxy.golang.org"
	relPollEvery   = 30 * time.Second
	relPollFor     = 30 * time.Minute
	relWarmTries   = 3
	relWarmEvery   = 20 * time.Second
	relListMax     = 20 // paths named in one refusal
)

// Messages of rulegen release beyond the shared ones in data.
const (
	relMsgNotMerged     = "PR #%d is not merged"                                                           // n
	relMsgNotMain       = "PR #%d was merged into %s, not main"                                            // n, base
	relMsgNotOnMain     = "merge commit %s of PR #%d is not on origin/main"                                // S, n
	relMsgNoTag         = "no vX.Y.Z tag is reachable from %s; the first release is manual (§11.7 manual)" // S
	relMsgNoChange      = "no change since %s; nothing to release"                                         // L
	relMsgTagExists     = "tag %s already exists"                                                          // tag
	relMsgBumpTooLow    = "-bump %s is below the minimum %s for the changes since %s: %s"                  // bump, minimum, L, reasons
	relMsgNoSection     = "CHANGELOG.md has no ## %s section"                                              // tag
	relMsgEmptySection  = "CHANGELOG.md's ## %s section is empty"                                          // tag
	relMsgUnlistedRows  = "CHANGELOG.md's ## %s section does not list the changed golden rows: %s"         // tag, ids
	relMsgNoSummary     = "_No rulegen summary comment was found on #%d._"                                 // n
	relMsgReleaseFailed = "tag %s was created on %s, but creating its release failed: %v"                  // tag, S, err
	relMsgWarmFailed    = "warning: the module proxy did not list %s@%s: %v"                               // module, tag, err
	relMsgNoGHToken     = "GH_TOKEN is not set"                                                            //
	relNoTagName        = "the first commit (no vX.Y.Z tag)"                                               // L when there is none
)

// relSummaryAuthors are the author associations whose summary comment is
// taken as release notes: people with write access, as the routine is.
// Anyone can comment, so a marker alone is not enough.
var relSummaryAuthors = []string{"OWNER", "MEMBER", "COLLABORATOR"}

// relRefusal is a release the rules do not allow. manual marks the cases
// the routine reports as "manual release needed".
type relRefusal struct {
	msg    string
	manual bool
}

func (r *relRefusal) Error() string { return r.msg }

func relRefuse(manual bool, format string, args ...any) error {
	return &relRefusal{msg: fmt.Sprintf(format, args...), manual: manual}
}

type releaser struct {
	e    *Env
	r    repo.Repo
	f    forge.Forge
	logw io.Writer
}

func runRelease(ctx context.Context, e *Env, args []string) int {
	fs := newFlags(e, "release")
	root := fs.String("root", "", "linkclean checkout, with remote origin (required)")
	pr := fs.Int("pr", 0, "merged rules PR to release as a data-only patch")
	manual := fs.Bool("manual", false, "maintainer release of non-data changes")
	bump := fs.String("bump", "", "with -manual: minor or patch")
	repoName := fs.String("repo", relDefaultRepo, "GitHub repository owner/name (for tests)")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if !requireFlags(fs, "root") {
		return exitUsage
	}
	b, bumpOK := relParseBump(*bump)
	switch {
	case *manual && *pr != 0:
		fmt.Fprintln(e.Stderr, "rulegen release: -pr and -manual are mutually exclusive")
		return exitUsage
	case *manual && !bumpOK:
		fmt.Fprintln(e.Stderr, "rulegen release: -manual needs -bump minor or -bump patch")
		return exitUsage
	case !*manual && *pr <= 0:
		fmt.Fprintln(e.Stderr, "rulegen release: -pr <n> (data release) or -manual -bump minor|patch is required")
		return exitUsage
	case !*manual && *bump != "":
		fmt.Fprintln(e.Stderr, "rulegen release: -bump applies only to -manual; a data release is always a patch")
		return exitUsage
	case !forge.ValidRepo(*repoName):
		fmt.Fprintf(e.Stderr, "rulegen release: -repo %q is not owner/name\n", *repoName)
		return exitUsage
	}
	token := e.Getenv("GH_TOKEN")
	if token == "" {
		fmt.Fprintln(e.Stderr, "rulegen release: "+relMsgNoGHToken)
		return exitFail
	}
	rel := &releaser{
		e:    e,
		r:    e.Repo(*root),
		f:    &forge.REST{Client: e.HTTP, Base: e.APIBase, Repo: *repoName, Token: token},
		logw: e.Stderr,
	}
	var tag relVersion
	var err error
	if *manual {
		tag, err = rel.manualRelease(ctx, b)
	} else {
		tag, err = rel.dataRelease(ctx, *pr)
	}
	if err != nil {
		fmt.Fprintf(e.Stderr, "rulegen release: %v\n", err)
		if ref, ok := errors.AsType[*relRefusal](err); ok && ref.manual {
			fmt.Fprintf(e.Stderr, "rulegen release: %s\n", data.MsgManualRelease)
		}
		return exitFail
	}
	fmt.Fprintln(e.Stdout, tag)
	return exitOK
}

func (rel *releaser) logf(format string, args ...any) {
	fmt.Fprintf(rel.logw, "rulegen release: "+format+"\n", args...)
}

// dataRelease is §11.7's data release of merged PR n.
func (rel *releaser) dataRelease(ctx context.Context, n int) (relVersion, error) {
	if err := rel.fetch(ctx); err != nil {
		return relVersion{}, err
	}
	pr, err := rel.f.PullRequest(ctx, n)
	if err != nil {
		return relVersion{}, err
	}
	if !pr.Merged {
		return relVersion{}, relRefuse(false, relMsgNotMerged, n)
	}
	if pr.BaseRef != "main" {
		return relVersion{}, relRefuse(false, relMsgNotMain, n, pr.BaseRef)
	}
	s := pr.MergeCommitSHA
	if err := rel.onMain(ctx, s); err != nil {
		return relVersion{}, relRefuse(false, relMsgNotOnMain+" (%v)", s, n, err)
	}
	if err := rel.ciGate(ctx, s); err != nil {
		return relVersion{}, err
	}
	last, ok, err := relLatestTag(ctx, rel.r, s)
	if err != nil {
		return relVersion{}, err
	}
	if !ok {
		return relVersion{}, relRefuse(true, relMsgNoTag, s)
	}
	base, err := rel.tagCommit(ctx, last)
	if err != nil {
		return relVersion{}, err
	}
	ch, err := relClassify(ctx, rel.r, base, s)
	if err != nil {
		return relVersion{}, err
	}
	if other := ch.of(relOther); len(other) > 0 {
		return relVersion{}, relRefuse(true, data.MsgUnreleasedNonData, last, relJoin(other))
	}
	if len(ch.of(relData)) == 0 {
		return relVersion{}, relRefuse(false, data.MsgNoDataChange, last)
	}
	next := last.next(relPatch)
	if err := rel.tagFree(ctx, next); err != nil {
		return relVersion{}, err
	}
	notes, err := rel.dataNotes(ctx, n, last, ch)
	if err != nil {
		return relVersion{}, err
	}
	msg := fmt.Sprintf("linkclean %s: rules data refresh (#%d)", next, n)
	return next, rel.publish(ctx, next, s, msg, notes)
}

// manualRelease is §11.7's manual release of origin/main's head.
func (rel *releaser) manualRelease(ctx context.Context, b relBump) (relVersion, error) {
	if err := rel.fetch(ctx); err != nil {
		return relVersion{}, err
	}
	s, ok, err := rel.r.RevParse(ctx, relMainRef+"^{commit}")
	if err != nil {
		return relVersion{}, err
	}
	if !ok {
		return relVersion{}, fmt.Errorf("%s not found after fetching origin main", relMainRef)
	}
	if err := rel.ciGate(ctx, s); err != nil {
		return relVersion{}, err
	}
	last, hasTag, err := relLatestTag(ctx, rel.r, s)
	if err != nil {
		return relVersion{}, err
	}
	lastName := relNoTagName
	var base string
	if hasTag {
		lastName = last.String()
		base, err = rel.tagCommit(ctx, last)
	} else {
		base, err = rel.emptyTree(ctx)
	}
	if err != nil {
		return relVersion{}, err
	}
	ch, err := relClassify(ctx, rel.r, base, s)
	if err != nil {
		return relVersion{}, err
	}
	if len(ch.Paths) == 0 {
		return relVersion{}, relRefuse(false, relMsgNoChange, lastName)
	}
	minimum, reasons := relMinimum(ch)
	if b < minimum {
		return relVersion{}, relRefuse(false, relMsgBumpTooLow, b, minimum, lastName, strings.Join(reasons, ", "))
	}
	next := last.next(b)
	changelog, _, err := rel.r.Show(ctx, s, relChangelogPath)
	if err != nil {
		return relVersion{}, err
	}
	section, ok := relChangelogSection(changelog, next)
	switch {
	case !ok:
		return relVersion{}, relRefuse(false, relMsgNoSection, next)
	case section == "":
		return relVersion{}, relRefuse(false, relMsgEmptySection, next)
	}
	// The first release has no earlier rows to compare: every row is new.
	if b == relMinor && hasTag {
		var changed []string
		for _, p := range ch.Paths {
			if !strings.HasPrefix(p.Path, "testdata/golden/") {
				continue
			}
			oldB, _, err := rel.r.Show(ctx, base, p.Path)
			if err != nil {
				return relVersion{}, err
			}
			newB, _, err := rel.r.Show(ctx, s, p.Path)
			if err != nil {
				return relVersion{}, err
			}
			changed = append(changed, relChangedRows(p.Path, oldB, newB)...)
		}
		if missing := relUnlisted(section, changed); len(missing) > 0 {
			return relVersion{}, relRefuse(false, relMsgUnlistedRows, next, strings.Join(missing, ", "))
		}
	}
	if err := rel.tagFree(ctx, next); err != nil {
		return relVersion{}, err
	}
	return next, rel.publish(ctx, next, s, "linkclean "+next.String(), section+"\n")
}

// relMinimum is the smallest bump the changes allow (§11.7 manual, step 2)
// and, for minor, the reasons.
func relMinimum(ch *relChanges) (relBump, []string) {
	var reasons []string
	for _, p := range ch.Paths {
		if relMinorPath(p) {
			reasons = append(reasons, p.Path)
		}
	}
	if ch.GoDirective {
		reasons = append(reasons, "go.mod (go or toolchain directive)")
	}
	if len(reasons) == 0 {
		return relPatch, nil
	}
	if len(reasons) > relListMax {
		reasons = append(reasons[:relListMax:relListMax], fmt.Sprintf("and %d more", len(reasons)-relListMax))
	}
	return relMinor, reasons
}

// fetch updates origin/main and the tags; every later read is from git
// objects.
func (rel *releaser) fetch(ctx context.Context) error {
	if _, err := rel.r.Bytes(ctx, "fetch", "--quiet", "--tags", "origin", "main"); err != nil {
		return fmt.Errorf("fetching origin main and tags: %w", err)
	}
	return nil
}

// onMain checks that s is a commit reachable from origin/main.
func (rel *releaser) onMain(ctx context.Context, s string) error {
	if !relIsHex(s, 40) {
		return fmt.Errorf("%q is not a commit id", s)
	}
	if _, ok, err := rel.r.RevParse(ctx, s+"^{commit}"); err != nil || !ok {
		return fmt.Errorf("not in the fetched history: %v", err)
	}
	ok, err := rel.r.IsAncestor(ctx, s, relMainRef)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("not an ancestor of " + relMainRef)
	}
	return nil
}

// ciGate waits up to relPollFor for the push run of the ci workflow on s
// and refuses unless it concluded success.
func (rel *releaser) ciGate(ctx context.Context, s string) error {
	for waited := time.Duration(0); ; waited += relPollEvery {
		runs, err := rel.f.WorkflowRuns(ctx, relWorkflow, s, "push")
		if err != nil {
			return fmt.Errorf("reading the push CI runs on %s: %w", s, err)
		}
		state, done := relRunState(runs, s)
		if state == "success" {
			return nil
		}
		if done || waited >= relPollFor {
			return relRefuse(false, data.MsgPushCI, s, state)
		}
		if err := rel.e.Sleep(ctx, relPollEvery); err != nil {
			return err
		}
	}
}

// relRunState is the state of the newest push run on s: its conclusion
// once completed (done), else its status; "missing" when there is none.
func relRunState(runs []forge.Run, s string) (state string, done bool) {
	var newest *forge.Run
	for i := range runs {
		r := &runs[i]
		if r.HeadSHA == s && r.Event == "push" && (newest == nil || r.ID > newest.ID) {
			newest = r
		}
	}
	switch {
	case newest == nil:
		return "missing", false
	case newest.Status != "completed":
		return relOr(newest.Status, "pending"), false
	}
	return relOr(newest.Conclusion, "completed without a conclusion"), true
}

func relOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// tagCommit returns the commit tag v points at.
func (rel *releaser) tagCommit(ctx context.Context, v relVersion) (string, error) {
	c, ok, err := rel.r.RevParse(ctx, "refs/tags/"+v.String()+"^{commit}")
	if err == nil && !ok {
		err = fmt.Errorf("tag %s does not resolve to a commit", v)
	}
	return c, err
}

// tagFree refuses a tag name that exists already, on any commit.
func (rel *releaser) tagFree(ctx context.Context, v relVersion) error {
	_, ok, err := rel.r.RevParse(ctx, "refs/tags/"+v.String())
	if err != nil {
		return err
	}
	if ok {
		return relRefuse(false, relMsgTagExists, v)
	}
	return nil
}

// emptyTree returns the id of the empty tree, the base when no tag exists.
func (rel *releaser) emptyTree(ctx context.Context) (string, error) {
	out, err := rel.r.Git.Run(ctx, rel.r.Dir, []byte{}, "hash-object", "-t", "tree", "--stdin")
	return strings.TrimSpace(string(out)), err
}

// dataNotes builds the notes of a data release: the routine's summary
// comment on PR n, then the classification entries, dependency bumps and
// inert paths since last.
func (rel *releaser) dataNotes(ctx context.Context, n int, last relVersion, ch *relChanges) (string, error) {
	comments, err := rel.f.Comments(ctx, n)
	if err != nil {
		return "", fmt.Errorf("reading the comments on #%d: %w", n, err)
	}
	summary := ""
	for _, c := range comments {
		if strings.Contains(c.Body, data.SummaryMarker) && slices.Contains(relSummaryAuthors, c.AuthorAssociation) {
			summary = strings.TrimSpace(strings.ReplaceAll(c.Body, data.SummaryMarker, ""))
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Rules data refresh from #%d.\n\n", n)
	if summary == "" {
		rel.logf("no summary comment on #%d; the notes say so", n)
		summary = fmt.Sprintf(relMsgNoSummary, n)
	}
	b.WriteString(summary + "\n")
	if len(ch.Classified) > 0 || len(ch.Declassed) > 0 {
		b.WriteString("\n### Debounce classifications\n\n")
		for _, e := range ch.Classified {
			fmt.Fprintf(&b, "- `%s`: %s\n", e.Pattern, e.Class)
		}
		for _, p := range ch.Declassed {
			fmt.Fprintf(&b, "- `%s`: removed (pattern gone upstream)\n", p)
		}
	}
	if deps := ch.of(relDeps); len(deps) > 0 {
		b.WriteString("\n### Dependencies\n\n")
		for _, d := range ch.Deps {
			fmt.Fprintf(&b, "- %s\n", d)
		}
		if len(ch.Deps) == 0 {
			for _, p := range deps {
				fmt.Fprintf(&b, "- `%s`\n", p.Path)
			}
		}
	}
	if inert := ch.of(relInert); len(inert) > 0 {
		fmt.Fprintf(&b, "\n### Inert changes since %s\n\n", last)
		for _, p := range inert {
			fmt.Fprintf(&b, "- `%s`\n", p.Path)
		}
	}
	return b.String(), nil
}

// publish creates the annotated tag on s and its release, then asks the
// module proxy for the version. A failed warm-up is a warning: the tag
// exists, and the proxy fetches it on first request anyway.
func (rel *releaser) publish(ctx context.Context, v relVersion, s, msg, notes string) error {
	if err := rel.f.CreateTag(ctx, v.String(), s, msg); err != nil {
		return fmt.Errorf("creating tag %s on %s: %w", v, s, err)
	}
	if err := rel.f.CreateRelease(ctx, v.String(), v.String(), notes); err != nil {
		return fmt.Errorf(relMsgReleaseFailed, v, s, err)
	}
	rel.logf("tagged %s on %s and published its release", v, s)
	if err := rel.warmProxy(ctx, v); err != nil {
		rel.logf(relMsgWarmFailed, relModule, v, err)
	}
	return nil
}

// warmProxy runs `go list -m <module>@<v>` against proxy.golang.org, from
// an empty directory so no main module is involved.
func (rel *releaser) warmProxy(ctx context.Context, v relVersion) error {
	dir, err := os.MkdirTemp("", "rulegen-proxy-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	env := []string{"GOPROXY=" + relProxy, "GOFLAGS=", "GOWORK=off", "GONOPROXY=", "GOPRIVATE=", "GONOSUMDB="}
	var last error
	for try := range relWarmTries {
		if try > 0 {
			if err := rel.e.Sleep(ctx, relWarmEvery); err != nil {
				return err
			}
		}
		_, stderr, err := rel.e.Go(ctx, dir, env, "list", "-m", relModule+"@"+v.String())
		if err == nil {
			return nil
		}
		last = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(stderr)))
	}
	return last
}

// relJoin names paths for a refusal, at most relListMax of them.
func relJoin(ps []relPath) string {
	var names []string
	for i, p := range ps {
		if i == relListMax {
			names = append(names, fmt.Sprintf("and %d more", len(ps)-relListMax))
			break
		}
		names = append(names, p.String())
	}
	return strings.Join(names, ", ")
}

func relIsHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}
