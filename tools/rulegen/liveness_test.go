package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/gittest"
)

// liveFixture is one liveness scenario (testdata/liveness/*.json): an
// upstream history, main's gitlink, PR heads by gitlink, and the values of
// the -prs stream (REST PR objects and wrapped comments and commits pages).
// "{{name}}" in them becomes the full id of upstream commit or head name;
// "{{short:name}}" in want its 12-hex short form.
type liveFixture struct {
	About    string            `json:"about"`
	Now      string            `json:"now"`
	Upstream []liveUpCommit    `json:"upstream"`
	Master   string            `json:"master"`
	Main     string            `json:"main"`  // upstream commit; "" for no gitlink
	Heads    map[string]string `json:"heads"` // head name → upstream commit of its gitlink
	PRs      []map[string]any  `json:"prs"`
	Want     string            `json:"want"`
	Code     int               `json:"code"`
}

type liveUpCommit struct {
	Name   string `json:"name"`
	Parent string `json:"parent"`
	Date   string `json:"date"`
}

func liveLoad(t *testing.T, name string) *liveFixture {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "liveness", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var f liveFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	t.Log(f.About)
	return &f
}

// liveResult is one run of rulegen liveness on a built fixture.
type liveResult struct {
	stdout, stderr string
	code           int
	names          map[string]string
}

var liveRef = regexp.MustCompile(`\{\{(short:)?([A-Za-z0-9_]+)\}\}`)

func (r *liveResult) expand(t *testing.T, s string) string {
	t.Helper()
	return liveRef.ReplaceAllStringFunc(s, func(m string) string {
		sub := liveRef.FindStringSubmatch(m)
		sha, ok := r.names[sub[2]]
		if !ok {
			t.Fatalf("fixture names no %q", sub[2])
		}
		if sub[1] != "" {
			return liveShort(sha)
		}
		return sha
	})
}

// liveRun builds f in git and runs rulegen liveness on it with extra
// arguments.
func liveRun(t *testing.T, f *liveFixture, extra ...string) *liveResult {
	t.Helper()
	return liveRunEdited(t, f, nil, extra...)
}

// liveRunEdited is liveRun with edit applied to the -prs file before the
// run. In extra, placeholders are expanded, and the pair "-upstream-status
// <line>" is not passed on but written to the clone's STATUS.
func liveRunEdited(t *testing.T, f *liveFixture, edit func(t *testing.T, prs string), extra ...string) *liveResult {
	t.Helper()
	status := data.StatusOK
	var pass []string
	for i := 0; i < len(extra); i++ {
		if extra[i] == "-upstream-status" && i+1 < len(extra) {
			status = extra[i+1]
			i++
			continue
		}
		pass = append(pass, extra[i])
	}
	e, stdout, stderr := newTestEnv(t, nil)
	if f.Now != "" {
		now, err := time.Parse(time.RFC3339, f.Now)
		if err != nil {
			t.Fatal(err)
		}
		e.Now = func() time.Time { return now }
	}
	r := &liveResult{names: map[string]string{}}

	u := gittest.NewUpstream(t, e.Git)
	for _, c := range f.Upstream {
		when, err := time.Parse(time.RFC3339, c.Date)
		if err != nil {
			t.Fatal(err)
		}
		parent := ""
		if c.Parent != "" {
			if parent = r.names[c.Parent]; parent == "" {
				t.Fatalf("upstream %s: parent %s not defined before it", c.Name, c.Parent)
			}
		}
		r.names[c.Name] = u.Commit(parent, map[string][]byte{"README": []byte(c.Name + "\n")}, when, c.Name)
	}
	u.SetRef("refs/heads/"+data.Branch, r.names[f.Master])
	clone := u.Clone(t, status)

	l := gittest.NewLinkclean(t, e.Git, gittest.Opts{})
	if f.Main != "" {
		l.SetGitlink(r.names[f.Main])
		l.Commit("main's gitlink")
		l.Push("main")
	}
	for name, g := range f.Heads {
		l.Run("checkout", "--quiet", "-b", "head-"+name, "main")
		if g == "" {
			l.Run("rm", "--cached", "--quiet", data.Submodule)
		} else {
			l.SetGitlink(r.names[g])
		}
		r.names[name] = l.Commit("head " + name)
		l.Run("checkout", "--quiet", "main")
	}

	// One value a line, as gh api --jq prints them, then the line the
	// command writes once every call succeeded.
	var stream strings.Builder
	for _, v := range f.PRs {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		stream.WriteString(r.expand(t, string(b)) + "\n")
	}
	stream.WriteString(liveDone + "\n")
	prs := filepath.Join(t.TempDir(), "prs.json")
	if err := os.WriteFile(prs, []byte(stream.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(t, prs)
	}
	args := []string{"liveness", "-root", l.Work.Dir, "-upstream", clone, "-prs", prs}
	for _, a := range pass {
		args = append(args, r.expand(t, a))
	}
	r.code = run(context.Background(), e, args)
	r.stdout, r.stderr = stdout.String(), stderr.String()
	return r
}

// liveCheck runs f and requires its verdict line and exit code.
func liveCheck(t *testing.T, f *liveFixture) *liveResult {
	t.Helper()
	r := liveRun(t, f)
	if want := r.expand(t, f.Want) + "\n"; r.stdout != want || r.code != f.Code {
		t.Errorf("verdict %q exit %d, want %q exit %d\nstderr:\n%s", r.stdout, r.code, want, f.Code, r.stderr)
	}
	return r
}

// liveAlert is the expected ALERT line naming G and M.
func liveAlert(g, m string) string {
	return "ALERT " + fmt.Sprintf(data.AlertNoPR, "{{short:"+g+"}}", "{{short:"+g+"}}..{{short:"+m+"}}") + liveCheckPage
}

// liveFixPR returns the PR object of #n.
func liveFixPR(t *testing.T, f *liveFixture, n int) map[string]any {
	t.Helper()
	for _, p := range f.PRs {
		if _, ok := p["head"]; ok && fmt.Sprint(p["number"]) == fmt.Sprint(n) {
			return p
		}
	}
	t.Fatalf("fixture has no #%d", n)
	return nil
}

// liveFixPage returns the first page of #n holding kind, "comments" or
// "commits".
func liveFixPage(t *testing.T, f *liveFixture, n int, kind string) map[string]any {
	t.Helper()
	for _, p := range f.PRs {
		if _, ok := p[kind]; ok && fmt.Sprint(p["pull_number"]) == fmt.Sprint(n) {
			return p
		}
	}
	t.Fatalf("fixture has no %s page of #%d", kind, n)
	return nil
}

// liveDropPR drops #n and its pages.
func liveDropPR(f *liveFixture, n int) {
	var keep []map[string]any
	for _, p := range f.PRs {
		if fmt.Sprint(p["number"]) != fmt.Sprint(n) && fmt.Sprint(p["pull_number"]) != fmt.Sprint(n) {
			keep = append(keep, p)
		}
	}
	f.PRs = keep
}

func liveSetDate(t *testing.T, f *liveFixture, name, date string) {
	t.Helper()
	for i := range f.Upstream {
		if f.Upstream[i].Name == name {
			f.Upstream[i].Date = date
			return
		}
	}
	t.Fatalf("fixture has no upstream %s", name)
}

// liveDependabotCommits drops every commit of a commits page not authored
// by Dependabot.
func liveDependabotCommits(page map[string]any) {
	var keep []any
	for _, c := range page["commits"].([]any) {
		if a, ok := c.(map[string]any)["author"].(map[string]any); ok && a["login"] == liveBotLogin {
			keep = append(keep, c)
		}
	}
	page["commits"] = keep
}

// TestLivenessFixtures runs the §11.8 rows the liveness command owns, each
// with a control that changes the one fact the row turns on, so a fixture
// passing for another reason shows.
func TestLivenessFixtures(t *testing.T) {
	t.Run("liveness_blocked_human_edited", func(t *testing.T) {
		t.Parallel()
		f := liveLoad(t, t.Name()[len("TestLivenessFixtures/"):])
		liveCheck(t, f)
		// Control: without the maintainer commit #5 is an unedited open PR,
		// G is its gitlink, and upstream moved past it.
		liveDependabotCommits(liveFixPage(t, f, 5, "commits"))
		f.Want, f.Code = liveAlert("G1", "M"), exitFail
		liveCheck(t, f)
	})
	t.Run("liveness_blocked_escalated", func(t *testing.T) {
		t.Parallel()
		f := liveLoad(t, t.Name()[len("TestLivenessFixtures/"):])
		r := liveCheck(t, f)
		if !strings.Contains(r.stderr, "open #5 carries non-Dependabot commits: 1d1e2f3a4b5c rules: brave/adblock-lists@") {
			t.Errorf("stderr does not name the sync commit:\n%s", r.stderr)
		}
		liveDependabotCommits(liveFixPage(t, f, 5, "commits"))
		f.Want, f.Code = liveAlert("G1", "M"), exitFail
		liveCheck(t, f)
	})
	t.Run("liveness_open_unedited_old", func(t *testing.T) {
		t.Parallel()
		f := liveLoad(t, t.Name()[len("TestLivenessFixtures/"):])
		liveCheck(t, f)
		// Control: G from main's gitlink instead.
		liveDropPR(f, 6)
		f.Want, f.Code = liveAlert("B", "G1"), exitFail
		liveCheck(t, f)
	})
	t.Run("liveness_cooldown_at_monday", func(t *testing.T) {
		t.Parallel()
		f := liveLoad(t, t.Name()[len("TestLivenessFixtures/"):])
		liveCheck(t, f)
		// Control: 3.04 days old at Monday 00:00 is past the cooldown.
		g := *f
		g.Upstream = append([]liveUpCommit(nil), f.Upstream...)
		liveSetDate(t, &g, "M", "2026-10-15T23:00:00Z")
		g.Want, g.Code = liveAlert("B", "M"), exitFail
		liveCheck(t, &g)
		// Boundary: exactly 3 days old at Monday 00:00 is no longer "less
		// than 3 days old", so Dependabot could have proposed it; one
		// second younger is still inside the cooldown.
		liveSetDate(t, &g, "M", "2026-10-16T00:00:00Z")
		liveCheck(t, &g)
		liveSetDate(t, &g, "M", "2026-10-16T00:00:01Z")
		g.Want, g.Code = "quiet (no change evidence)", exitOK
		liveCheck(t, &g)
		// Control: a week later the most recent Monday is 2026-10-26.
		f.Now = "2026-10-27T11:00:00Z"
		f.Want, f.Code = liveAlert("B", "M"), exitFail
		liveCheck(t, f)
	})
	t.Run("held_closed", func(t *testing.T) {
		t.Parallel()
		f := liveLoad(t, t.Name()[len("TestLivenessFixtures/"):])
		liveCheck(t, f)
		// Control: if the routine's close is not recognised, G is taken
		// from main and the alert fires.
		liveFixPage(t, f, 7, "comments")["comments"] = []any{}
		f.Want, f.Code = liveAlert("B", "G7"), exitFail
		liveCheck(t, f)
	})
	t.Run("takeover_merged_conflict_closes", func(t *testing.T) {
		t.Parallel()
		f := liveLoad(t, t.Name()[len("TestLivenessFixtures/"):])
		liveCheck(t, f)
		liveFixPage(t, f, 9, "comments")["comments"] = []any{}
		f.Want, f.Code = liveAlert("T", "G9"), exitFail
		liveCheck(t, f)
	})
	t.Run("spike_recorded", func(t *testing.T) {
		t.Parallel()
		name := t.Name()[len("TestLivenessFixtures/"):]
		liveCheck(t, liveLoad(t, name))
		// Control: inside the window #6, opened 2026-10-17, is enough.
		f := liveLoad(t, name)
		f.Now, f.Want, f.Code = "2026-10-20T11:00:00Z", "ok", exitOK
		liveCheck(t, f)
		// Control: reopened, #1's maintainer commit blocks the path.
		f = liveLoad(t, name)
		liveFixPR(t, f, 1)["state"] = "open"
		f.Want, f.Code = "blocked #1", exitOK
		if r := liveCheck(t, f); !strings.Contains(r.stderr, "open #1 carries non-Dependabot commits: 7e58eb00a8bf Sync vendored data") {
			t.Errorf("stderr does not name #1's maintainer commit:\n%s", r.stderr)
		}
		// Control: without #6, G is #2's gitlink, and upstream moved past it.
		f = liveLoad(t, name)
		liveDropPR(f, 6)
		f.Want, f.Code = liveAlert("G2", "G6"), exitFail
		liveCheck(t, f)
	})
	t.Run("takeover_merged_past_gitlink_closes", func(t *testing.T) {
		t.Parallel()
		f := liveLoad(t, t.Name()[len("TestLivenessFixtures/"):])
		r := liveCheck(t, f)
		if !strings.Contains(r.stderr, "#9 (newest routine-closed PR): main's gitlink is not an ancestor") {
			t.Errorf("the ancestor guard did not drop #9:\n%s", r.stderr)
		}
		// Control: once upstream moves past T by an old commit the alert
		// names T, not #9's older G9.
		f.Upstream = append(f.Upstream, liveUpCommit{Name: "N", Parent: "T", Date: "2026-10-01T09:00:00Z"})
		f.Master = "N"
		f.Want, f.Code = liveAlert("T", "N"), exitFail
		liveCheck(t, f)
	})
}

// livePull is a REST PR object labelled rules-update; state is OPEN,
// CLOSED (not merged) or MERGED, head a fixture head name.
func livePull(n int, state, created, login, ref, head string) map[string]any {
	rest, merged := "closed", any(nil)
	switch state {
	case "OPEN":
		rest = "open"
	case "MERGED":
		merged = created
	}
	return map[string]any{
		"number": n, "state": rest, "merged_at": merged, "created_at": created,
		"user": map[string]any{"login": login}, "head": map[string]any{"ref": ref, "sha": "{{" + head + "}}"},
		"labels": []any{map[string]any{"name": liveLabel}},
	}
}

// liveCommitPage is a pulls/<n>/commits element; login "" stands for a
// commit GitHub matched to no account (author null).
func liveCommitPage(sha, login, msg string) map[string]any {
	var author any
	if login != "" {
		author = map[string]any{"login": login}
	}
	return map[string]any{"sha": sha, "author": author, "commit": map[string]any{"message": msg}}
}

// liveDependabot returns a Dependabot submodule PR with head head as the
// documented command lists it: the PR object, a comments page and a
// commits page holding Dependabot's commit and one commit by each login.
func liveDependabot(n int, state, created, head string, commits ...string) []map[string]any {
	cs := []any{liveCommitPage(strings.Repeat("d", 40), liveBotLogin, "Bump tools/upstream/adblock-lists")}
	for _, login := range commits {
		cs = append(cs, liveCommitPage(strings.Repeat("e", 40), login, "an edit"))
	}
	return []map[string]any{
		livePull(n, state, created, liveBotLogin, "dependabot/submodules/tools/upstream/adblock-lists-abc1234", head),
		{"pull_number": n, "comments": []any{}},
		{"pull_number": n, "commits": cs},
	}
}

// liveTakeover is a PR a human opened; the command fetches no pages for it.
func liveTakeover(n int, state, created, head string) []map[string]any {
	return []map[string]any{livePull(n, state, created, "maintainer", "rules/takeover-0123456789ab", head)}
}

func liveComment(pr []map[string]any, body string) []map[string]any {
	for _, v := range pr {
		if cs, ok := v["comments"].([]any); ok {
			v["comments"] = append(cs, map[string]any{"user": map[string]any{"login": "maintainer"}, "body": body})
			return pr
		}
	}
	panic("liveComment: no comments page")
}

// liveLine is upstream B → G1 → G2 → M, all older than the cooldown; main's
// gitlink is B.
func liveLine(prs ...[]map[string]any) *liveFixture {
	return &liveFixture{
		Upstream: []liveUpCommit{
			{Name: "B", Date: "2026-08-01T00:00:00Z"},
			{Name: "G1", Parent: "B", Date: "2026-08-10T00:00:00Z"},
			{Name: "G2", Parent: "G1", Date: "2026-08-20T00:00:00Z"},
			{Name: "M", Parent: "G2", Date: "2026-09-01T00:00:00Z"},
		},
		Master: "M",
		Main:   "B",
		Heads:  map[string]string{"hB": "B", "h1": "G1", "h2": "G2", "hM": "M"},
		PRs:    slices.Concat(prs...),
	}
}

const liveOld = "2026-09-02T04:00:00Z" // before the 14-day window of the test clock

func TestLiveness(t *testing.T) {
	alertB := liveAlert("B", "M")
	tests := []struct {
		name string
		f    *liveFixture
		want string
		code int
		log  string // in stderr
	}{
		{"no_prs", liveLine(), alertB, exitFail, "G = "},
		{"window_start_inclusive", liveLine(liveDependabot(4, "MERGED", "2026-10-06T00:00:00Z", "h1")), "ok", exitOK, "#4 opened 2026-10-06T00:00:00Z"},
		{"window_start_exclusive", liveLine(liveDependabot(4, "MERGED", "2026-10-05T23:59:59Z", "h1")), alertB, exitFail, ""},
		{"window_closed_pr", liveLine(liveDependabot(4, "CLOSED", "2026-10-13T04:00:00Z", "h1")), "ok", exitOK, ""},
		{"window_ignores_takeover", liveLine(liveTakeover(4, "MERGED", "2026-10-13T04:00:00Z", "hB")), alertB, exitFail, ""},
		{"window_ignores_other_dependabot_branch", liveLine(func() []map[string]any {
			p := liveDependabot(4, "MERGED", "2026-10-13T04:00:00Z", "hB")
			p[0]["head"].(map[string]any)["ref"] = "dependabot/go_modules/golang.org/x/net-0.30.0"
			return p
		}()), alertB, exitFail, ""},
		{"window_ignores_unlabelled", liveLine(func() []map[string]any {
			p := liveDependabot(4, "MERGED", "2026-10-13T04:00:00Z", "hB")
			p[0]["labels"] = []any{map[string]any{"name": "dependencies"}}
			return p
		}()), alertB, exitFail, ""},
		{"blocked_newest", liveLine(
			liveDependabot(5, "OPEN", liveOld, "h1", "maintainer"),
			liveDependabot(6, "OPEN", liveOld, "h2", "maintainer"),
		), "blocked #6", exitOK, ""},
		{"blocked_commit_without_login", liveLine(liveDependabot(5, "OPEN", liveOld, "h1", "")), "blocked #5", exitOK, ""},
		{"blocked_co_author", liveLine(liveCoAuthored(5, "Someone <someone@example.invalid>")), "blocked #5", exitOK, "open #5 carries non-Dependabot commits: dddddddddddd Bump"},
		{"co_author_dependabot", liveLine(liveCoAuthored(5, "dependabot[bot] <"+liveBotEmail+">")), liveAlert("G1", "M"), exitFail, ""},
		{"co_author_not_trailer", liveLine(func() []map[string]any {
			p := liveDependabot(5, "OPEN", liveOld, "h1")
			p[2]["commits"].([]any)[0].(map[string]any)["commit"] = map[string]any{"message": "Bump\nCo-authored-by: Someone <someone@example.invalid>\n\nSigned-off-by: x"}
			return p
		}()), liveAlert("G1", "M"), exitFail, ""},
		{"blocked_null_author_dependabot_co_author", liveLine(func() []map[string]any {
			// GitHub matched the commit's own author to no account; a
			// Dependabot trailer does not make the commit Dependabot's.
			p := liveDependabot(5, "OPEN", liveOld, "h1", "")
			p[2]["commits"].([]any)[1].(map[string]any)["commit"] = map[string]any{"message": "an edit\n\nCo-authored-by: dependabot[bot] <" + liveBotEmail + ">\n"}
			return p
		}()), "blocked #5", exitOK, "open #5 carries non-Dependabot commits: eeeeeeeeeeee an edit\n"},
		{"co_author_dependabot_mixed_case", liveLine(liveCoAuthored(5, "dependabot[bot] <49699333+Dependabot[bot]@Users.NoReply.GitHub.com>")), liveAlert("G1", "M"), exitFail, ""},
		{"blocked_co_author_crlf", liveLine(func() []map[string]any {
			p := liveDependabot(5, "OPEN", liveOld, "h1")
			p[2]["commits"].([]any)[0].(map[string]any)["commit"] = map[string]any{"message": "Bump\r\n\r\nCo-authored-by: Someone <someone@example.invalid>\r\n"}
			return p
		}()), "blocked #5", exitOK, "open #5 carries non-Dependabot commits: dddddddddddd Bump\n"},
		{"co_author_subject_only", liveLine(func() []map[string]any {
			p := liveDependabot(5, "OPEN", liveOld, "h1")
			p[2]["commits"].([]any)[0].(map[string]any)["commit"] = map[string]any{"message": "Co-authored-by: Someone <someone@example.invalid>"}
			return p
		}()), liveAlert("G1", "M"), exitFail, ""},
		{"window_ignores_human_on_dependabot_branch", liveLine([]map[string]any{
			livePull(4, "MERGED", "2026-10-13T04:00:00Z", "maintainer", "dependabot/submodules/tools/upstream/adblock-lists-abc1234", "hM"),
		}), alertB, exitFail, ""},
		{"routine_close_of_human_pr_not_candidate", liveLine(liveComment([]map[string]any{
			livePull(7, "CLOSED", liveOld, "maintainer", "dependabot/submodules/tools/upstream/adblock-lists-abc1234", "hM"),
			{"pull_number": 7, "comments": []any{}},
			{"pull_number": 7, "commits": []any{}},
		}, data.CommentGone)), alertB, exitFail, ""},
		{"closed_edited_not_blocked", liveLine(liveDependabot(5, "CLOSED", liveOld, "h1", "maintainer")), alertB, exitFail, ""},
		{"open_dependabot_newest_gitlink", liveLine(
			liveDependabot(5, "OPEN", liveOld, "hM"),
			liveDependabot(6, "OPEN", liveOld, "h1"),
		), "quiet (no change evidence)", exitOK, "G = " + "{{short:M}} (#5, open Dependabot PR)"},
		{"open_dependabot_behind_main", func() *liveFixture {
			f := liveLine(liveDependabot(5, "OPEN", liveOld, "hB"))
			f.Main = "G1"
			return f
		}(), liveAlert("G1", "M"), exitFail, "#5 (open Dependabot PR): main's gitlink is not an ancestor"},
		{"open_takeover", liveLine(liveTakeover(8, "OPEN", liveOld, "hM")), "quiet (no change evidence)", exitOK, "#8, newest open takeover PR"},
		{"open_takeover_only_newest", liveLine(
			liveTakeover(8, "OPEN", liveOld, "hM"),
			liveTakeover(9, "OPEN", liveOld, "h1"),
		), liveAlert("G1", "M"), exitFail, ""},
		{"merged_takeover_ignored", liveLine(liveTakeover(8, "MERGED", liveOld, "hM")), alertB, exitFail, ""},
		{"routine_closed_gone", liveLine(liveComment(liveDependabot(7, "CLOSED", liveOld, "hM"), data.CommentGone)), "quiet (no change evidence)", exitOK, ""},
		{"routine_closed_leading_blank", liveLine(liveComment(liveDependabot(7, "CLOSED", liveOld, "hM"), "\n"+data.CommentRulesMoved)), "quiet (no change evidence)", exitOK, ""},
		{"routine_closed_only_newest", liveLine(
			liveComment(liveDependabot(7, "CLOSED", liveOld, "hM"), data.CommentGone),
			liveComment(liveDependabot(8, "CLOSED", liveOld, "h1"), data.CommentGone),
		), liveAlert("G1", "M"), exitFail, ""},
		{"routine_closed_behind_main", func() *liveFixture {
			f := liveLine(liveComment(liveDependabot(7, "CLOSED", liveOld, "hB"), data.CommentRulesMovedPast))
			f.Main = "G2"
			return f
		}(), liveAlert("G2", "M"), exitFail, "main's gitlink is not an ancestor"},
		{"superseded_close_not_routine_close", liveLine(liveComment(liveDependabot(7, "CLOSED", liveOld, "hM"), fmt.Sprintf(data.CommentSuperseded, 8))), alertB, exitFail, ""},
		{"quoted_close_comment_not_routine_close", liveLine(liveComment(liveDependabot(7, "CLOSED", liveOld, "hM"), "> "+data.CommentGone)), alertB, exitFail, ""},
		{"close_comment_on_merged_pr", liveLine(liveComment(liveDependabot(7, "MERGED", liveOld, "hM"), data.CommentGone)), alertB, exitFail, ""},
		{"candidate_off_master", func() *liveFixture {
			f := liveLine(liveDependabot(5, "OPEN", liveOld, "hX"))
			f.Upstream = append(f.Upstream, liveUpCommit{Name: "X", Parent: "M", Date: "2026-09-05T00:00:00Z"})
			f.Heads["hX"] = "X"
			return f
		}(), alertB, exitFail, "is not on upstream master; skipped"},
		{"main_off_master", func() *liveFixture {
			f := liveLine(liveDependabot(5, "OPEN", liveOld, "hM"))
			f.Upstream = append(f.Upstream, liveUpCommit{Name: "R", Date: "2026-07-01T00:00:00Z"})
			f.Main = "R"
			return f
		}(), liveAlert("R", "M"), exitFail, "G is not on upstream master"},
		{"head_without_gitlink", func() *liveFixture {
			f := liveLine(liveDependabot(5, "OPEN", liveOld, "hN"))
			f.Heads["hN"] = ""
			return f
		}(), alertB, exitFail, "has no gitlink; skipped"},
		{"young_tail_quiet", func() *liveFixture {
			f := liveLine(liveDependabot(5, "OPEN", liveOld, "hM"))
			f.Upstream = append(f.Upstream, liveUpCommit{Name: "Y", Parent: "M", Date: "2026-10-18T12:00:00Z"})
			f.Master = "Y"
			return f
		}(), "quiet (no change evidence)", exitOK, "all 1 commits in G..M were under 3 days old"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := tt.f
			f.Want, f.Code = tt.want, tt.code
			r := liveCheck(t, f)
			if tt.log != "" && !strings.Contains(r.stderr, r.expand(t, tt.log)) {
				t.Errorf("stderr lacks %q:\n%s", r.expand(t, tt.log), r.stderr)
			}
		})
	}
}

// liveCoAuthored is open Dependabot PR #n with head h1 whose only commit,
// authored by Dependabot, carries a Co-authored-by trailer for coAuthor.
func liveCoAuthored(n int, coAuthor string) []map[string]any {
	p := liveDependabot(n, "OPEN", liveOld, "h1")
	p[2]["commits"].([]any)[0].(map[string]any)["commit"] = map[string]any{"message": "Bump\n\nbody\n\nCo-authored-by: " + coAuthor + "\n"}
	return p
}

func TestLivenessErrors(t *testing.T) {
	noCommits := liveDependabot(5, "OPEN", liveOld, "h1")[:2]
	noComments := slices.Delete(liveDependabot(7, "CLOSED", liveOld, "h1"), 1, 2)
	noMergedAt := liveDependabot(5, "OPEN", liveOld, "h1")
	delete(noMergedAt[0], "merged_at")
	headSha := regexp.MustCompile(`("head":\{"ref":"[^"]*","sha":")[0-9a-f]{40}"`)
	tests := []struct {
		name  string
		f     *liveFixture
		edit  func(t *testing.T, prs string) // rewrites the -prs file
		extra []string
		code  int
		err   string
	}{
		{"head_not_fetched", liveLine(liveDependabot(5, "OPEN", liveOld, "h1")), func(t *testing.T, prs string) {
			b, _ := os.ReadFile(prs)
			b = []byte(headSha.ReplaceAllString(string(b), `${1}`+strings.Repeat("a", 40)+`"`))
			os.WriteFile(prs, b, 0o644)
		}, nil, exitUnverifiable, "is not in"},
		{"head_not_sha", liveLine(liveDependabot(5, "OPEN", liveOld, "h1")), func(t *testing.T, prs string) {
			b, _ := os.ReadFile(prs)
			b = []byte(headSha.ReplaceAllString(string(b), `${1}main"`))
			os.WriteFile(prs, b, 0o644)
		}, nil, exitUnverifiable, `head.sha "main" is not a commit id`},
		{"missing_commits_page", liveLine(noCommits), nil, nil, exitUnverifiable, "open #5 has no commits page; use the command of rulegen liveness -h"},
		{"missing_comments_page", liveLine(noComments), nil, nil, exitUnverifiable, "closed #7 has no comments page; use the command of rulegen liveness -h"},
		{"missing_key", liveLine(noMergedAt), nil, nil, exitUnverifiable, `value 1: PR object has no "merged_at"; use the command`},
		{"gh_pr_list_array", liveLine(), func(t *testing.T, prs string) {
			os.WriteFile(prs, []byte(`[{"number": 5, "state": "OPEN", "headRefOid": "main"}]`), 0o644)
		}, nil, exitUnverifiable, "value 1: not a JSON object"},
		{"unknown_value", liveLine(), func(t *testing.T, prs string) { os.WriteFile(prs, []byte(`{"total_count": 0}`), 0o644) }, nil, exitUnverifiable, "neither a PR object nor"},
		{"page_both_kinds", liveLine(), func(t *testing.T, prs string) {
			os.WriteFile(prs, []byte(`{"pull_number": 5, "comments": [], "commits": []}`), 0o644)
		}, nil, exitUnverifiable, `exactly one of "comments" and "commits"`},
		{"page_of_unlisted_pr", liveLine(), func(t *testing.T, prs string) {
			os.WriteFile(prs, []byte(`{"pull_number": 5, "comments": []}`+"\n"+liveDone), 0o644)
		}, nil, exitUnverifiable, "a page of #5, which is not listed"},
		// A listing that did not complete is never read as one with fewer
		// PRs: without its last line #4 would be missing and the window empty.
		{"listing_empty", liveLine(liveDependabot(4, "OPEN", "2026-10-13T04:00:00Z", "h1")), func(t *testing.T, prs string) {
			os.WriteFile(prs, nil, 0o644)
		}, nil, exitUnverifiable, "the listing did not complete"},
		{"listing_cut", liveLine(liveDependabot(4, "OPEN", "2026-10-13T04:00:00Z", "h1")), func(t *testing.T, prs string) {
			liveEditLines(t, prs, func(l []string) []string { return l[:len(l)-1] })
		}, nil, exitUnverifiable, "the listing did not complete"},
		{"listing_marker_first", liveLine(liveDependabot(4, "OPEN", "2026-10-13T04:00:00Z", "h1")), func(t *testing.T, prs string) {
			liveEditLines(t, prs, func(l []string) []string { return append(l[len(l)-1:], l[:len(l)-1]...) })
		}, nil, exitUnverifiable, "value 2: a value after " + liveDone},
		{"listing_marker_twice", liveLine(), func(t *testing.T, prs string) {
			liveEditLines(t, prs, func(l []string) []string { return append(l, liveDone) })
		}, nil, exitUnverifiable, "value 2: a value after " + liveDone},
		{"listing_marker_other", liveLine(), func(t *testing.T, prs string) {
			os.WriteFile(prs, []byte(`{"listing": "partial"}`), 0o644)
		}, nil, exitUnverifiable, "value 1: not " + liveDone},
		{"listing_marker_extra_key", liveLine(), func(t *testing.T, prs string) {
			os.WriteFile(prs, []byte(`{"listing": "complete", "number": 4}`), 0o644)
		}, nil, exitUnverifiable, "value 1: not " + liveDone},
		{"no_number", liveLine(func() []map[string]any {
			p := liveDependabot(5, "OPEN", liveOld, "h1")
			p[0]["number"] = 0
			return p
		}()), nil, nil, exitUnverifiable, "PR without a number"},
		{"null_author", liveLine(func() []map[string]any {
			p := liveDependabot(5, "OPEN", liveOld, "h1")
			p[0]["user"] = nil
			return p
		}()), nil, nil, exitUnverifiable, "#5: no author"},
		{"null_created_at", liveLine(func() []map[string]any {
			p := liveDependabot(5, "OPEN", liveOld, "h1")
			p[0]["created_at"] = nil
			return p
		}()), nil, nil, exitUnverifiable, "#5: no created_at"},
		{"not_json", liveLine(), func(t *testing.T, prs string) { os.WriteFile(prs, []byte("[{"), 0o644) }, nil, exitUnverifiable, "prs.json"},
		{"unknown_state", liveLine(func() []map[string]any {
			p := liveDependabot(5, "OPEN", liveOld, "h1")
			p[0]["state"] = "OPEN"
			return p
		}()), nil, nil, exitUnverifiable, `unknown state "OPEN"`},
		{"merged_but_open", liveLine(func() []map[string]any {
			p := liveDependabot(5, "OPEN", liveOld, "h1")
			p[0]["merged_at"] = liveOld
			return p
		}()), nil, nil, exitUnverifiable, `unknown state "open"`},
		{"duplicate_number", liveLine(liveDependabot(5, "OPEN", liveOld, "h1"), liveDependabot(5, "OPEN", liveOld, "h1")), nil, nil, exitUnverifiable, "#5 listed twice"},
		{"master_mismatch", liveLine(), nil, []string{"-master", strings.Repeat("b", 40)}, exitUnverifiable, "re-run rulegen upstream"},
		{"no_main_gitlink", func() *liveFixture { f := liveLine(); f.Main = ""; return f }(), nil, nil, exitUnverifiable, "has no gitlink at " + data.Submodule},
		{"bad_now", liveLine(), nil, []string{"-now", "tuesday"}, exitUsage, "-now"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := liveRunEdited(t, tt.f, tt.edit, tt.extra...)
			if r.code != tt.code || r.stdout != "" || !strings.Contains(r.stderr, tt.err) {
				t.Errorf("exit %d stdout %q stderr %q; want exit %d, no verdict, %q", r.code, r.stdout, r.stderr, tt.code, tt.err)
			}
		})
	}
	t.Run("upstream_unreachable", func(t *testing.T) {
		t.Parallel()
		r := liveRunEdited(t, liveLine(), nil, "-upstream-status", data.StatusUnreachablePrefix+"connection refused")
		if r.code != exitUnverifiable || !strings.Contains(r.stderr, "upstream clone: unreachable: connection refused") {
			t.Errorf("exit %d stderr %q", r.code, r.stderr)
		}
	})
	t.Run("master_matches", func(t *testing.T) {
		t.Parallel()
		f := liveLine()
		f.Want, f.Code = liveAlert("B", "M"), exitFail
		r := liveRunEdited(t, f, nil, "-master", "{{M}}")
		if r.stdout != r.expand(t, f.Want)+"\n" || r.code != exitFail {
			t.Errorf("verdict %q exit %d\n%s", r.stdout, r.code, r.stderr)
		}
	})
}

// liveEditLines rewrites the -prs file line by line.
func liveEditLines(t *testing.T, prs string, edit func([]string) []string) {
	t.Helper()
	b, err := os.ReadFile(prs)
	if err != nil {
		t.Fatal(err)
	}
	lines := edit(strings.Split(strings.TrimSuffix(string(b), "\n"), "\n"))
	if err := os.WriteFile(prs, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLivenessUsage(t *testing.T) {
	for _, tt := range []struct {
		args []string
		code int
		out  string
	}{
		{nil, exitUsage, "-root is required"},
		{[]string{"-root", "r", "-upstream", "u"}, exitUsage, "-prs is required"},
		{[]string{"-h"}, exitOK, `gh api --paginate "$R/pulls?state=all&sort=created&direction=asc&per_page=100"`},
		{[]string{"-root", "r", "-upstream", "u", "-prs", "p", "x"}, exitUsage, "unexpected arguments"},
	} {
		e, stdout, stderr := newTestEnv(t, nil)
		if code := run(context.Background(), e, append([]string{"liveness"}, tt.args...)); code != tt.code || stdout.Len() > 0 || !strings.Contains(stderr.String(), tt.out) {
			t.Errorf("%q: exit %d stdout %q stderr %q; want exit %d and %q", tt.args, code, stdout, stderr, tt.code, tt.out)
		}
	}
	// The help gives the producing command and every close prefix.
	e, _, stderr := newTestEnv(t, nil)
	run(context.Background(), e, []string{"liveness", "-h"})
	for _, s := range append([]string{liveIndent(liveProduce, "        ")}, liveClosePrefixes...) {
		if !strings.Contains(stderr.String(), s) {
			t.Errorf("help lacks %q", s)
		}
	}
}

func TestLivenessClosePrefixes(t *testing.T) {
	want := []string{data.CommentGone, data.CommentRulesMoved, data.CommentRulesMovedPast, "upstream change held: "}
	if strings.Join(liveClosePrefixes, "\n") != strings.Join(want, "\n") {
		t.Errorf("liveClosePrefixes = %q", liveClosePrefixes)
	}
	held := fmt.Sprintf(data.CommentHeld, "a reason", "2026-12-01")
	if p := (livePR{State: "CLOSED", Comments: []string{held}}); !p.closedByRoutine() {
		t.Errorf("held close comment %q not recognised", held)
	}
}

func TestLiveMonday(t *testing.T) {
	for _, tt := range []struct{ now, want string }{
		{"2026-10-20T11:00:00Z", "2026-10-19T00:00:00Z"}, // Tuesday run
		{"2026-10-19T00:00:00Z", "2026-10-19T00:00:00Z"}, // Monday midnight
		{"2026-10-19T23:59:59Z", "2026-10-19T00:00:00Z"},
		{"2026-10-18T23:59:59Z", "2026-10-12T00:00:00Z"},      // Sunday
		{"2026-10-20T01:00:00+05:00", "2026-10-19T00:00:00Z"}, // still Monday in UTC
	} {
		now, _ := time.Parse(time.RFC3339, tt.now)
		if got := liveMonday(now).Format(time.RFC3339); got != tt.want {
			t.Errorf("liveMonday(%s) = %s, want %s", tt.now, got, tt.want)
		}
	}
}

// TestLivenessProduceCommand keeps the documented -prs command REST-only
// (the routine has no GraphQL, which gh's PR, issue and label subcommands
// need), complete (every call paginates) and read-only (gh api sends POST
// once a field flag is given), its filters equal to liveness' own, and the
// help's copy equal to ROUTINE.md's.
func TestLivenessProduceCommand(t *testing.T) {
	routine, err := os.ReadFile("ROUTINE.md")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(routine), "\n"+liveProduce+"\n"); n != 1 {
		t.Errorf("ROUTINE.md holds the -prs command %d times, want once as its own lines:\n%s", n, liveProduce)
	}
	e, _, stderr := newTestEnv(t, nil)
	run(context.Background(), e, []string{"liveness", "-h"})
	if !strings.Contains(stderr.String(), liveIndent(liveProduce, "        ")) {
		t.Errorf("the help lacks the -prs command")
	}

	if !strings.Contains(liveProduce, "R=repos/Espresso-Aficionados/linkclean;") {
		t.Errorf("the command does not name the repository")
	}
	for _, s := range []string{`.name == "` + liveLabel + `"`, `startswith("` + liveHeadPrefix + `")`, `.merged_at == null`} {
		if !strings.Contains(liveProduce, s) {
			t.Errorf("the command's filters lack %s", s)
		}
	}
	// Oldest first, so a PR opened during the listing shifts no page.
	if !strings.Contains(liveProduce, `"$R/pulls?state=all&sort=created&direction=asc&per_page=100"`) {
		t.Errorf("the PR listing is not oldest first")
	}
	// The completion line comes last, inside the subshell that every failed
	// call leaves.
	if want := "  done\n  echo '" + liveDone + "' >> \"$F\"\n) || echo \"listing PRs failed\""; !strings.HasSuffix(liveProduce, want) {
		t.Errorf("the command does not end with %q", want)
	}
	joined := strings.ReplaceAll(liveProduce, "\\\n", " ")
	ghCall := regexp.MustCompile(`\bgh\s+(.*)`)
	write := regexp.MustCompile(`(^|\s)(-X|--method|-f|-F|--field|--raw-field|--input)(\s|=|$)`)
	calls := ghCall.FindAllStringSubmatch(joined, -1)
	if len(calls) != 3 {
		t.Errorf("%d gh calls, want 3", len(calls))
	}
	for _, m := range calls {
		switch {
		case !strings.HasPrefix(m[1], "api --paginate \"$R/"):
			t.Errorf("not a paginated REST call on the repository: gh %s", m[1])
		case write.MatchString(m[1]):
			t.Errorf("not a GET: gh %s", m[1])
		case strings.Contains(strings.ToLower(m[1]), "graphql"):
			t.Errorf("GraphQL: gh %s", m[1])
		}
	}
}
