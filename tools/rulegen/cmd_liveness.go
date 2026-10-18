package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	// liveBotLogin is Dependabot's GitHub login, as the REST API spells it
	// for the author of a PR and of a commit Dependabot made itself.
	liveBotLogin = "dependabot[bot]"
	// liveBotEmail is the commit email GitHub resolves to liveBotLogin; a
	// Co-authored-by trailer naming it is Dependabot's.
	liveBotEmail = "49699333+dependabot[bot]@users.noreply.github.com"
	// liveHeadPrefix starts the head branch of every Dependabot submodule PR.
	liveHeadPrefix = "dependabot/submodules/"
	// liveLabel marks the PRs liveness reads.
	liveLabel = "rules-update"

	// liveCheckPage ends the liveness alert: Dependabot's job errors have no
	// documented API, so a human has to look.
	liveCheckPage = "; check the repository's Dependabot page (Insights → Dependency graph → Dependabot) for errors"
)

// liveProduce writes the -prs file with GitHub REST calls only (GraphQL,
// which gh's PR, issue and label subcommands use, is unavailable to the
// routine). ROUTINE.md
// gives the same command; a test keeps them equal. Every call is a GET
// (gh api sends POST once a field flag is given, so there is none) and
// paginates, so the listing is complete: it is not bounded. Its size grows
// by one PR object a week, plus one comments and one commits page for each
// unmerged Dependabot submodule PR, which only the routine's closes and
// Dependabot's supersedes add.
const liveProduce = `(
  R=repos/Espresso-Aficionados/linkclean; F=/tmp/lc/prs.json
  gh api --paginate "$R/pulls?state=all&per_page=100" \
    --jq '.[] | select(any(.labels[]; .name == "rules-update"))' > "$F" || exit 1
  for n in $(jq -r 'select(.merged_at == null and (.head.ref | startswith("dependabot/submodules/"))) | .number' "$F"); do
    gh api --paginate "$R/issues/$n/comments?per_page=100" --jq "{pull_number: $n, comments: .}" >> "$F" || exit 1
    gh api --paginate "$R/pulls/$n/commits?per_page=100" --jq "{pull_number: $n, commits: .}" >> "$F" || exit 1
  done
) || echo "listing PRs failed"`

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

  -prs: every rules-update PR, as this command writes it with GitHub REST
      calls only (ROUTINE.md step 1 runs the same command):

` + liveIndent(liveProduce, "        ") + `

      If it prints "listing PRs failed", do not run liveness on its output.
      The file is a stream of JSON values: each PR object of
      GET /repos/{owner}/{repo}/pulls, and for every unmerged
      dependabot/submodules/... PR one value per page of its comments
      (GET .../issues/<n>/comments) and its commits (GET .../pulls/<n>/commits),
      wrapped as {"pull_number": <n>, "comments": [...]} and
      {"pull_number": <n>, "commits": [...]}. Fields liveness does not read
      are ignored.
  -root: a linkclean clone holding -main and the head commit of every
      PR whose gitlink is needed; fetch them first with
        git -C <root> fetch origin main '+refs/pull/*/head:refs/remotes/origin/pull/*'
  -upstream: the clone written by 'rulegen upstream'.
  -master: upstream master as 'git ls-remote https://github.com/brave/adblock-lists
      refs/heads/master' reports it; it must equal the clone's master (re-run
      'rulegen upstream' if not). Omitted: the clone's master.

The verdict, the first of these that applies, is one line on stdout:

  ok              a Dependabot submodule PR (author dependabot[bot], head
                  dependabot/submodules/...) was opened, in any state, on or
                  after 00:00 UTC 14 days before today.
  blocked #<n>    open Dependabot submodule PR #<n> (the newest such) carries
                  a commit whose authors (its author login and any
                  Co-authored-by trailer) are not all dependabot[bot]
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

// livePR is one rules-update PR of the -prs stream, as liveness reads it.
type livePR struct {
	Number      int
	State       string // OPEN, CLOSED (not merged) or MERGED
	Login       string // the PR author's
	HeadRefName string
	HeadRefOid  string
	CreatedAt   time.Time
	Comments    []string // comment bodies
	Commits     []liveCommitAuthors

	hasComments, hasCommits bool // a comments or commits value was listed
}

// liveCommitAuthors is one commit of a PR: its id, subject and the logins
// of its authors, "" for one GitHub resolved to no account.
type liveCommitAuthors struct {
	Oid, Headline string
	Logins        []string
}

func (p *livePR) dependabotSubmodule() bool {
	return p.Login == liveBotLogin && strings.HasPrefix(p.HeadRefName, liveHeadPrefix)
}

// takeover reports whether p is a takeover PR: one a human opened.
func (p *livePR) takeover() bool { return p.Login != liveBotLogin }

// foreignCommits lists the commits whose authors are not all Dependabot.
func (p *livePR) foreignCommits() []string {
	var out []string
	for _, c := range p.Commits {
		ok := len(c.Logins) > 0
		for _, a := range c.Logins {
			ok = ok && a == liveBotLogin
		}
		if !ok {
			out = append(out, liveShort(c.Oid)+" "+c.Headline)
		}
	}
	return out
}

func (p *livePR) closedByRoutine() bool {
	if p.State != "CLOSED" {
		return false
	}
	for _, c := range p.Comments {
		body := strings.TrimLeft(c, " \t\r\n")
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
	prsPath := fs.String("prs", "", "file written by the REST command above")
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
		return "", fmt.Errorf("#%d: head.sha %q is not a commit id", p.Number, p.HeadRefOid)
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

// The REST shapes liveness reads; other fields are ignored.
type (
	restPull struct {
		Number    int        `json:"number"`
		State     string     `json:"state"` // open or closed
		MergedAt  *time.Time `json:"merged_at"`
		CreatedAt time.Time  `json:"created_at"`
		User      *restUser  `json:"user"`
		Head      struct {
			Ref string `json:"ref"`
			Sha string `json:"sha"`
		} `json:"head"`
		Labels []restLabel `json:"labels"`
	}
	restLabel struct {
		Name string `json:"name"`
	}
	restUser struct {
		Login string `json:"login"`
	}
	restPage struct {
		PullNumber int `json:"pull_number"`
		Comments   *[]struct {
			Body string `json:"body"`
		} `json:"comments"`
		Commits *[]struct {
			Sha    string    `json:"sha"`
			Author *restUser `json:"author"` // null: no account matches the email
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
		} `json:"commits"`
	}
)

// livePullKeys must be in every PR object: a missing merged_at would read
// as an unmerged PR, missing labels as one that is not rules-update.
var livePullKeys = []string{"number", "state", "merged_at", "created_at", "user", "head", "labels"}

// liveReadPRs reads the -prs stream and returns its rules-update PRs.
func liveReadPRs(path string) ([]livePR, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	bad := func(i int, format string, args ...any) error {
		return fmt.Errorf("%s: value %d: %s; use the command of rulegen liveness -h", path, i, fmt.Sprintf(format, args...))
	}
	var prs []livePR
	all := map[int]bool{} // every PR listed, labelled or not
	at := map[int]int{}   // rules-update PR number → index in prs
	var pages []restPage  // in stream order, applied once every PR is known
	var pageAt []int      // stream index of each page
	dec := json.NewDecoder(f)
	for i := 1; ; i++ {
		var raw json.RawMessage
		if err := dec.Decode(&raw); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			return nil, bad(i, "not a JSON object")
		}
		if _, ok := keys["pull_number"]; ok {
			var pg restPage
			if err := json.Unmarshal(raw, &pg); err != nil {
				return nil, fmt.Errorf("%s: value %d: %w", path, i, err)
			}
			if (pg.Comments == nil) == (pg.Commits == nil) {
				return nil, bad(i, "a page needs exactly one of \"comments\" and \"commits\"")
			}
			pages, pageAt = append(pages, pg), append(pageAt, i)
			continue
		}
		if _, ok := keys["head"]; !ok {
			return nil, bad(i, "neither a PR object nor a comments or commits page")
		}
		for _, k := range livePullKeys {
			if _, ok := keys[k]; !ok {
				return nil, bad(i, "PR object has no %q", k)
			}
		}
		var rp restPull
		if err := json.Unmarshal(raw, &rp); err != nil {
			return nil, fmt.Errorf("%s: value %d: %w", path, i, err)
		}
		p := livePR{Number: rp.Number, HeadRefName: rp.Head.Ref, HeadRefOid: rp.Head.Sha, CreatedAt: rp.CreatedAt}
		if rp.User != nil {
			p.Login = rp.User.Login
		}
		switch {
		case rp.State == "open" && rp.MergedAt == nil:
			p.State = "OPEN"
		case rp.State == "closed" && rp.MergedAt == nil:
			p.State = "CLOSED"
		case rp.State == "closed":
			p.State = "MERGED"
		}
		switch {
		case p.Number <= 0:
			return nil, fmt.Errorf("%s: PR without a number", path)
		case all[p.Number]:
			return nil, fmt.Errorf("%s: #%d listed twice", path, p.Number)
		case p.State == "":
			return nil, fmt.Errorf("%s: #%d: unknown state %q", path, p.Number, rp.State)
		case p.CreatedAt.IsZero():
			return nil, fmt.Errorf("%s: #%d: no created_at", path, p.Number)
		case p.Login == "":
			return nil, fmt.Errorf("%s: #%d: no author", path, p.Number)
		}
		all[p.Number] = true
		if slices.Contains(rp.Labels, restLabel{liveLabel}) {
			at[p.Number] = len(prs)
			prs = append(prs, p)
		}
	}
	for j, pg := range pages {
		if !all[pg.PullNumber] {
			return nil, fmt.Errorf("%s: value %d: a page of #%d, which is not listed", path, pageAt[j], pg.PullNumber)
		}
		k, ok := at[pg.PullNumber]
		if !ok {
			continue
		}
		p := &prs[k]
		if pg.Comments != nil {
			p.hasComments = true
			for _, c := range *pg.Comments {
				p.Comments = append(p.Comments, c.Body)
			}
			continue
		}
		p.hasCommits = true
		for _, c := range *pg.Commits {
			lc := liveCommitAuthors{Oid: c.Sha, Headline: liveHeadline(c.Commit.Message)}
			if c.Author != nil {
				lc.Logins = append(lc.Logins, c.Author.Login)
			} else {
				lc.Logins = append(lc.Logins, "")
			}
			lc.Logins = append(lc.Logins, liveCoAuthors(c.Commit.Message)...)
			p.Commits = append(p.Commits, lc)
		}
	}
	// What liveness reads must be listed: the commits of an open and the
	// comments of a closed Dependabot submodule PR.
	for _, p := range prs {
		switch {
		case !p.dependabotSubmodule():
		case p.State == "OPEN" && !p.hasCommits:
			return nil, fmt.Errorf("%s: open #%d has no commits page; use the command of rulegen liveness -h", path, p.Number)
		case p.State == "CLOSED" && !p.hasComments:
			return nil, fmt.Errorf("%s: closed #%d has no comments page; use the command of rulegen liveness -h", path, p.Number)
		}
	}
	return prs, nil
}

// liveHeadline is the first line of a commit message.
func liveHeadline(msg string) string {
	h, _, _ := strings.Cut(msg, "\n")
	return strings.TrimSpace(h)
}

// liveCoAuthors returns one login per Co-authored-by trailer in the last
// paragraph of msg, as GitHub counts the authors of a commit: Dependabot's
// for its own email, "" for any other (liveness cannot resolve accounts).
func liveCoAuthors(msg string) []string {
	msg = strings.TrimRight(strings.ReplaceAll(msg, "\r\n", "\n"), "\n")
	if i := strings.LastIndex(msg, "\n\n"); i >= 0 {
		msg = msg[i+2:]
	} else {
		return nil // a subject alone carries no trailers
	}
	var out []string
	for line := range strings.SplitSeq(msg, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "Co-authored-by") {
			continue
		}
		login := ""
		if strings.Contains(strings.ToLower(v), "<"+liveBotEmail+">") {
			login = liveBotLogin
		}
		out = append(out, login)
	}
	return out
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

// liveIndent prefixes every line of s with ind.
func liveIndent(s, ind string) string {
	return ind + strings.ReplaceAll(s, "\n", "\n"+ind)
}

func liveShort(sha string) string {
	if len(sha) > data.ShortLen {
		return sha[:data.ShortLen]
	}
	return sha
}
