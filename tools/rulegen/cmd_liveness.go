package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
)

func init() { register("liveness", runLiveness) }

// The review routine's step 1: is Dependabot's submodule path alive?
const (
	liveWindowDays = 14             // a Dependabot submodule PR opened this recently: ok
	liveCooldown   = 72 * time.Hour // Dependabot's default cooldown for gitsubmodule

	// liveCommitLogin is the GitHub login of every commit author on a
	// commit Dependabot made itself.
	liveCommitLogin = "dependabot[bot]"
	// liveHeadPrefix starts the head branch of every Dependabot submodule PR.
	liveHeadPrefix = "dependabot/submodules/"

	// liveCheckPage ends the liveness alert: Dependabot's job errors have no
	// documented API, so a human has to look.
	liveCheckPage = "; check the repository's Dependabot page (Insights → Dependency graph → Dependabot) for errors"

	// liveListLimit is the documented gh pr list --limit. GitHub refuses a
	// query that may return more than 500,000 nodes, and gh asks for 100
	// commits with 100 authors each, plus 100 comments, per PR: 10,201
	// nodes a PR, so at most 49 PRs. The newest 40 rules-update PRs cover
	// months of weekly PRs, far more than the 14-day window and the newest
	// routine-closed PR need.
	liveListLimit = 40
)

// livePRLogins are the logins gh reports for a PR that Dependabot opened.
// gh prints bot authors as "app/<slug>"; the REST spelling is accepted too.
var livePRLogins = []string{"app/dependabot", "dependabot[bot]"}

// liveClosePrefixes mark a PR the routine closed itself in a way that
// leaves its gitlink "already proposed" (held; gitlink off upstream master
// or gone; main's rules moved, by conflict or backwards move): a comment
// on a closed PR whose body starts with one of these, after leading
// blanks. They are the routine's close comments verbatim; the held one up
// to its first verb, since the reason and date vary and the routine
// appends its findings. The "superseded by #n" close is deliberately not
// one: its newer PR carries the newer gitlink.
var liveClosePrefixes = func() []string {
	held, _, _ := strings.Cut(data.CommentHeld, "%")
	return []string{data.CommentGone, data.CommentRulesMoved, data.CommentRulesMovedPast, held}
}()

var liveUsage = `usage: rulegen liveness -root <checkout> -upstream <clone> -prs <file> [-master <sha>] [-main <rev>] [-now <time>]

Decides step 1 of the review routine: whether the lack of a recent
Dependabot submodule PR means Dependabot's path is dead. It reads no
network; its inputs are:

  -prs: the newest rules-update PRs, as written by
      gh pr list --repo Espresso-Aficionados/linkclean --label rules-update \
        --state all --limit ` + strconv.Itoa(liveListLimit) + ` \
        --json ` + strings.Join(liveFields, ",") + ` \
        > "$TMP/prs.json"
      (a larger --limit exceeds GitHub's GraphQL node limit and gh fails;
      if gh fails, do not run liveness on its output)
  -root: a linkclean clone holding -main and the head commit of every
      PR whose gitlink is needed; fetch them first with
        git -C <root> fetch origin main '+refs/pull/*/head:refs/remotes/origin/pull/*'
  -upstream: the clone written by 'rulegen upstream'.
  -master: upstream master as 'git ls-remote https://github.com/brave/adblock-lists
      refs/heads/master' reports it; it must equal the clone's master (re-run
      'rulegen upstream' if not). Omitted: the clone's master.

The verdict, the first of these that applies, is one line on stdout:

  ok              a Dependabot submodule PR (author Dependabot, head
                  dependabot/submodules/...) was opened, in any state, on or
                  after 00:00 UTC 14 days before today.
  blocked #<n>    open Dependabot submodule PR #<n> (the newest such) carries
                  a commit whose authors are not all dependabot[bot]
                  (escalated or human-edited): Dependabot opens nothing newer
                  while it is open. Steps 2-4 report it.
  quiet (no change evidence)
                  upstream gave Dependabot nothing to propose: master M
                  equals G, or every commit in G..M was under 3 days old at
                  00:00 UTC on the most recent Monday (the cooldown as
                  Dependabot's Monday run saw it).
  ALERT <message> otherwise; the message is the routine's alert text.

G is the newest gitlink already proposed: among the gitlinks of every open
Dependabot submodule PR, of the newest open takeover PR (any other author)
and of the newest Dependabot submodule PR the routine closed, those that
descend from main's gitlink and are on upstream master, the one with the
fewest commits in G..M; if there is none, main's gitlink. A PR counts as
closed by the routine if it is closed (not merged) and carries a comment
starting with one of the routine's close comments:
  ` + strings.Join(liveClosePrefixes, "\n  ") + `
A gitlink off upstream master is never G (it is not in the clone); if
main's own gitlink is off master, the verdict is ALERT.

Exit codes: 0 ok, blocked or quiet; 1 ALERT; 2 undecidable (bad input,
missing objects; the reason is on stderr); 64 usage error. Details go to
stderr.

flags:
`

// livePR is one PR of gh pr list --json; other fields are ignored.
type livePR struct {
	Number      int       `json:"number"`
	State       string    `json:"state"` // OPEN, CLOSED or MERGED
	Author      liveUser  `json:"author"`
	HeadRefName string    `json:"headRefName"`
	HeadRefOid  string    `json:"headRefOid"`
	CreatedAt   time.Time `json:"createdAt"`
	Comments    []struct {
		Body string `json:"body"`
	} `json:"comments"`
	Commits []struct {
		Oid             string     `json:"oid"`
		MessageHeadline string     `json:"messageHeadline"`
		Authors         []liveUser `json:"authors"`
	} `json:"commits"`
}

type liveUser struct {
	Login string `json:"login"`
}

func (p *livePR) dependabotSubmodule() bool {
	return slices.Contains(livePRLogins, p.Author.Login) && strings.HasPrefix(p.HeadRefName, liveHeadPrefix)
}

// takeover reports whether p is a takeover PR: one a human opened.
func (p *livePR) takeover() bool { return !slices.Contains(livePRLogins, p.Author.Login) }

// foreignCommits lists the commits whose authors are not all Dependabot.
func (p *livePR) foreignCommits() []string {
	var out []string
	for _, c := range p.Commits {
		ok := len(c.Authors) > 0
		for _, a := range c.Authors {
			ok = ok && a.Login == liveCommitLogin
		}
		if !ok {
			out = append(out, liveShort(c.Oid)+" "+c.MessageHeadline)
		}
	}
	return out
}

func (p *livePR) closedByRoutine() bool {
	if p.State != "CLOSED" {
		return false
	}
	for _, c := range p.Comments {
		body := strings.TrimLeft(c.Body, " \t\r\n")
		for _, pre := range liveClosePrefixes {
			if strings.HasPrefix(body, pre) {
				return true
			}
		}
	}
	return false
}

func runLiveness(ctx context.Context, e *Env, args []string) int {
	fs := newFlags(e, "liveness")
	root := fs.String("root", "", "linkclean clone holding -main and the PR head commits")
	upstream := fs.String("upstream", "", "upstream clone written by rulegen upstream")
	prsPath := fs.String("prs", "", "gh pr list JSON of every rules-update PR")
	master := fs.String("master", "", "upstream master commit from git ls-remote (default: the clone's)")
	mainRev := fs.String("main", "origin/main", "revision of -root whose gitlink is main's")
	nowFlag := fs.String("now", "", "current time, RFC 3339 (tests)")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), liveUsage)
		fs.PrintDefaults()
	}
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if !requireFlags(fs, "root", "upstream", "prs") {
		return exitUsage
	}
	now := e.Now()
	if *nowFlag != "" {
		t, err := time.Parse(time.RFC3339, *nowFlag)
		if err != nil {
			fmt.Fprintf(e.Stderr, "rulegen liveness: -now: %v\n", err)
			return exitUsage
		}
		now = t
	}
	l := &liveness{
		ctx: ctx, e: e, now: now.UTC(),
		root: e.Repo(*root), up: e.Repo(*upstream), upDir: *upstream,
		master: *master, mainRev: *mainRev,
	}
	verdict, err := l.decide(*prsPath)
	if err != nil {
		fmt.Fprintf(e.Stderr, "rulegen liveness: undecidable: %v\n", err)
		return exitUnverifiable
	}
	fmt.Fprintln(e.Stdout, verdict)
	if strings.HasPrefix(verdict, "ALERT ") {
		return exitFail
	}
	return exitOK
}

type liveness struct {
	ctx      context.Context
	e        *Env
	now      time.Time
	root, up repo.Repo
	upDir    string
	master   string
	mainRev  string
}

func (l *liveness) note(format string, args ...any) {
	fmt.Fprintf(l.e.Stderr, "liveness: "+format+"\n", args...)
}

func (l *liveness) decide(prsPath string) (string, error) {
	prs, err := liveReadPRs(prsPath)
	if err != nil {
		return "", err
	}
	// Newest first: PR numbers grow with creation.
	slices.SortFunc(prs, func(a, b livePR) int { return b.Number - a.Number })

	cutoff := liveDay(l.now).AddDate(0, 0, -liveWindowDays)
	for _, p := range prs {
		if p.dependabotSubmodule() && !p.CreatedAt.Before(cutoff) {
			l.note("Dependabot submodule PR #%d opened %s, on or after %s", p.Number, p.CreatedAt.UTC().Format(time.RFC3339), cutoff.Format(time.DateOnly))
			return "ok", nil
		}
	}
	l.note("no Dependabot submodule PR opened on or after %s", cutoff.Format(time.DateOnly))

	for _, p := range prs {
		if p.State == "OPEN" && p.dependabotSubmodule() {
			if fc := p.foreignCommits(); len(fc) > 0 {
				l.note("open #%d carries non-Dependabot commits: %s", p.Number, strings.Join(fc, "; "))
				return fmt.Sprintf("blocked #%d", p.Number), nil
			}
		}
	}

	m, onMaster, err := l.upstreamMaster()
	if err != nil {
		return "", err
	}
	b, ok, err := l.root.Gitlink(l.ctx, l.mainRev, data.Submodule)
	if err != nil {
		return "", fmt.Errorf("main's gitlink: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("%s has no gitlink at %s", l.mainRev, data.Submodule)
	}
	g, from, err := l.newestProposed(prs, b, m, onMaster)
	if err != nil {
		return "", err
	}
	l.note("M = %s (upstream master); G = %s (%s); main's gitlink %s", liveShort(m), liveShort(g), from, liveShort(b))
	alert := "ALERT " + fmt.Sprintf(data.AlertNoPR, liveShort(g), liveShort(g)+".."+liveShort(m)) + liveCheckPage
	if g == m {
		l.note("M == G")
		return "quiet (no change evidence)", nil
	}
	if _, ok := onMaster[g]; !ok {
		// Only main's gitlink can get here; candidates must be on master.
		l.note("G is not on upstream master (history rewritten?), so G..M cannot be aged")
		return alert, nil
	}
	monday := liveMonday(l.now)
	commits, err := l.revList(g + ".." + m)
	if err != nil {
		return "", err
	}
	var old []string
	for _, c := range commits {
		if monday.Sub(c.when) >= liveCooldown {
			old = append(old, liveShort(c.sha)+" "+c.when.Format(time.RFC3339))
		}
	}
	if len(old) == 0 {
		l.note("all %d commits in G..M were under 3 days old at %s", len(commits), monday.Format(time.RFC3339))
		return "quiet (no change evidence)", nil
	}
	l.note("%d of %d commits in G..M were 3 days old or more at %s: %s", len(old), len(commits), monday.Format(time.RFC3339), strings.Join(old[:min(len(old), 5)], ", "))
	return alert, nil
}

// upstreamMaster returns upstream master M and every commit of master's
// history, with its committer time. Membership in that set is how
// liveness tells whether an object is in the clone: asking git about a
// missing commit would fetch it from the promisor remote.
func (l *liveness) upstreamMaster() (string, map[string]time.Time, error) {
	if st, err := os.ReadFile(filepath.Join(l.upDir, data.StatusFile)); err == nil {
		if s := strings.TrimSpace(string(st)); strings.HasPrefix(s, data.StatusUnreachablePrefix) {
			return "", nil, fmt.Errorf("upstream clone: %s", s)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}
	m, ok, err := l.up.RevParse(l.ctx, data.MasterRef)
	if err != nil {
		return "", nil, fmt.Errorf("upstream clone: %w", err)
	}
	if !ok {
		return "", nil, fmt.Errorf("upstream clone %s has no %s", l.upDir, data.MasterRef)
	}
	if l.master != "" && l.master != m {
		return "", nil, fmt.Errorf("upstream master is %s but the clone has %s; re-run rulegen upstream", l.master, m)
	}
	commits, err := l.revList(m)
	if err != nil {
		return "", nil, err
	}
	set := make(map[string]time.Time, len(commits))
	for _, c := range commits {
		set[c.sha] = c.when
	}
	return m, set, nil
}

type liveCommit struct {
	sha  string
	when time.Time
}

func (l *liveness) revList(spec string) ([]liveCommit, error) {
	out, err := l.up.Out(l.ctx, "rev-list", "--timestamp", "--end-of-options", spec, "--")
	if err != nil {
		return nil, err
	}
	var cs []liveCommit
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		ts, sha, ok := strings.Cut(line, " ")
		sec, err := strconv.ParseInt(ts, 10, 64)
		if !ok || err != nil {
			return nil, fmt.Errorf("rev-list: malformed line %q", line)
		}
		cs = append(cs, liveCommit{sha, time.Unix(sec, 0).UTC()})
	}
	return cs, nil
}

// newestProposed returns G and where it came from.
func (l *liveness) newestProposed(prs []livePR, b, m string, onMaster map[string]time.Time) (string, string, error) {
	type cand struct {
		pr   *livePR
		kind string
	}
	var cands []cand
	var takeover, closed bool
	for i := range prs {
		p := &prs[i]
		switch {
		case p.State == "OPEN" && p.dependabotSubmodule():
			cands = append(cands, cand{p, "open Dependabot PR"})
		case p.State == "OPEN" && p.takeover() && !takeover:
			takeover = true
			cands = append(cands, cand{p, "newest open takeover PR"})
		case p.dependabotSubmodule() && p.closedByRoutine() && !closed:
			closed = true
			cands = append(cands, cand{p, "newest routine-closed PR"})
		}
	}
	_, bOnMaster := onMaster[b]
	best, from, bestN := b, "main's gitlink", -1
	for _, c := range cands {
		g, err := l.prGitlink(c.pr)
		if err != nil {
			return "", "", err
		}
		if g == "" {
			l.note("#%d (%s) has no gitlink; skipped", c.pr.Number, c.kind)
			continue
		}
		if _, ok := onMaster[g]; !ok {
			l.note("#%d (%s): gitlink %s is not on upstream master; skipped", c.pr.Number, c.kind, liveShort(g))
			continue
		}
		desc := false
		if bOnMaster {
			if desc, err = l.up.IsAncestor(l.ctx, b, g); err != nil {
				return "", "", err
			}
		}
		if !desc {
			l.note("#%d (%s): main's gitlink is not an ancestor of its gitlink %s; skipped", c.pr.Number, c.kind, liveShort(g))
			continue
		}
		n, err := l.up.Out(l.ctx, "rev-list", "--count", "--end-of-options", g+".."+m, "--")
		if err != nil {
			return "", "", err
		}
		count, err := strconv.Atoi(n)
		if err != nil {
			return "", "", fmt.Errorf("rev-list --count: %q", n)
		}
		l.note("#%d (%s): gitlink %s, %d commits behind master", c.pr.Number, c.kind, liveShort(g), count)
		if bestN < 0 || count < bestN {
			best, from, bestN = g, fmt.Sprintf("#%d, %s", c.pr.Number, c.kind), count
		}
	}
	return best, from, nil
}

// prGitlink is the gitlink at p's head commit in -root, "" if it has none.
func (l *liveness) prGitlink(p *livePR) (string, error) {
	if !liveIsSHA(p.HeadRefOid) {
		return "", fmt.Errorf("#%d: headRefOid %q is not a commit id", p.Number, p.HeadRefOid)
	}
	if _, ok, err := l.root.RevParse(l.ctx, p.HeadRefOid); err != nil {
		return "", err
	} else if !ok {
		return "", fmt.Errorf("#%d: head %s is not in %s; fetch '+refs/pull/*/head:refs/remotes/origin/pull/*' first", p.Number, p.HeadRefOid, l.root.Dir)
	}
	g, ok, err := l.root.Gitlink(l.ctx, p.HeadRefOid, data.Submodule)
	if err != nil || !ok {
		return "", err
	}
	return g, nil
}

// liveFields are the gh pr list --json fields liveness reads.
var liveFields = []string{"number", "state", "author", "headRefName", "createdAt", "closedAt", "headRefOid", "comments", "commits"}

func liveReadPRs(path string) ([]livePR, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Every field of the documented gh command must be there: a missing
	// commits or comments list would read as an unedited or unclosed PR.
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var prs []livePR
	if err := json.Unmarshal(b, &prs); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[int]bool{}
	for i, p := range prs {
		for _, k := range liveFields {
			if _, ok := raw[i][k]; !ok {
				return nil, fmt.Errorf("%s: PR %d of the list has no %q; use the gh command of rulegen liveness -h", path, i+1, k)
			}
		}
		switch {
		case p.Number <= 0:
			return nil, fmt.Errorf("%s: PR without a number", path)
		case seen[p.Number]:
			return nil, fmt.Errorf("%s: #%d listed twice", path, p.Number)
		case p.State != "OPEN" && p.State != "CLOSED" && p.State != "MERGED":
			return nil, fmt.Errorf("%s: #%d: unknown state %q", path, p.Number, p.State)
		case p.CreatedAt.IsZero():
			return nil, fmt.Errorf("%s: #%d: no createdAt", path, p.Number)
		case p.Author.Login == "":
			return nil, fmt.Errorf("%s: #%d: no author", path, p.Number)
		}
		seen[p.Number] = true
	}
	return prs, nil
}

// liveDay is 00:00 UTC of t's UTC date.
func liveDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// liveMonday is 00:00 UTC on the most recent Monday at or before t: the
// earliest time Dependabot's weekly run (no time: in dependabot.yml) can
// start.
func liveMonday(t time.Time) time.Time {
	d := liveDay(t)
	return d.AddDate(0, 0, -((int(d.Weekday()) - int(time.Monday) + 7) % 7))
}

func liveIsSHA(s string) bool {
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

func liveShort(sha string) string {
	if len(sha) > data.ShortLen {
		return sha[:data.ShortLen]
	}
	return sha
}
