// Package forgetest is an httptest fake of the GitHub REST endpoints that
// forge.REST calls. It keeps pull requests, workflow runs and comments set
// by the test, records every request, and stores the tags, refs and
// releases it is asked to create. It is imported only by tests.
package forgetest

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/forge"
)

// Tag is a created annotated tag object.
type Tag struct {
	SHA, Tag, Message, Object, Type string
}

// Ref is a created ref, with the commit its tag object points at.
type Ref struct {
	Ref, SHA, Commit string
}

// Release is a created release.
type Release struct {
	Tag, Name, Body string
}

// Fake serves the API for one repository. Set its fields before the code
// under test runs, or under Lock.
type Fake struct {
	Repo  string // owner/name
	Token string // the bearer token every request must carry

	PRs      map[int]forge.PR
	Runs     map[string][]forge.Run // workflow file → runs, newest first
	Comments map[int][]forge.Comment
	// RunsFor, when set, answers the runs query instead of Runs; call
	// counts the runs queries so far, from 1.
	RunsFor func(call int, workflow, headSHA, event string) []forge.Run
	// CommentPage is the page size of comment listings (default 100).
	CommentPage int
	// OnRef runs when a tag ref is created, e.g. to mirror the tag into a
	// local origin; an error fails the request with 422.
	OnRef func(tag, commit, message string) error

	Tags     []Tag
	Refs     []Ref
	Releases []Release
	Requests []string // "METHOD /path?query", in order

	mu       sync.Mutex
	runCalls int
	srv      *httptest.Server
}

// New starts a fake for repo that accepts token; it stops when t ends.
func New(t testing.TB, repo, token string) *Fake {
	f := &Fake{
		Repo: repo, Token: token,
		PRs: map[int]forge.PR{}, Runs: map[string][]forge.Run{}, Comments: map[int][]forge.Comment{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// URL is the API root to pass as forge.REST.Base.
func (f *Fake) URL() string { return f.srv.URL }

// Client returns an HTTP client that reaches the fake.
func (f *Fake) Client() *http.Client { return f.srv.Client() }

// Lock and Unlock guard the fields while the fake may be serving.
func (f *Fake) Lock()   { f.mu.Lock() }
func (f *Fake) Unlock() { f.mu.Unlock() }

// Count returns how many recorded requests start with prefix.
func (f *Fake) Count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.Requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

var (
	rePull     = regexp.MustCompile(`^/pulls/([0-9]+)$`)
	reRuns     = regexp.MustCompile(`^/actions/workflows/([^/]+)/runs$`)
	reComments = regexp.MustCompile(`^/issues/([0-9]+)/comments$`)
)

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := r.Method + " " + r.URL.Path
	if r.URL.RawQuery != "" {
		rec += "?" + r.URL.RawQuery
	}
	f.Requests = append(f.Requests, rec)
	if r.Header.Get("Authorization") != "Bearer "+f.Token {
		reply(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/repos/"+f.Repo)
	if !ok {
		reply(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	switch {
	case r.Method == http.MethodGet && rePull.MatchString(path):
		n, _ := strconv.Atoi(rePull.FindStringSubmatch(path)[1])
		pr, ok := f.PRs[n]
		if !ok {
			reply(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		reply(w, http.StatusOK, map[string]any{
			"number": n, "merged": pr.Merged, "merge_commit_sha": pr.MergeCommitSHA,
			"head": map[string]string{"sha": pr.HeadSHA}, "base": map[string]string{"ref": pr.BaseRef},
		})
	case r.Method == http.MethodGet && reRuns.MatchString(path):
		wf := reRuns.FindStringSubmatch(path)[1]
		q := r.URL.Query()
		f.runCalls++
		var runs []forge.Run
		if f.RunsFor != nil {
			runs = f.RunsFor(f.runCalls, wf, q.Get("head_sha"), q.Get("event"))
		} else {
			for _, x := range f.Runs[wf] {
				if (q.Get("head_sha") == "" || x.HeadSHA == q.Get("head_sha")) && (q.Get("event") == "" || x.Event == q.Get("event")) {
					runs = append(runs, x)
				}
			}
		}
		out := []map[string]any{}
		for _, x := range runs {
			var concl any
			if x.Conclusion != "" {
				concl = x.Conclusion
			}
			out = append(out, map[string]any{"id": x.ID, "status": x.Status, "conclusion": concl, "head_sha": x.HeadSHA, "event": x.Event})
		}
		reply(w, http.StatusOK, map[string]any{"total_count": len(out), "workflow_runs": out})
	case r.Method == http.MethodGet && reComments.MatchString(path):
		n, _ := strconv.Atoi(reComments.FindStringSubmatch(path)[1])
		size := f.CommentPage
		if size <= 0 {
			size = 100
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page = max(page, 1)
		all := f.Comments[n]
		lo, hi := min((page-1)*size, len(all)), min(page*size, len(all))
		if hi < len(all) {
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?per_page=100&page=%d>; rel="next"`, f.srv.URL, r.URL.Path, page+1))
		}
		out := []map[string]any{}
		for _, c := range all[lo:hi] {
			out = append(out, map[string]any{"id": c.ID, "user": map[string]string{"login": c.User}, "author_association": c.AuthorAssociation, "body": c.Body})
		}
		reply(w, http.StatusOK, out)
	case r.Method == http.MethodPost && path == "/git/tags":
		var in struct{ Tag, Message, Object, Type string }
		if !decode(w, r, &in) {
			return
		}
		if in.Tag == "" || in.Type != "commit" || !isHex40(in.Object) {
			reply(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed"})
			return
		}
		sum := sha1.Sum([]byte(in.Tag + "\x00" + in.Object + "\x00" + in.Message))
		t := Tag{SHA: hex.EncodeToString(sum[:]), Tag: in.Tag, Message: in.Message, Object: in.Object, Type: in.Type}
		f.Tags = append(f.Tags, t)
		reply(w, http.StatusCreated, map[string]string{"sha": t.SHA, "tag": t.Tag})
	case r.Method == http.MethodPost && path == "/git/refs":
		var in struct{ Ref, SHA string }
		if !decode(w, r, &in) {
			return
		}
		i := slices.IndexFunc(f.Tags, func(t Tag) bool { return t.SHA == in.SHA })
		exists := slices.ContainsFunc(f.Refs, func(x Ref) bool { return x.Ref == in.Ref })
		tag, isTag := strings.CutPrefix(in.Ref, "refs/tags/")
		if i < 0 || exists || !isTag || f.Tags[i].Tag != tag {
			reply(w, http.StatusUnprocessableEntity, map[string]string{"message": "Reference already exists or object unknown"})
			return
		}
		if f.OnRef != nil {
			if err := f.OnRef(tag, f.Tags[i].Object, f.Tags[i].Message); err != nil {
				reply(w, http.StatusUnprocessableEntity, map[string]string{"message": err.Error()})
				return
			}
		}
		f.Refs = append(f.Refs, Ref{Ref: in.Ref, SHA: in.SHA, Commit: f.Tags[i].Object})
		reply(w, http.StatusCreated, map[string]any{"ref": in.Ref, "object": map[string]string{"sha": in.SHA, "type": "tag"}})
	case r.Method == http.MethodPost && path == "/releases":
		var in struct {
			TagName string `json:"tag_name"`
			Name    string `json:"name"`
			Body    string `json:"body"`
		}
		if !decode(w, r, &in) {
			return
		}
		if !slices.ContainsFunc(f.Refs, func(x Ref) bool { return x.Ref == "refs/tags/"+in.TagName }) ||
			slices.ContainsFunc(f.Releases, func(x Release) bool { return x.Tag == in.TagName }) {
			reply(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed"})
			return
		}
		f.Releases = append(f.Releases, Release{Tag: in.TagName, Name: in.Name, Body: in.Body})
		reply(w, http.StatusCreated, map[string]any{"tag_name": in.TagName})
	default:
		reply(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	b, err := io.ReadAll(r.Body)
	if err == nil {
		err = json.Unmarshal(b, v)
	}
	if err != nil {
		reply(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"})
		return false
	}
	return true
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}
