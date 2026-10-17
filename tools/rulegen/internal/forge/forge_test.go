package forge_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/forge"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/forge/forgetest"
)

const (
	repoName = "owner/linkclean"
	token    = "test-token"
	commit   = "0123456789abcdef0123456789abcdef01234567"
)

func newClient(f *forgetest.Fake) *forge.REST {
	return &forge.REST{Client: f.Client(), Base: f.URL(), Repo: repoName, Token: token}
}

func TestPullRequest(t *testing.T) {
	f := forgetest.New(t, repoName, token)
	f.PRs[7] = forge.PR{Merged: true, MergeCommitSHA: commit, HeadSHA: "h", BaseRef: "main"}
	pr, err := newClient(f).PullRequest(context.Background(), 7)
	if err != nil || pr != (forge.PR{Number: 7, Merged: true, MergeCommitSHA: commit, HeadSHA: "h", BaseRef: "main"}) {
		t.Errorf("PullRequest = %+v, %v", pr, err)
	}
	_, err = newClient(f).PullRequest(context.Background(), 8)
	if apiErr, ok := errors.AsType[*forge.Error](err); !ok || apiErr.Status != http.StatusNotFound {
		t.Errorf("missing PR: %v", err)
	}
	if got := f.Requests; len(got) != 2 || got[0] != "GET /repos/owner/linkclean/pulls/7" {
		t.Errorf("requests %q", got)
	}
}

func TestBadToken(t *testing.T) {
	f := forgetest.New(t, repoName, token)
	c := newClient(f)
	c.Token = "wrong"
	_, err := c.PullRequest(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "401 Bad credentials") || strings.Contains(err.Error(), "wrong") {
		t.Errorf("err = %v", err)
	}
}

func TestInvalidRepo(t *testing.T) {
	f := forgetest.New(t, repoName, token)
	for _, r := range []string{"", "owner", "owner/", "/x", "a/b/c", "a/..", "a b/c", "owner/x?y"} {
		c := newClient(f)
		c.Repo = r
		if _, err := c.PullRequest(context.Background(), 1); err == nil || !strings.Contains(err.Error(), "invalid repository") {
			t.Errorf("repo %q: %v", r, err)
		}
	}
	if len(f.Requests) != 0 {
		t.Errorf("requests sent: %q", f.Requests)
	}
}

func TestWorkflowRuns(t *testing.T) {
	f := forgetest.New(t, repoName, token)
	f.Runs["ci.yml"] = []forge.Run{
		{ID: 3, Status: "in_progress", HeadSHA: commit, Event: "push"},
		{ID: 2, Status: "completed", Conclusion: "success", HeadSHA: commit, Event: "pull_request"},
		{ID: 1, Status: "completed", Conclusion: "failure", HeadSHA: "other", Event: "push"},
	}
	runs, err := newClient(f).WorkflowRuns(context.Background(), "ci.yml", commit, "push")
	if err != nil || len(runs) != 1 || runs[0] != (forge.Run{ID: 3, Status: "in_progress", HeadSHA: commit, Event: "push"}) {
		t.Errorf("runs = %+v, %v", runs, err)
	}
	want := "GET /repos/owner/linkclean/actions/workflows/ci.yml/runs?event=push&head_sha=" + commit + "&per_page=100"
	if f.Requests[0] != want {
		t.Errorf("request %q, want %q", f.Requests[0], want)
	}
}

func TestCommentsPaginate(t *testing.T) {
	f := forgetest.New(t, repoName, token)
	f.CommentPage = 2
	for i := range 5 {
		f.Comments[4] = append(f.Comments[4], forge.Comment{ID: int64(i), User: "u", AuthorAssociation: "OWNER", Body: "b"})
	}
	cs, err := newClient(f).Comments(context.Background(), 4)
	if err != nil || len(cs) != 5 || cs[4].ID != 4 || cs[0].User != "u" || cs[0].AuthorAssociation != "OWNER" {
		t.Errorf("comments = %+v, %v", cs, err)
	}
	if n := f.Count("GET /repos/owner/linkclean/issues/4/comments"); n != 3 {
		t.Errorf("%d pages fetched, want 3", n)
	}
}

// A Link header pointing outside the API root would carry the token
// elsewhere; the client refuses to follow it.
func TestCommentsForeignNextPage(t *testing.T) {
	var hits int
	srv := http.NewServeMux()
	srv.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Link", `<https://elsewhere.invalid/repos/owner/linkclean/issues/1/comments?page=2>; rel="next"`)
		w.Write([]byte("[]"))
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	c := &forge.REST{Client: ts.Client(), Base: ts.URL, Repo: repoName, Token: token}
	if _, err := c.Comments(context.Background(), 1); err == nil || !strings.Contains(err.Error(), "outside the API root") {
		t.Errorf("err = %v", err)
	}
	if hits != 1 {
		t.Errorf("%d requests", hits)
	}
}

func TestCreateTagAndRelease(t *testing.T) {
	f := forgetest.New(t, repoName, token)
	var mirrored []string
	f.OnRef = func(tag, c, msg string) error {
		mirrored = append(mirrored, tag+" "+c+" "+msg)
		return nil
	}
	c := newClient(f)
	ctx := context.Background()
	if err := c.CreateTag(ctx, "v0.1.0", commit, "linkclean v0.1.0"); err != nil {
		t.Fatal(err)
	}
	if len(f.Tags) != 1 || f.Tags[0].Object != commit || f.Tags[0].Type != "commit" || f.Tags[0].Message != "linkclean v0.1.0" {
		t.Errorf("tags %+v", f.Tags)
	}
	if len(f.Refs) != 1 || f.Refs[0].Ref != "refs/tags/v0.1.0" || f.Refs[0].SHA != f.Tags[0].SHA || f.Refs[0].Commit != commit {
		t.Errorf("refs %+v", f.Refs)
	}
	if len(mirrored) != 1 || mirrored[0] != "v0.1.0 "+commit+" linkclean v0.1.0" {
		t.Errorf("OnRef calls %q", mirrored)
	}
	if err := c.CreateTag(ctx, "v0.1.0", commit, "again"); err == nil || !strings.Contains(err.Error(), "422") {
		t.Errorf("duplicate tag: %v", err)
	}
	if err := c.CreateRelease(ctx, "v0.1.0", "v0.1.0", "notes"); err != nil {
		t.Fatal(err)
	}
	if len(f.Releases) != 1 || f.Releases[0] != (forgetest.Release{Tag: "v0.1.0", Name: "v0.1.0", Body: "notes"}) {
		t.Errorf("releases %+v", f.Releases)
	}
	if err := c.CreateRelease(ctx, "v9.9.9", "v9.9.9", ""); err == nil {
		t.Error("release for a missing tag accepted")
	}
}
