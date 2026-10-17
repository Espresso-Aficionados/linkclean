package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/gittest"
)

// liveFixture is one liveness scenario (testdata/liveness/*.json): an
// upstream history, main's gitlink, PR heads by gitlink, and the gh pr list
// JSON. "{{name}}" in the PR JSON becomes the full id of upstream commit or
// head name; "{{short:name}}" in want its 12-hex short form.
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

	b, err := json.Marshal(f.PRs)
	if err != nil {
		t.Fatal(err)
	}
	prs := filepath.Join(t.TempDir(), "prs.json")
	if err := os.WriteFile(prs, []byte(r.expand(t, string(b))), 0o644); err != nil {
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

func liveFixPR(t *testing.T, f *liveFixture, n int) map[string]any {
	t.Helper()
	for _, p := range f.PRs {
		if p["number"] == float64(n) {
			return p
		}
	}
	t.Fatalf("fixture has no #%d", n)
	return nil
}

func liveDropPR(f *liveFixture, n int) {
	var keep []map[string]any
	for _, p := range f.PRs {
		if p["number"] != float64(n) {
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

// liveDependabotCommits drops every commit of p not authored by Dependabot.
func liveDependabotCommits(p map[string]any) {
	var keep []any
	for _, c := range p["commits"].([]any) {
		if strings.Contains(fmt.Sprint(c), liveCommitLogin) {
			keep = append(keep, c)
		}
	}
	p["commits"] = keep
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
		liveDependabotCommits(liveFixPR(t, f, 5))
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
		liveDependabotCommits(liveFixPR(t, f, 5))
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
		liveFixPR(t, f, 7)["comments"] = []any{}
		f.Want, f.Code = liveAlert("B", "G7"), exitFail
		liveCheck(t, f)
	})
	t.Run("takeover_merged_conflict_closes", func(t *testing.T) {
		t.Parallel()
		f := liveLoad(t, t.Name()[len("TestLivenessFixtures/"):])
		liveCheck(t, f)
		liveFixPR(t, f, 9)["comments"] = []any{}
		f.Want, f.Code = liveAlert("T", "G9"), exitFail
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

// liveDependabot returns a Dependabot submodule PR with head head.
func liveDependabot(n int, state, created, head string, commits ...string) map[string]any {
	cs := []any{map[string]any{
		"authors":         []any{map[string]any{"login": liveCommitLogin}},
		"messageHeadline": "Bump tools/upstream/adblock-lists",
		"oid":             strings.Repeat("d", 40),
	}}
	for _, login := range commits {
		cs = append(cs, map[string]any{
			"authors":         []any{map[string]any{"login": login}},
			"messageHeadline": "an edit",
			"oid":             strings.Repeat("e", 40),
		})
	}
	return map[string]any{
		"number": n, "state": state, "author": map[string]any{"login": "app/dependabot", "is_bot": true},
		"headRefName": "dependabot/submodules/tools/upstream/adblock-lists-abc1234", "headRefOid": "{{" + head + "}}",
		"createdAt": created, "closedAt": nil, "comments": []any{}, "commits": cs,
	}
}

func liveTakeover(n int, state, created, head string) map[string]any {
	p := liveDependabot(n, state, created, head, "maintainer")
	p["author"] = map[string]any{"login": "maintainer"}
	p["headRefName"] = "rules/takeover-0123456789ab"
	return p
}

func liveComment(p map[string]any, body string) map[string]any {
	p["comments"] = append(p["comments"].([]any), map[string]any{"author": map[string]any{"login": "maintainer"}, "body": body})
	return p
}

// liveLine is upstream B → G1 → G2 → M, all older than the cooldown; main's
// gitlink is B.
func liveLine(prs ...map[string]any) *liveFixture {
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
		PRs:    append([]map[string]any{}, prs...),
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
		{"window_ignores_other_dependabot_branch", liveLine(func() map[string]any {
			p := liveDependabot(4, "MERGED", "2026-10-13T04:00:00Z", "hB")
			p["headRefName"] = "dependabot/go_modules/golang.org/x/net-0.30.0"
			return p
		}()), alertB, exitFail, ""},
		{"blocked_newest", liveLine(
			liveDependabot(5, "OPEN", liveOld, "h1", "maintainer"),
			liveDependabot(6, "OPEN", liveOld, "h2", "maintainer"),
		), "blocked #6", exitOK, ""},
		{"blocked_commit_without_login", liveLine(liveDependabot(5, "OPEN", liveOld, "h1", "")), "blocked #5", exitOK, ""},
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

func TestLivenessErrors(t *testing.T) {
	missingField := liveDependabot(5, "OPEN", liveOld, "h1")
	delete(missingField, "commits")
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
			b = []byte(regexp.MustCompile(`"headRefOid":"[0-9a-f]{40}"`).ReplaceAllString(string(b), `"headRefOid":"`+strings.Repeat("a", 40)+`"`))
			os.WriteFile(prs, b, 0o644)
		}, nil, exitUnverifiable, "is not in"},
		{"head_not_sha", liveLine(liveDependabot(5, "OPEN", liveOld, "h1")), func(t *testing.T, prs string) {
			b, _ := os.ReadFile(prs)
			b = []byte(regexp.MustCompile(`"headRefOid":"[0-9a-f]{40}"`).ReplaceAllString(string(b), `"headRefOid":"main"`))
			os.WriteFile(prs, b, 0o644)
		}, nil, exitUnverifiable, `headRefOid "main" is not a commit id`},
		{"missing_field", liveLine(missingField), nil, nil, exitUnverifiable, `has no "commits"; use the gh command`},
		{"not_json", liveLine(), func(t *testing.T, prs string) { os.WriteFile(prs, []byte("[{"), 0o644) }, nil, exitUnverifiable, "prs.json"},
		{"unknown_state", liveLine(func() map[string]any {
			p := liveDependabot(5, "OPEN", liveOld, "h1")
			p["state"] = "open"
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

func TestLivenessUsage(t *testing.T) {
	for _, tt := range []struct {
		args []string
		code int
		out  string
	}{
		{nil, exitUsage, "-root is required"},
		{[]string{"-root", "r", "-upstream", "u"}, exitUsage, "-prs is required"},
		{[]string{"-h"}, exitOK, "gh pr list --repo Espresso-Aficionados/linkclean --label rules-update"},
		{[]string{"-root", "r", "-upstream", "u", "-prs", "p", "x"}, exitUsage, "unexpected arguments"},
	} {
		e, stdout, stderr := newTestEnv(t, nil)
		if code := run(context.Background(), e, append([]string{"liveness"}, tt.args...)); code != tt.code || stdout.Len() > 0 || !strings.Contains(stderr.String(), tt.out) {
			t.Errorf("%q: exit %d stdout %q stderr %q; want exit %d and %q", tt.args, code, stdout, stderr, tt.code, tt.out)
		}
	}
	// The help names every field liveness requires and every close prefix.
	e, _, stderr := newTestEnv(t, nil)
	run(context.Background(), e, []string{"liveness", "-h"})
	for _, s := range append([]string{"--json " + strings.Join(liveFields, ",")}, liveClosePrefixes...) {
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
	if p := (livePR{State: "CLOSED", Comments: []struct {
		Body string `json:"body"`
	}{{Body: held}}}); !p.closedByRoutine() {
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

// TestLivenessListCommand keeps the documented gh pr list within GitHub's
// GraphQL budget: a query that may return more than 500,000 nodes is
// refused outright, which would leave liveness nothing to read every run.
// gh asks for up to 100 comments and 100 commits per PR and 100 authors per
// commit, so each listed PR costs 1 + 100 + 100 + 100*100 nodes. The help
// and ROUTINE.md must give the same command.
func TestLivenessListCommand(t *testing.T) {
	const budget, perPR = 500_000, 1 + 100 + 100 + 100*100
	limitRe := regexp.MustCompile(`--label rules-update(?:\s|\\)+--state all --limit (\d+)(?:\s|\\)+--json (\S+)`)
	e, _, stderr := newTestEnv(t, nil)
	run(context.Background(), e, []string{"liveness", "-h"})
	routine, err := os.ReadFile("ROUTINE.md")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for name, text := range map[string]string{"help": stderr.String(), "ROUTINE.md": string(routine)} {
		ms := limitRe.FindAllStringSubmatch(text, -1)
		if len(ms) != 1 {
			t.Errorf("%s: %d step-1 gh pr list commands, want 1", name, len(ms))
			continue
		}
		var limit int
		fmt.Sscan(ms[0][1], &limit)
		if limit*perPR > budget {
			t.Errorf("%s: --limit %d asks GitHub for up to %d nodes, over its %d limit", name, limit, limit*perPR, budget)
		}
		if ms[0][2] != strings.Join(liveFields, ",") {
			t.Errorf("%s: --json %s, want %s", name, ms[0][2], strings.Join(liveFields, ","))
		}
		got = append(got, ms[0][1]+" "+ms[0][2])
	}
	if len(got) == 2 && got[0] != got[1] {
		t.Errorf("help and ROUTINE.md disagree: %q", got)
	}
}
