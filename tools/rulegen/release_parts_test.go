package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/forge"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/forge/forgetest"
)

func TestReleaseUsage(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		code int
		msg  string
	}{
		{"no_root", []string{"-pr", "1"}, exitUsage, "-root is required"},
		{"no_mode", []string{"-root", "x"}, exitUsage, "-pr <n> (data release) or -manual"},
		{"both", []string{"-root", "x", "-pr", "1", "-manual", "-bump", "minor"}, exitUsage, "mutually exclusive"},
		{"manual_no_bump", []string{"-root", "x", "-manual"}, exitUsage, "-manual needs -bump"},
		{"manual_major", []string{"-root", "x", "-manual", "-bump", "major"}, exitUsage, "-manual needs -bump"},
		{"data_bump", []string{"-root", "x", "-pr", "1", "-bump", "minor"}, exitUsage, "a data release is always a patch"},
		{"bad_repo", []string{"-root", "x", "-pr", "1", "-repo", "x/../y"}, exitUsage, "is not owner/name"},
		{"no_token", []string{"-root", "x", "-pr", "1"}, exitFail, relMsgNoGHToken},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, stdout, stderr := newTestEnv(t, nil) // no GH_TOKEN; HTTP refuses everything
			if code := run(context.Background(), e, append([]string{"release"}, tt.args...)); code != tt.code {
				t.Errorf("exit %d, want %d; stderr %q", code, tt.code, stderr)
			}
			if !strings.Contains(stderr.String(), tt.msg) || stdout.Len() > 0 {
				t.Errorf("stderr %q, want %q; stdout %q", stderr, tt.msg, stdout)
			}
		})
	}
}

func TestReleaseStaticClass(t *testing.T) {
	for p, want := range map[string]relClass{
		"rules/brave/clean-urls.json":                   relData,
		"rules/brave/NOTICE":                            relData,
		"rules.lock.json":                               relData,
		"rules/overlay/debounce-classes.json":           relData, // only after its diff is checked
		"rules/overlay/overlay.json":                    relOther,
		"rules/overlay/debounce-classes.json.license":   relOther,
		"rules/overlay/README.md":                       relOther,
		"rules/acks.json":                               relInert,
		"rules/other.json":                              relOther,
		".github/workflows/ci.yml":                      relInert,
		"tools/upstream/adblock-lists":                  relInert,
		"tools/rulegen/main.go":                         relInert,
		".gitmodules":                                   relInert,
		"testdata/corpus/eval-2026-10/known-broken.txt": relInert,
		"testdata/semantics/x.txtar":                    relInert,
		"discordlink/testdata/find.txtar":               relInert,
		"discordlink/testdata/fuzz/FuzzFind/0123abcd":   relInert,
		"unshorten/testdata/responses/t.co/x.txtar":     relInert, // recorded evidence, not expectations
		"unshorten/testdata/livenet/results.md.tmpl":    relInert,
		"unshorten/testdata/fuzz/FuzzResolve/0123abcd":  relInert,
		"unshorten/nettest_test.go":                     relInert,
		"unshorten/resolve.go":                          relOther,
		"testdata/golden/network.txtar":                 relOther,
		"internal/x/testdata/golden/positive.txtar":     relInert, // golden tracking is root-only
		"discordlink/testdata":                          relOther, // a file, not a directory
		"discordlink/find.go":                           relOther,
		"testdata/golden/positive.txtar":                relOther,
		"testdata/golden/README.md":                     relOther,
		"semantics_test.go":                             relInert,
		"internal/query/query_test.go":                  relInert,
		"README.md":                                     relInert,
		"NOTICE.md":                                     relInert,
		"CHANGELOG.md":                                  relOther,
		"docs/CHANGELOG.md":                             relOther,
		"README.MD":                                     relOther,
		"go.mod":                                        relDeps,
		"go.sum":                                        relDeps,
		"internal/x/go.mod":                             relOther,
		"doc.go":                                        relOther,
		"LICENSE":                                       relOther,
		".gitattributes":                                relOther,
		".gitignore":                                    relOther,
	} {
		if got := relStaticClass(p); got != want {
			t.Errorf("%s: %v, want %v", p, got, want)
		}
	}
	for p, want := range map[string]bool{
		"doc.go": true, "internal/rawurl/rawurl.go": true, "rules/overlay/overlay.json": true,
		"testdata/golden/negative.txt": true, "CHANGELOG.md": false, "LICENSE": false, "x_test.go": false,
		"testdata/golden/network.txtar": true, "unshorten/resolve.go": true,
		"unshorten/testdata/responses/t.co/x.txtar": false, "unshorten/nettest_test.go": false,
	} {
		if got := relMinorPath(relPath{Path: p, Class: relStaticClass(p)}); got != want {
			t.Errorf("relMinorPath(%s) = %v", p, got)
		}
	}
}

func TestReleaseGoModDiff(t *testing.T) {
	const base = "module m\n\ngo 1.26.0\n\nrequire (\n\tgolang.org/x/net v0.59.0\n\tgolang.org/x/text v0.42.0 // indirect\n)\n"
	for _, tt := range []struct {
		name, new string
		deps      []string
		goChanged bool
		note      string
	}{
		{"same", base, nil, false, ""},
		{"bump", strings.Replace(base, "v0.59.0", "v0.60.0", 1), []string{"golang.org/x/net v0.59.0 → v0.60.0"}, false, ""},
		{"bump_reformatted", "module m\ngo 1.26.0\n// deps\nrequire golang.org/x/text v0.43.0 // indirect\nrequire \"golang.org/x/net\" v0.59.0\n",
			[]string{"golang.org/x/text v0.42.0 → v0.43.0"}, false, ""},
		{"go", strings.Replace(base, "go 1.26.0", "go 1.27.0", 1), nil, true, "go or toolchain directive changed"},
		{"toolchain", base + "toolchain go1.27.1\n", nil, true, "go or toolchain directive changed"},
		{"go_and_bump", strings.Replace(strings.Replace(base, "go 1.26.0", "go 1.27", 1), "v0.59.0", "v0.60.0", 1),
			[]string{"golang.org/x/net v0.59.0 → v0.60.0"}, true, "go or toolchain directive changed"},
		{"added", strings.Replace(base, ")\n", "\tgolang.org/x/sync v0.10.0\n)\n", 1), nil, false, "required modules added or removed"},
		{"removed", strings.Replace(base, "\tgolang.org/x/text v0.42.0 // indirect\n", "", 1), nil, false, "required modules added or removed"},
		{"replace", base + "replace golang.org/x/net => ../net\n", nil, false, "changes other than require versions"},
		{"exclude", base + "exclude golang.org/x/net v0.58.0\n", nil, false, "changes other than require versions"},
		{"module", strings.Replace(base, "module m", "module m2", 1), nil, false, "changes other than require versions"},
		{"indirect_flip", strings.Replace(base, " // indirect", "", 1), nil, false, "changes other than require versions"},
		{"unterminated", "module m\nrequire (\n", nil, false, "unparsable: go.mod: unterminated require block"},
		{"duplicate", base + "require golang.org/x/net v0.60.0\n", nil, false, "unparsable: go.mod:9: golang.org/x/net required twice"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := relGoModDiff([]byte(base), true, []byte(tt.new), true)
			if !slices.Equal(d.deps, tt.deps) || d.goChanged != tt.goChanged || d.note != tt.note {
				t.Errorf("got deps %q go %v note %q; want %q %v %q", d.deps, d.goChanged, d.note, tt.deps, tt.goChanged, tt.note)
			}
		})
	}
	if d := relGoModDiff(nil, false, []byte(base), true); d.note != "added" {
		t.Errorf("added go.mod: %+v", d)
	}
	if d := relGoModDiff([]byte(base), true, nil, false); d.note != "deleted" {
		t.Errorf("deleted go.mod: %+v", d)
	}
}

func TestReleaseClassesDiff(t *testing.T) {
	deb := func(pats ...string) []byte {
		b, _ := json.Marshal([]map[string]any{{"include": pats, "action": "redirect", "param": "url"}})
		return b
	}
	cls := func(entries ...string) []byte {
		return []byte(`{"schema": 1, "patterns": {` + strings.Join(entries, ", ") + `}}`)
	}
	a := `"a": {"class": "unwrap"}`
	b := `"b": {"class": "affiliate", "network": "N", "idParam": "id"}`
	bLoc := `"b": {"class": "affiliate", "network": "N", "idParam": "uid"}`
	c := `"c": {"class": "off", "why": "x"}`
	for _, tt := range []struct {
		name           string
		oldC, newC     []byte
		oldDeb, newDeb []byte
		added          []relEntry
		removed        []string
		note           string
	}{
		{"add_new", cls(a), cls(a, b), deb("a"), deb("a", "b"), []relEntry{{"b", "affiliate"}}, nil, ""},
		{"remove_gone", cls(a, c), cls(a), deb("a", "c"), deb("a"), nil, []string{"c"}, ""},
		{"add_and_remove", cls(a, c), cls(a, b), deb("a", "c"), deb("a", "b"), []relEntry{{"b", "affiliate"}}, []string{"c"}, ""},
		{"add_not_new", cls(a), cls(a, b), deb("a", "b"), deb("a", "b"), nil, nil, `adds entry "b" for a pattern not new in rules/brave/debounce.json`},
		{"add_not_upstream", cls(a), cls(a, b), deb("a"), deb("a"), nil, nil, `adds entry "b" for a pattern not new in rules/brave/debounce.json`},
		{"remove_still_upstream", cls(a, c), cls(a), deb("a", "c"), deb("a", "c"), nil, nil, `removes entry "c" for a pattern still in rules/brave/debounce.json`},
		{"change", cls(a, b), cls(a, bLoc), deb("a", "b"), deb("a", "b"), nil, nil, `changes existing entry "b"`},
		{"mixed", cls(a, b), cls(a, bLoc, c), deb("a", "b"), deb("a", "b", "c"), nil, nil, `changes existing entry "b"`},
		{"reformat", cls(a, b), []byte("{\"schema\":1,\"patterns\":{\n" + b + ",\n" + a + "}}"), deb("a", "b"), deb("a", "b"), nil, nil, "reformatted without adding or removing entries"},
		{"top_level", cls(a), []byte(`{"schema": 2, "patterns": {` + a + `, ` + b + `}}`), deb("a"), deb("a", "b"), nil, nil, "changes fields other than patterns"},
		{"malformed", cls(a), []byte(`{"schema": 1, "patterns": {`), deb("a"), deb("a"), nil, nil, "unparsable: unexpected end of JSON input"},
		{"no_patterns", cls(a), []byte(`{"schema": 1}`), deb("a"), deb("a"), nil, nil, "unparsable: no patterns object"},
		{"debounce_broken", cls(a), cls(a, b), deb("a"), []byte("[{"), nil, nil, "debounce list unreadable: unexpected end of JSON input"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := relClassesDiff(tt.oldC, true, tt.newC, true, tt.oldDeb, true, tt.newDeb, true)
			if !slices.Equal(d.added, tt.added) || !slices.Equal(d.removed, tt.removed) || d.note != tt.note {
				t.Errorf("got added %v removed %v note %q; want %v %v %q", d.added, d.removed, d.note, tt.added, tt.removed, tt.note)
			}
		})
	}
	if d := relClassesDiff(nil, false, cls(a), true, nil, false, deb("a"), true); d.note != "added" {
		t.Errorf("added file: %+v", d)
	}
	if d := relClassesDiff(cls(a), true, nil, false, deb("a"), true, deb("a"), true); d.note != "deleted" {
		t.Errorf("deleted file: %+v", d)
	}
}

func TestReleaseVersion(t *testing.T) {
	for s, want := range map[string]bool{
		"v0.1.0": true, "v10.20.30": true, "v0.0.0": true,
		"0.1.0": false, "v0.1": false, "v0.1.0.1": false, "v01.1.0": false, "v0.01.0": false,
		"v0.1.0-rc1": false, "v0.1.0+meta": false, "v-1.0.0": false, "v0..0": false, "V0.1.0": false, "v1.0.0x": false,
	} {
		if _, ok := relParseVersion(s); ok != want {
			t.Errorf("relParseVersion(%q) ok = %v", s, ok)
		}
	}
	v, _ := relParseVersion("v0.3.9")
	if v.next(relPatch).String() != "v0.3.10" || v.next(relMinor).String() != "v0.4.0" {
		t.Errorf("next: %v %v", v.next(relPatch), v.next(relMinor))
	}
	if (relVersion{}).next(relMinor).String() != "v0.1.0" {
		t.Error("first minor is not v0.1.0")
	}
	if b, ok := relParseBump("minor"); !ok || b != relMinor || relMinor <= relPatch {
		t.Error("bump order")
	}
}

// relLatestTag takes the highest strict tag reachable from the commit, by
// version, not by date or name.
func TestReleaseLatestTag(t *testing.T) {
	f := relNew(t, "release_data_only")
	ctx := context.Background()
	if _, ok, err := relLatestTag(ctx, f.l.Work, "HEAD"); ok || err != nil {
		t.Fatalf("no tags: ok %v, %v", ok, err)
	}
	f.tag("v0.10.0")
	f.tag("v0.9.0")
	f.tag("v1.0.0-rc1")
	f.tag("v2.0")
	f.l.Run("tag", "unrelated")
	side := f.l.Run("commit-tree", "-m", "side", f.l.Run("rev-parse", "HEAD^{tree}"))
	f.l.Run("tag", "v9.9.9", side) // not reachable from HEAD
	v, ok, err := relLatestTag(ctx, f.l.Work, "HEAD")
	if !ok || err != nil || v.String() != "v0.10.0" {
		t.Errorf("latest = %v, %v, %v", v, ok, err)
	}
}

func TestReleaseChangelogSection(t *testing.T) {
	const cl = "# Changelog\n\n## v0.4.0 (2026-10-20)\n\n- A.\n- B.\n\n## v0.3.10\n\n- C.\n\n## v0.3.1\n\n## v0.3.0\n- D.\n"
	for _, tt := range []struct {
		v    string
		body string
		ok   bool
	}{
		{"v0.4.0", "- A.\n- B.", true},
		{"v0.3.10", "- C.", true},
		{"v0.3.1", "", true}, // a heading prefix of v0.3.10 is not v0.3.1's
		{"v0.3.0", "- D.", true},
		{"v0.5.0", "", false},
	} {
		v, _ := relParseVersion(tt.v)
		if body, ok := relChangelogSection([]byte(cl), v); body != tt.body || ok != tt.ok {
			t.Errorf("%s: %q %v, want %q %v", tt.v, body, ok, tt.body, tt.ok)
		}
	}
}

func TestReleaseRowTokens(t *testing.T) {
	const text = "Golden rows 12, 28b and E1 (`ov-amazon-tag`); see #7. v0.1.0 and 3-4."
	for id, want := range map[string]bool{
		"12": true, "28b": true, "E1": true, "ov-amazon-tag": true, "7": true,
		"1": false, "2": false, "28": false, "0": false, "3": false, "amazon": false,
	} {
		if got := relListsToken(text, id); got != want {
			t.Errorf("relListsToken(%q) = %v", id, got)
		}
	}
	oldP := "head\n-- 1 --\nin: a\n-- 2 --\nin: b\n-- 3 --\nin: c\n"
	newP := "head changed\n-- 1 --\nin: a\n-- 2 --\nin: B\n-- 4 --\nin: d\n"
	if got := relChangedRows("testdata/golden/positive.txtar", []byte(oldP), []byte(newP)); !slices.Equal(got, []string{"2", "4", "3"}) {
		t.Errorf("positive rows %q", got)
	}
	oldN := "# c\n25 https://a/\n26 https://b/\n"
	newN := "# changed comment\n25 https://a/\n26 https://B/\n27 https://c/\n"
	if got := relChangedRows("testdata/golden/negative.txt", []byte(oldN), []byte(newN)); !slices.Equal(got, []string{"26", "27"}) {
		t.Errorf("negative rows %q", got)
	}
	if got := relChangedRows("testdata/golden/risky-params.txt", []byte("a\n"), []byte("b\n")); len(got) != 0 {
		t.Errorf("risky-params rows %q", got)
	}
	// network.txtar's rows are -- N1 -- sections like positive.txtar's. A
	// range such as "N1-N3" lists none of its rows, so a CHANGELOG writes
	// each row out.
	newNet := "head\n-- N1 --\nin: a\n-- N2 --\nin: b\n"
	if got := relChangedRows("testdata/golden/network.txtar", nil, []byte(newNet)); !slices.Equal(got, []string{"N1", "N2"}) {
		t.Errorf("network rows %q", got)
	}
	for text, want := range map[string][]string{
		"Rows N1, N2, N3.": nil,
		"Rows N1-N3.":      {"N1", "N2", "N3"},
		"Rows N1\u2013N3.": {"N1", "N2", "N3"},
	} {
		if got := relUnlisted(text, []string{"N1", "N2", "N3"}); !slices.Equal(got, want) {
			t.Errorf("relUnlisted(%q) = %q, want %q", text, got, want)
		}
	}
}

// A minor release must list every golden row changed since the last tag
// in its CHANGELOG section.
func TestReleaseManualGoldenRows(t *testing.T) {
	f := relNew(t, "release_manual_minor")
	f.files["want-unlisted"] = "exit 1\nrefuse CHANGELOG.md's ## v0.4.0 section does not list the changed golden rows: 25, 28b\ntag none\n"
	f.files["want-ok"] = "exit 0\ntag v0.4.0\n"
	f.files["want-empty"] = "exit 1\nrefuse CHANGELOG.md's ## v0.4.0 section is empty\ntag none\n"
	f.tag("v0.3.1")
	f.land("golden",
		f.replace("testdata/golden/positive.txtar", "want: https://example.com/?id=7\n", "want: https://example.com/?id=8\n"),
		f.replace("testdata/golden/positive.txtar", "in: https://example.com/?x=1\n", "in: https://example.com/?x=2\n"),
		f.replace("testdata/golden/negative.txt", "25 https://example.com/?var=1&adid=2\n", "25 https://example.com/?var=1\n"))
	f.s = f.land("changelog", f.replace("CHANGELOG.md", "## Unreleased\n\n- Work in progress.\n", "## v0.4.0\n"))
	f.release("want-empty", "-manual", "-bump", "minor")
	f.s = f.land("changelog", f.replace("CHANGELOG.md", "## v0.4.0\n", "## v0.4.0\n\n- Row 1 now keeps `id=8`; v0.1.0 notes.\n"))
	f.release("want-unlisted", "-manual", "-bump", "minor")
	f.s = f.land("changelog", f.replace("CHANGELOG.md", "notes.\n", "notes. Rows 28b, 25: inputs.\n"))
	f.release("want-ok", "-manual", "-bump", "minor")
}

func TestReleasePullRequestChecks(t *testing.T) {
	f := relNew(t, "release_data_only")
	f.tag("v0.3.1")
	n := f.merge("data", f.syncData())
	pr := f.fake.PRs[n]

	f.fake.PRs[n] = forge.PR{Number: n, Merged: false, HeadSHA: pr.HeadSHA, BaseRef: "main"}
	f.files["want-open"] = fmt.Sprintf("exit 1\nrefuse PR #%d is not merged\ntag none\n", n)
	f.release("want-open", "-pr", fmt.Sprint(n))

	f.fake.PRs[n] = forge.PR{Number: n, Merged: true, MergeCommitSHA: pr.MergeCommitSHA, BaseRef: "release-branch"}
	f.files["want-base"] = fmt.Sprintf("exit 1\nrefuse PR #%d was merged into release-branch, not main\ntag none\n", n)
	f.release("want-base", "-pr", fmt.Sprint(n))

	side := f.l.Run("commit-tree", "-p", "HEAD", "-m", "side", f.l.Run("rev-parse", "HEAD^{tree}"))
	f.l.Push(side + ":refs/heads/side")
	f.fake.PRs[n] = forge.PR{Number: n, Merged: true, MergeCommitSHA: side, BaseRef: "main"}
	f.files["want-off-main"] = fmt.Sprintf("exit 1\nrefuse merge commit %s of PR #%d is not on origin/main (not an ancestor of refs/remotes/origin/main)\ntag none\n", side, n)
	f.release("want-off-main", "-pr", fmt.Sprint(n))

	f.files["want-missing"] = "exit 1\nrefuse GitHub API GET /repos/" + relTestRepo + "/pulls/999: 404 Not Found\ntag none\n"
	f.release("want-missing", "-pr", "999")

	// The next tag already exists on a commit outside main.
	f.fake.PRs[n] = pr
	f.l.Run("-c", "user.name=t", "-c", "user.email=t@example.invalid", "tag", "-a", "-m", "stray", "v0.3.2", side)
	f.l.Push("refs/tags/v0.3.2")
	f.files["want-exists"] = "exit 1\nrefuse tag v0.3.2 already exists\ntag none\n"
	f.release("want-exists", "-pr", fmt.Sprint(n))
	if n := f.fake.Count("POST "); n != 0 {
		t.Errorf("%d POST requests", n)
	}
}

func TestReleaseNotesWithoutSummary(t *testing.T) {
	f := relNew(t, "release_data_only")
	f.tag("v0.3.1")
	n := f.merge("data", f.syncData())
	f.fake.CommentPage = 1
	f.fake.Comments[n] = []forge.Comment{
		{ID: 1, User: "someone", AuthorAssociation: "CONTRIBUTOR", Body: data.SummaryMarker + "\nnot the routine"},
		{ID: 2, User: "maintainer", AuthorAssociation: "MEMBER", Body: "LGTM"},
	}
	f.files["want-nosum"] = "exit 0\ntag v0.3.2\nnotes notes-nosum\n"
	f.files["notes-nosum"] = fmt.Sprintf("Rules data refresh from #%d.\n\n"+relMsgNoSummary+"\n\n### Inert changes since v0.3.1\n\n- `tools/upstream/adblock-lists`\n", n, n)
	f.release("want-nosum", "-pr", fmt.Sprint(n))
	if !strings.Contains(f.stderr.String(), fmt.Sprintf("no summary comment on #%d", n)) {
		t.Errorf("stderr %q", f.stderr)
	}
}

func TestReleasePublishFailures(t *testing.T) {
	t.Run("proxy", func(t *testing.T) {
		f := relNew(t, "release_data_only")
		f.tag("v0.3.1")
		n := f.merge("data", f.syncData())
		f.goErr = errors.New("exit status 1")
		f.files["want-proxy"] = "exit 0\ntag v0.3.2\nrefuse " + fmt.Sprintf(relMsgWarmFailed, relModule, "v0.3.2", "exit status 1: proxy says no") + "\n"
		f.release("want-proxy", "-pr", fmt.Sprint(n))
		if len(f.goRuns) != relWarmTries || len(f.sleeps) != relWarmTries-1 {
			t.Errorf("%d warm-up tries, %d sleeps", len(f.goRuns), len(f.sleeps))
		}
		for _, r := range f.goRuns {
			if !slices.Contains(r.env, "GOWORK=off") || !slices.Contains(r.env, "GOFLAGS=") || r.dir == f.l.Work.Dir {
				t.Errorf("warm-up run %+v", r)
			}
		}
	})
	t.Run("release", func(t *testing.T) {
		f := relNew(t, "release_data_only")
		f.tag("v0.3.1")
		n := f.merge("data", f.syncData())
		f.fake.Releases = append(f.fake.Releases, forgetest.Release{Tag: "v0.3.2"}) // GitHub refuses a second release for the tag
		f.files["want-tagged-only"] = "exit 1\nrefuse " + fmt.Sprintf(relMsgReleaseFailed, "v0.3.2", "{S}", "GitHub API POST /repos/"+relTestRepo+"/releases: 422 Validation Failed") + "\n"
		f.release("want-tagged-only", "-pr", fmt.Sprint(n))
		if len(f.fake.Refs) != 1 || f.stdout.Len() > 0 || len(f.goRuns) > 0 {
			t.Errorf("refs %+v, stdout %q, go %+v", f.fake.Refs, f.stdout, f.goRuns)
		}
	})
}

// TestReleaseInertNotEmbedded guards relStaticClass's inert paths (any
// testdata directory, *.md, tools/…): no non-test file of the root module
// embeds one, or a change to it would ship in the library yet be classified
// release-inert.
func TestReleaseInertNotEmbedded(t *testing.T) {
	t.Run("real", func(t *testing.T) {
		if got := relEmbeddedInert(t, filepath.Join("..", "..")); len(got) > 0 {
			t.Errorf("non-test files embed paths relStaticClass calls inert: %q", got)
		}
	})
	t.Run("negatives", func(t *testing.T) {
		root := t.TempDir()
		for p, src := range map[string]string{
			"go.mod":                "module m\n",
			"x/x.go":                "package x\n\nimport _ \"embed\"\n\n//go:embed testdata/a.txt\nvar a string\n",
			"x/testdata/a.txt":      "a",
			"y/y.go":                "package y\n\nimport \"embed\"\n\n//go:embed all:* \"notes.md\"\nvar f embed.FS\n",
			"y/notes.md":            "n",
			"y/testdata/b.txt":      "b",
			"y/z_test.go":           "package y\n\n//go:embed testdata/b.txt\nvar b string\n",
			"z/z.go":                "package z\n\nimport _ \"embed\"\n\n//go:embed data.json\nvar d string\n",
			"z/data.json":           "{}",
			"nested/go.mod":         "module n\n",
			"nested/n.go":           "package n\n\n//go:embed testdata/c.txt\nvar c string\n",
			"nested/testdata/c.txt": "c",
		} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, p), []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want := []string{"x/testdata/a.txt", "y/notes.md", "y/testdata/b.txt", "y/z_test.go"}
		if got := relEmbeddedInert(t, root); !slices.Equal(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

// relEmbeddedInert returns, sorted and without duplicates, every file that
// a //go:embed directive in a non-test .go file of the module at root
// embeds and that relStaticClass classifies as inert. It skips directories
// the go command does not build (testdata, dot and underscore names) and
// nested modules.
func relEmbeddedInert(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			if n := d.Name(); n == "testdata" || strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_") {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				rest, ok := strings.CutPrefix(c.Text, "//go:embed ")
				if !ok {
					continue
				}
				for _, pat := range strings.Fields(rest) {
					if u, err := strconv.Unquote(pat); err == nil {
						pat = u
					}
					pat = strings.TrimPrefix(pat, "all:")
					matches, err := filepath.Glob(filepath.Join(filepath.Dir(p), filepath.FromSlash(pat)))
					if err != nil {
						return fmt.Errorf("%s: //go:embed %s: %v", fset.Position(c.Pos()), pat, err)
					}
					for _, m := range matches {
						err := filepath.WalkDir(m, func(q string, d fs.DirEntry, err error) error {
							if err != nil || d.IsDir() {
								return err
							}
							rel, err := filepath.Rel(root, q)
							if err == nil && relStaticClass(filepath.ToSlash(rel)) == relInert {
								found = append(found, filepath.ToSlash(rel))
							}
							return err
						})
						if err != nil {
							return err
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(found)
	return slices.Compact(found)
}
