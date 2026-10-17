package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/forge"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/forge/forgetest"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/gittest"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
)

const (
	relTestRepo  = "test-owner/linkclean"
	relTestToken = "release-test-token"
	// relNewPattern is the debounce include pattern the classification
	// fixtures add upstream (rule 0), and relOldPattern an existing,
	// classified one whose entry they edit.
	relNewPattern = "*://www.newnet.example/click?*"
	relOldPattern = "*://*.3anx.net/c/*"
	// relLocatorOld is the start of relOldPattern's entry, and
	// relLocatorNew the same with its idPath locator changed.
	relLocatorOld = `"` + relOldPattern + `": {"class": "affiliate", "network": "Impact", "idPath": "^/c/([0-9]+)/"`
	relLocatorNew = `"` + relOldPattern + `": {"class": "affiliate", "network": "Impact", "idPath": "^/c/([0-9]{4,})/"`
)

// relFixture is one release scenario: a copy of linkclean with a local
// origin, the GitHub fake, and the Env the command runs with. The fixture
// txtar (testdata/release/<name>.txtar) holds the payload files and the
// expected outcome of each run.
type relFixture struct {
	t      *testing.T
	l      *gittest.Linkclean
	g      repo.Git
	fake   *forgetest.Fake
	e      *Env
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	files  map[string]string // the fixture txtar's sections
	s      string            // the commit being released ({S} in wants)
	nextPR int
	sleeps []time.Duration
	goRuns []relGoRun
	goErr  error // returned by every fake go command
	syncs  int
}

type relGoRun struct {
	dir  string
	env  []string
	args []string
}

// relNew builds the fixture's base history: the repository's rules/** and
// rules.lock.json plus testdata/release/base.txtar's synthetic module files
// and a gitlink, committed on main and pushed to origin.
func relNew(t *testing.T, name string) *relFixture {
	t.Helper()
	g := repo.Exec{Env: gittest.Isolate(t)}
	f := &relFixture{t: t, g: g, nextPR: 100}
	f.files = relReadTxtar(t, filepath.Join("testdata", "release", name+".txtar"))
	f.l = gittest.NewLinkclean(t, g, gittest.Opts{})
	for p, b := range relReadTxtar(t, filepath.Join("testdata", "release", "base.txtar")) {
		f.l.Write(p, []byte(b))
	}
	positive, err := os.ReadFile(filepath.Join("testdata", "release", "golden-positive.txt"))
	if err != nil {
		t.Fatal(err)
	}
	f.l.Write("testdata/golden/positive.txtar", positive)
	f.l.WriteGitmodules(data.CanonicalURL, data.Branch)
	f.l.SetGitlink(relFakeID("upstream 0"))
	f.l.Commit("base tree")
	f.l.Push("main")

	f.fake = forgetest.New(t, relTestRepo, relTestToken)
	// GitHub creates the tag on origin; mirror it so a later fetch sees it.
	f.fake.OnRef = func(tag, commit, msg string) error {
		_, err := g.Run(context.Background(), f.l.Origin, nil, "tag", "-a", "-m", msg, tag, commit)
		return err
	}
	f.e, f.stdout, f.stderr = newTestEnv(t, map[string]string{"GH_TOKEN": relTestToken})
	f.e.Git = g
	f.e.HTTP = f.fake.Client()
	f.e.APIBase = f.fake.URL()
	f.e.Sleep = func(ctx context.Context, d time.Duration) error {
		f.sleeps = append(f.sleeps, d)
		return ctx.Err()
	}
	f.e.Go = func(_ context.Context, dir string, env []string, args ...string) ([]byte, []byte, error) {
		f.goRuns = append(f.goRuns, relGoRun{dir, env, args})
		if f.goErr != nil {
			return nil, []byte("proxy says no"), f.goErr
		}
		return []byte(relModule + " " + args[len(args)-1][len(relModule)+1:] + "\n"), nil, nil
	}
	return f
}

// relFakeID is a stable 40-hex id for a made-up upstream commit.
func relFakeID(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// tag tags main's head as v, on origin, as an earlier release did.
func (f *relFixture) tag(v string) {
	f.t.Helper()
	f.l.Run("-c", "user.name=t", "-c", "user.email=t@example.invalid", "tag", "-a", "-m", "linkclean "+v, v)
	f.l.Push("refs/tags/" + v)
}

// land commits the edits on main as a merged PR would, pushes, gives the
// commit a green push CI run and returns it.
func (f *relFixture) land(msg string, edits ...func()) string {
	f.t.Helper()
	for _, ed := range edits {
		ed()
	}
	s := f.l.Commit(msg)
	f.l.Push("main")
	f.ci(s, "completed", "success")
	return s
}

// merge lands the edits as merged PR n and returns n; its squash commit
// becomes {S}.
func (f *relFixture) merge(msg string, edits ...func()) int {
	f.t.Helper()
	f.s = f.land(msg, edits...)
	f.nextPR++
	f.fake.PRs[f.nextPR] = forge.PR{Number: f.nextPR, Merged: true, MergeCommitSHA: f.s, HeadSHA: relFakeID("head " + f.s), BaseRef: "main"}
	return f.nextPR
}

// ci replaces the push runs of the ci workflow on s by one run in state.
func (f *relFixture) ci(s, status, conclusion string) {
	runs := slices.DeleteFunc(f.fake.Runs[relWorkflow], func(r forge.Run) bool { return r.HeadSHA == s })
	id := int64(len(f.fake.Runs[relWorkflow]) + 1000)
	f.fake.Runs[relWorkflow] = append([]forge.Run{{ID: id, Status: status, Conclusion: conclusion, HeadSHA: s, Event: "push"}}, runs...)
}

// summary posts the routine's summary comment on PR n.
func (f *relFixture) summary(n int, body string) {
	f.fake.Comments[n] = append(f.fake.Comments[n], forge.Comment{
		ID: int64(len(f.fake.Comments[n]) + 1), User: "maintainer", AuthorAssociation: "OWNER",
		Body: data.SummaryMarker + "\n" + body,
	})
}

// file returns a payload section of the fixture txtar.
func (f *relFixture) file(name string) []byte {
	f.t.Helper()
	b, ok := f.files[name]
	if !ok {
		f.t.Fatalf("fixture has no section %q", name)
	}
	return []byte(b)
}

// write returns an edit writing the payload section name to path p.
func (f *relFixture) write(p, name string) func() {
	return func() { f.l.Write(p, f.file(name)) }
}

// replace returns an edit replacing the one occurrence of old in p.
func (f *relFixture) replace(p, old, new string) func() {
	return func() {
		f.t.Helper()
		b := string(f.l.Read(p))
		if strings.Count(b, old) != 1 {
			f.t.Fatalf("%s: %d occurrences of %q", p, strings.Count(b, old), old)
		}
		f.l.Write(p, []byte(strings.Replace(b, old, new, 1)))
	}
}

// syncData is the routine's sync of an upstream change: the gitlink moves,
// a vendored file changes and the lock follows.
func (f *relFixture) syncData() func() {
	return func() {
		f.t.Helper()
		f.syncs++
		g := relFakeID(fmt.Sprintf("upstream %d", f.syncs))
		f.l.SetGitlink(g)
		f.l.Write("rules/brave/clean-urls.json", append(f.l.Read("rules/brave/clean-urls.json"), ' '))
		lock, err := data.ParseLock(f.l.Read(data.LockPath))
		if err != nil {
			f.t.Fatal(err)
		}
		lock.Upstream.Commit = g
		f.l.Write(data.LockPath, lock.Encode())
	}
}

// noOpBump moves only the gitlink, as a merged no-op Dependabot bump does.
func (f *relFixture) noOpBump() func() {
	return func() {
		f.syncs++
		f.l.SetGitlink(relFakeID(fmt.Sprintf("upstream %d", f.syncs)))
	}
}

// addUpstreamPattern adds relNewPattern to debounce rule 0, as a sync of
// an upstream commit that added it does.
func (f *relFixture) addUpstreamPattern() func() {
	const first = "[\n  {\n    \"include\": [\n"
	return f.replace("rules/brave/debounce.json", first, first+"      \""+relNewPattern+"\",\n")
}

// classify adds a debounce-classes.json entry for pattern.
func (f *relFixture) classify(pattern, entry string) func() {
	return f.replace(data.ClassesPath, "\"patterns\": {\n", "\"patterns\": {\n  \""+pattern+"\": "+entry+",\n")
}

// release runs rulegen release with args after -root and -repo, and checks
// the outcome against the fixture's want section.
func (f *relFixture) release(want string, args ...string) {
	f.t.Helper()
	f.stdout.Reset()
	f.stderr.Reset()
	before := len(f.fake.Refs)
	beforeGo := len(f.goRuns)
	argv := append([]string{"release", "-root", f.l.Work.Dir, "-repo", relTestRepo}, args...)
	code := run(context.Background(), f.e, argv)
	f.check(want, code, before, beforeGo)
}

// check compares one run with the want section: "exit N", "refuse MSG"
// (a stderr line "rulegen release: MSG"), "manual" (the routine's "manual
// release needed" line; its absence is checked when not listed), "tag V"
// (tag V created on {S}, its release published, printed and warmed; "tag
// none" means nothing was created), and "notes SECTION" (the release
// notes equal that section of the txtar).
func (f *relFixture) check(want string, code, refsBefore, goBefore int) {
	f.t.Helper()
	spec, ok := f.files[want]
	if !ok {
		f.t.Fatalf("fixture has no section %q", want)
	}
	stderr := f.stderr.String()
	lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
	manual := false
	for line := range strings.SplitSeq(strings.TrimSpace(spec), "\n") {
		key, val, _ := strings.Cut(strings.TrimSpace(line), " ")
		val = strings.ReplaceAll(val, "{S}", f.s)
		switch key {
		case "exit":
			if fmt.Sprint(code) != val {
				f.t.Errorf("exit %d, want %s; stderr:\n%s", code, val, stderr)
			}
		case "refuse":
			if !slices.Contains(lines, "rulegen release: "+val) {
				f.t.Errorf("stderr lacks the line\n\trulegen release: %s\ngot:\n%s", val, stderr)
			}
		case "manual":
			manual = true
		case "tag":
			newRefs := f.fake.Refs[refsBefore:]
			if val == "none" {
				if len(newRefs) > 0 || len(f.goRuns) > goBefore || f.stdout.Len() > 0 {
					f.t.Errorf("released anyway: refs %+v, go %v, stdout %q", newRefs, f.goRuns[goBefore:], f.stdout)
				}
				continue
			}
			if len(newRefs) != 1 || newRefs[0].Ref != "refs/tags/"+val || newRefs[0].Commit != f.s {
				f.t.Errorf("refs created %+v, want refs/tags/%s on %s", newRefs, val, f.s)
			}
			if i := slices.IndexFunc(f.fake.Releases, func(r forgetest.Release) bool { return r.Tag == val }); i < 0 || f.fake.Releases[i].Name != val {
				f.t.Errorf("no release %s: %+v", val, f.fake.Releases)
			}
			if f.stdout.String() != val+"\n" {
				f.t.Errorf("stdout %q, want %q", f.stdout, val+"\n")
			}
			runs := f.goRuns[goBefore:]
			if len(runs) == 0 || !slices.Equal(runs[0].args, []string{"list", "-m", relModule + "@" + val}) || !slices.Contains(runs[0].env, "GOPROXY="+relProxy) {
				f.t.Errorf("proxy warm-up %+v", runs)
			}
		case "notes":
			wantNotes := strings.ReplaceAll(string(f.file(val)), "{S}", f.s)
			if len(f.fake.Releases) == 0 {
				f.t.Errorf("no release, want notes %q", wantNotes)
			} else if got := f.fake.Releases[len(f.fake.Releases)-1].Body; got != wantNotes {
				f.t.Errorf("release notes:\n%s\nwant:\n%s", got, wantNotes)
			}
		default:
			f.t.Fatalf("want %q: unknown line %q", want, line)
		}
	}
	if got := strings.Contains(stderr, "rulegen release: "+data.MsgManualRelease+"\n"); got != manual {
		f.t.Errorf("%q printed: %v, want %v; stderr:\n%s", data.MsgManualRelease, got, manual, stderr)
	}
}

// relReadTxtar reads a txtar archive into its sections by name. The
// comment before the first section is dropped.
func relReadTxtar(t testing.TB, file string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	name := ""
	for line := range strings.SplitAfterSeq(string(b), "\n") {
		if n, ok := relTxtarMarker(strings.TrimSuffix(line, "\n")); ok {
			if _, dup := out[n]; dup {
				t.Fatalf("%s: section %q twice", file, n)
			}
			name = n
			out[name] = ""
			continue
		}
		if name != "" {
			out[name] += line
		}
	}
	return out
}

// TestReleaseFixtures runs the release_* rows of the tooling test table.
func TestReleaseFixtures(t *testing.T) {
	const affiliate = `{"class": "affiliate", "network": "NewNet", "idParam": "aid", "ref": "vendor doc"}`
	fixtures := map[string]func(t *testing.T){
		// Push CI success on S → tag vX.Y.(Z+1) on S; release created;
		// proxy warm called.
		"release_data_only": func(t *testing.T) {
			f := relNew(t, "release_data_only")
			f.tag("v0.3.1")
			n := f.merge("Bump tools/upstream/adblock-lists", f.syncData())
			f.summary(n, "## rulegen summary\n\nclean-urls.json: +1 param")
			// Anyone can comment with the marker; only a writer's counts.
			f.fake.Comments[n] = append(f.fake.Comments[n], forge.Comment{ID: 9, User: "drive-by", AuthorAssociation: "NONE", Body: data.SummaryMarker + "\nforged notes"})
			f.release("want", "-pr", fmt.Sprint(n))
			if len(f.fake.Tags) != 1 || f.fake.Tags[0].Message != fmt.Sprintf("linkclean v0.3.2: rules data refresh (#%d)", n) {
				t.Errorf("tag objects %+v", f.fake.Tags)
			}
			if f.fake.Count("GET /repos/"+relTestRepo+"/actions/workflows/ci.yml/runs?event=push&head_sha="+f.s) != 1 {
				t.Errorf("CI queries: %q", f.fake.Requests)
			}
			// Released again, nothing has changed since the new tag.
			f.release("want-again", "-pr", fmt.Sprint(n))
		},
		// Push CI failure or still pending after the poll → refuses; no
		// tag.
		"release_ci_not_green": func(t *testing.T) {
			t.Run("failure", func(t *testing.T) {
				f := relNew(t, "release_ci_not_green")
				f.tag("v0.3.1")
				n := f.merge("data", f.syncData())
				f.ci(f.s, "completed", "failure")
				f.release("want-failure", "-pr", fmt.Sprint(n))
				if len(f.sleeps) != 0 {
					t.Errorf("waited %v on a finished run", f.sleeps)
				}
			})
			t.Run("pending", func(t *testing.T) {
				f := relNew(t, "release_ci_not_green")
				f.tag("v0.3.1")
				n := f.merge("data", f.syncData())
				f.ci(f.s, "in_progress", "")
				f.release("want-pending", "-pr", fmt.Sprint(n))
				var total time.Duration
				for _, d := range f.sleeps {
					total += d
				}
				if total != relPollFor || len(f.sleeps) != int(relPollFor/relPollEvery) {
					t.Errorf("polled %d times over %v, want %v", len(f.sleeps), total, relPollFor)
				}
			})
			t.Run("missing", func(t *testing.T) {
				f := relNew(t, "release_ci_not_green")
				f.tag("v0.3.1")
				n := f.merge("data", f.syncData())
				f.fake.Runs[relWorkflow] = nil
				f.release("want-missing", "-pr", fmt.Sprint(n))
			})
			t.Run("green_after_wait", func(t *testing.T) {
				f := relNew(t, "release_ci_not_green")
				f.tag("v0.3.1")
				n := f.merge("data", f.syncData())
				s := f.s
				f.fake.RunsFor = func(call int, _, head, _ string) []forge.Run {
					r := forge.Run{ID: 1, Status: "in_progress", HeadSHA: head, Event: "push"}
					if call >= 4 {
						r.Status, r.Conclusion = "completed", "success"
					}
					if head != s {
						return nil
					}
					return []forge.Run{r}
				}
				f.release("want-green", "-pr", fmt.Sprint(n))
				if len(f.sleeps) != 3 {
					t.Errorf("slept %d times, want 3", len(f.sleeps))
				}
			})
		},
		// An overlay commit landed on main since the last tag; the data PR
		// merges → refuses "unreleased non-data changes since vX.Y.Z"; no
		// tag.
		"release_unreleased_non_data": func(t *testing.T) {
			f := relNew(t, "release_unreleased_non_data")
			f.tag("v0.3.1")
			f.land("overlay fix", f.replace(data.OverlayPath, `"id": "google-url"`, `"id": "google-url-v2"`))
			n := f.merge("data", f.syncData())
			f.release("want", "-pr", fmt.Sprint(n))
		},
		// Data + a golang-x require bump → patch tag. With a go directive
		// change → refuses.
		"release_deps_only": func(t *testing.T) {
			t.Run("require_bump", func(t *testing.T) {
				f := relNew(t, "release_deps_only")
				f.tag("v0.3.1")
				f.land("Bump golang.org/x/net", f.write("go.mod", "go.mod.require"), f.write("go.sum", "go.sum.require"))
				n := f.merge("data", f.syncData())
				f.summary(n, "summary")
				f.release("want-require", "-pr", fmt.Sprint(n))
			})
			t.Run("go_directive", func(t *testing.T) {
				f := relNew(t, "release_deps_only")
				f.tag("v0.3.1")
				f.land("go 1.27", f.write("go.mod", "go.mod.directive"))
				n := f.merge("data", f.syncData())
				f.release("want-directive", "-pr", fmt.Sprint(n))
			})
			t.Run("new_module", func(t *testing.T) {
				f := relNew(t, "release_deps_only")
				f.tag("v0.3.1")
				f.land("new dependency", f.write("go.mod", "go.mod.added"))
				n := f.merge("data", f.syncData())
				f.release("want-added", "-pr", fmt.Sprint(n))
			})
		},
		// The routine's monthly cleanup PR (edits only rules/acks.json)
		// merged since the last tag, then the data PR → patch tag; release
		// notes list the inert paths.
		"release_after_acks_cleanup": func(t *testing.T) {
			f := relNew(t, "release_after_acks_cleanup")
			f.tag("v0.3.1")
			f.land("rules: remove lapsed acks", f.write(data.AcksPath, "acks.json"))
			n := f.merge("data", f.syncData())
			f.summary(n, "summary")
			f.release("want", "-pr", fmt.Sprint(n))
		},
		// A PR touching only semantics_test.go, known-broken.txt,
		// keep-coverage-exempt.txt and README.md merged since the last tag,
		// then the data PR → patch tag.
		"release_after_test_only_change": func(t *testing.T) {
			f := relNew(t, "release_after_test_only_change")
			f.tag("v0.3.1")
			f.land("tests and docs",
				f.write("semantics_test.go", "semantics_test.go"),
				f.write("testdata/corpus/eval-2026-10/known-broken.txt", "known-broken.txt"),
				f.write("testdata/keep-coverage-exempt.txt", "keep-coverage-exempt.txt"),
				f.write("README.md", "README.md"))
			n := f.merge("data", f.syncData())
			f.summary(n, "summary")
			f.release("want", "-pr", fmt.Sprint(n))
		},
		// Since the last tag, a testdata/golden/positive.txtar change (or a
		// CHANGELOG.md change, or a non-test .go change such as doc.go) →
		// refuses "unreleased non-data changes"; no tag.
		"release_golden_or_changelog_refuses": func(t *testing.T) {
			for _, c := range []struct{ name, path, old, new string }{
				{"golden", "testdata/golden/positive.txtar", "want: https://example.com/?id=7\n", "want: https://example.com/?id=8\n"},
				{"changelog", "CHANGELOG.md", "## Unreleased\n", "## Unreleased\n\n- Next: a fix.\n"},
				{"doc_go", "doc.go", "package linkclean", "package linkclean // import \"github.com/Espresso-Aficionados/linkclean\""},
			} {
				t.Run(c.name, func(t *testing.T) {
					f := relNew(t, "release_golden_or_changelog_refuses")
					f.tag("v0.3.1")
					f.land("edit "+c.path, f.replace(c.path, c.old, c.new))
					n := f.merge("data", f.syncData())
					f.release("want-"+c.name, "-pr", fmt.Sprint(n))
				})
			}
		},
		// Since the last tag only the gitlink (merged no-op bumps) and other
		// inert paths changed → refuses "no data change since vX.Y.Z;
		// nothing to release".
		"release_no_data_change": func(t *testing.T) {
			f := relNew(t, "release_no_data_change")
			f.tag("v0.3.1")
			f.land("no-op bump 1", f.noOpBump())
			f.land("acks", f.write(data.AcksPath, "acks.json"))
			n := f.merge("no-op bump 2", f.noOpBump())
			f.release("want", "-pr", fmt.Sprint(n))
		},
		// Overlay change: -bump patch refused; -bump minor without a
		// CHANGELOG section refused; with it → tag.
		"release_manual_minor": func(t *testing.T) {
			f := relNew(t, "release_manual_minor")
			f.tag("v0.3.1")
			f.s = f.land("overlay fix", f.replace(data.OverlayPath, `"id": "google-url"`, `"id": "google-url-v2"`))
			f.release("want-patch", "-manual", "-bump", "patch")
			f.release("want-no-section", "-manual", "-bump", "minor")
			f.s = f.land("changelog", f.write("CHANGELOG.md", "CHANGELOG.md"))
			f.release("want-minor", "-manual", "-bump", "minor")
			if len(f.fake.Tags) != 1 || f.fake.Tags[0].Message != "linkclean v0.4.0" {
				t.Errorf("tag objects %+v", f.fake.Tags)
			}
		},
		// No prior tag → v0.1.0.
		"release_first_tag": func(t *testing.T) {
			f := relNew(t, "release_first_tag")
			n := f.merge("data", f.syncData())
			f.release("want-data", "-pr", fmt.Sprint(n))
			f.s = f.land("changelog", f.write("CHANGELOG.md", "CHANGELOG.md"))
			f.release("want-patch", "-manual", "-bump", "patch")
			f.release("want-minor", "-manual", "-bump", "minor")
		},
		// Since the last tag: a data PR adds debounce pattern P upstream
		// plus a human commit adding P's entry to debounce-classes.json →
		// patch tag; release notes list P and its class.
		"release_classification_only": func(t *testing.T) {
			f := relNew(t, "release_classification_only")
			f.tag("v0.3.1")
			n := f.merge("data + classification", f.syncData(), f.addUpstreamPattern(), f.classify(relNewPattern, affiliate))
			f.summary(n, "summary")
			f.release("want", "-pr", fmt.Sprint(n))
		},
		// Since the last tag: an edit changing an existing entry's class or
		// locator → refuses "unreleased non-data changes".
		"release_reclassification_refuses": func(t *testing.T) {
			for _, c := range []struct{ name, old, new string }{
				{"class", `"` + relOldPattern + `": {"class": "affiliate"`, `"` + relOldPattern + `": {"class": "unwrap"`},
				{"locator", relLocatorOld, relLocatorNew},
			} {
				t.Run(c.name, func(t *testing.T) {
					f := relNew(t, "release_reclassification_refuses")
					f.tag("v0.3.1")
					f.land("reclassify", f.replace(data.ClassesPath, c.old, c.new))
					n := f.merge("data", f.syncData())
					f.release("want", "-pr", fmt.Sprint(n))
				})
			}
		},
		// Since the last tag: one commit adds the entry for new upstream
		// pattern P and changes existing entry Q's locator → refuses
		// "unreleased non-data changes" (the diff is judged as a whole).
		"release_classification_mixed_refuses": func(t *testing.T) {
			f := relNew(t, "release_classification_mixed_refuses")
			f.tag("v0.3.1")
			n := f.merge("data + classification", f.syncData(), f.addUpstreamPattern(), f.classify(relNewPattern, affiliate),
				f.replace(data.ClassesPath, relLocatorOld, relLocatorNew))
			f.release("want", "-pr", fmt.Sprint(n))
		},
	}
	// Every release_* fixture file has a subtest and vice versa.
	files, err := filepath.Glob(filepath.Join("testdata", "release", "release_*.txtar"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range files {
		names = append(names, strings.TrimSuffix(filepath.Base(p), ".txtar"))
	}
	for name := range fixtures {
		if !slices.Contains(names, name) {
			t.Errorf("fixture %s has no testdata/release/%s.txtar", name, name)
		}
	}
	for _, name := range names {
		run, ok := fixtures[name]
		if !ok {
			t.Errorf("testdata/release/%s.txtar has no subtest", name)
			continue
		}
		t.Run(name, run)
	}
}
