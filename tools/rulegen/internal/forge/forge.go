// Package forge is rulegen's view of the GitHub REST API: the few calls
// rulegen release makes (a pull request, workflow runs, PR comments,
// annotated tags and releases), behind an interface so tests substitute
// the httptest fake in package forgetest. Nothing else in rulegen talks
// to GitHub's API.
package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// PR is the part of a pull request rulegen release reads.
type PR struct {
	Number         int
	Merged         bool
	MergeCommitSHA string // the squash commit once merged
	HeadSHA        string
	BaseRef        string
}

// Run is one workflow run.
type Run struct {
	ID         int64
	Status     string // queued, in_progress, completed, …
	Conclusion string // success, failure, cancelled, … once completed
	HeadSHA    string
	Event      string
}

// Comment is one issue comment on a pull request.
type Comment struct {
	ID                int64
	User              string
	AuthorAssociation string // OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR, NONE, …
	Body              string
}

// Forge is the GitHub API surface of rulegen release.
type Forge interface {
	PullRequest(ctx context.Context, n int) (PR, error)
	// WorkflowRuns lists the runs of the workflow file (e.g. "ci.yml")
	// for headSHA triggered by event, newest first.
	WorkflowRuns(ctx context.Context, workflowFile, headSHA, event string) ([]Run, error)
	// Comments lists every issue comment on PR n, oldest first.
	Comments(ctx context.Context, n int) ([]Comment, error)
	// CreateTag creates the annotated tag object tag → commit sha with
	// message, then the ref refs/tags/<tag> pointing at it.
	CreateTag(ctx context.Context, tag, sha, message string) error
	// CreateRelease publishes a release for the existing tag.
	CreateRelease(ctx context.Context, tag, name, body string) error
}

// REST is Forge over the GitHub REST API.
type REST struct {
	Client *http.Client
	Base   string // API root, e.g. "https://api.github.com"
	Repo   string // owner/name
	Token  string // sent as a bearer token; never printed
}

var _ Forge = (*REST)(nil)

// ValidRepo reports whether s has the shape owner/name.
func ValidRepo(s string) bool {
	owner, name, ok := strings.Cut(s, "/")
	return ok && validName(owner) && validName(name)
}

func validName(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// maxBody bounds every response body read.
const maxBody = 16 << 20

// maxPages bounds a paginated listing.
const maxPages = 100

// Error is a non-success HTTP response.
type Error struct {
	Method, Path string
	Status       int
	Message      string // GitHub's "message" field, when present
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("GitHub API %s %s: %d %s", e.Method, e.Path, e.Status, msg)
}

func (c *REST) repoURL(path string, q url.Values) string {
	u := strings.TrimRight(c.Base, "/") + "/repos/" + c.Repo + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// do sends one request to the absolute URL u and decodes a response with
// status want into out (nil to discard). It returns the response headers.
func (c *REST) do(ctx context.Context, method, u string, in any, want int, out any) (http.Header, error) {
	if !ValidRepo(c.Repo) {
		return nil, fmt.Errorf("forge: invalid repository %q", c.Repo)
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "linkclean-rulegen")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBody {
		return nil, fmt.Errorf("GitHub API %s %s: response larger than %d bytes", method, req.URL.Path, maxBody)
	}
	if resp.StatusCode != want {
		e := &Error{Method: method, Path: req.URL.Path, Status: resp.StatusCode}
		var m struct{ Message string }
		if json.Unmarshal(b, &m) == nil {
			e.Message = m.Message
		}
		return nil, e
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return nil, fmt.Errorf("GitHub API %s %s: %w", method, req.URL.Path, err)
		}
	}
	return resp.Header, nil
}

func (c *REST) PullRequest(ctx context.Context, n int) (PR, error) {
	var r struct {
		Number         int    `json:"number"`
		Merged         bool   `json:"merged"`
		MergeCommitSHA string `json:"merge_commit_sha"`
		Head           struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if _, err := c.do(ctx, http.MethodGet, c.repoURL("/pulls/"+strconv.Itoa(n), nil), nil, http.StatusOK, &r); err != nil {
		return PR{}, err
	}
	return PR{Number: r.Number, Merged: r.Merged, MergeCommitSHA: r.MergeCommitSHA, HeadSHA: r.Head.SHA, BaseRef: r.Base.Ref}, nil
}

func (c *REST) WorkflowRuns(ctx context.Context, workflowFile, headSHA, event string) ([]Run, error) {
	q := url.Values{"head_sha": {headSHA}, "event": {event}, "per_page": {"100"}}
	var r struct {
		WorkflowRuns []struct {
			ID         int64  `json:"id"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			HeadSHA    string `json:"head_sha"`
			Event      string `json:"event"`
		} `json:"workflow_runs"`
	}
	u := c.repoURL("/actions/workflows/"+url.PathEscape(workflowFile)+"/runs", q)
	if _, err := c.do(ctx, http.MethodGet, u, nil, http.StatusOK, &r); err != nil {
		return nil, err
	}
	runs := make([]Run, 0, len(r.WorkflowRuns))
	for _, w := range r.WorkflowRuns {
		runs = append(runs, Run{ID: w.ID, Status: w.Status, Conclusion: w.Conclusion, HeadSHA: w.HeadSHA, Event: w.Event})
	}
	return runs, nil
}

func (c *REST) Comments(ctx context.Context, n int) ([]Comment, error) {
	u := c.repoURL("/issues/"+strconv.Itoa(n)+"/comments", url.Values{"per_page": {"100"}})
	var all []Comment
	for page := 0; u != ""; page++ {
		if page == maxPages {
			return nil, fmt.Errorf("forge: more than %d pages of comments on #%d", maxPages, n)
		}
		var r []struct {
			ID   int64 `json:"id"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
			AuthorAssociation string `json:"author_association"`
			Body              string `json:"body"`
		}
		h, err := c.do(ctx, http.MethodGet, u, nil, http.StatusOK, &r)
		if err != nil {
			return nil, err
		}
		for _, x := range r {
			all = append(all, Comment{ID: x.ID, User: x.User.Login, AuthorAssociation: x.AuthorAssociation, Body: x.Body})
		}
		if u, err = c.next(h.Get("Link")); err != nil {
			return nil, err
		}
	}
	return all, nil
}

// next returns the rel="next" URL of a Link header, or "" for none. The
// token goes with it, so it must stay under the API root.
func (c *REST) next(link string) (string, error) {
	for part := range strings.SplitSeq(link, ",") {
		target, params, ok := strings.Cut(strings.TrimSpace(part), ";")
		if !ok || !strings.Contains(params, `rel="next"`) {
			continue
		}
		target = strings.TrimSpace(target)
		if !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
			return "", fmt.Errorf("forge: malformed Link header %q", link)
		}
		target = target[1 : len(target)-1]
		root := strings.TrimRight(c.Base, "/") + "/"
		if !strings.HasPrefix(target, root) {
			return "", fmt.Errorf("forge: next page %q is outside the API root %s", target, root)
		}
		return target, nil
	}
	return "", nil
}

func (c *REST) CreateTag(ctx context.Context, tag, sha, message string) error {
	in := map[string]string{"tag": tag, "message": message, "object": sha, "type": "commit"}
	var obj struct {
		SHA string `json:"sha"`
	}
	if _, err := c.do(ctx, http.MethodPost, c.repoURL("/git/tags", nil), in, http.StatusCreated, &obj); err != nil {
		return err
	}
	if obj.SHA == "" {
		return errors.New("forge: tag object created without a sha")
	}
	ref := map[string]string{"ref": "refs/tags/" + tag, "sha": obj.SHA}
	_, err := c.do(ctx, http.MethodPost, c.repoURL("/git/refs", nil), ref, http.StatusCreated, nil)
	return err
}

func (c *REST) CreateRelease(ctx context.Context, tag, name, body string) error {
	in := map[string]any{"tag_name": tag, "name": name, "body": body, "draft": false, "prerelease": false}
	_, err := c.do(ctx, http.MethodPost, c.repoURL("/releases", nil), in, http.StatusCreated, nil)
	return err
}
