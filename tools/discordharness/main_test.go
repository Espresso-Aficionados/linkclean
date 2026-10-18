package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Synthetic IDs and token; nothing here is a real Discord object.
const (
	testGuild   = "100000000000000001"
	testChannel = "100000000000000002"
	testBot     = "100000000000000003"
	testRole    = "100000000000000004"
	testUser    = "100000000000000005"
	testToken   = "test-token.not-a-real-one_42"
	testName    = "harness-test"
	testBotName = "harness-bot"
	testGuildNm = "harness-guild"
)

// fakeClock is the env's now and sleep: sleeping advances it.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return ctx.Err()
}

type request struct {
	method, path, auth string
	body               []byte
	at                 time.Time
}

type stored struct {
	message
	created time.Time
}

// fakeDiscord is the REST API the harness talks to, on an httptest server.
type fakeDiscord struct {
	t     *testing.T
	clock *fakeClock
	srv   *httptest.Server

	mu       sync.Mutex
	reqs     []request
	me       user
	ch       channel
	g        guild
	msgs     map[string]*stored
	nextID   int
	limit429 int                       // POSTs still to answer with 429
	embedFor func(content string) bool // messages that unfurl, 5 s after creation
	postErr  int                       // non-zero: POST answers this status
	badReply int                       // POSTs from this one on (1-based) store the message, then answer garbage
	gone     map[string]bool           // DELETE answers 404 Unknown Message
}

func newFake(t *testing.T) *fakeDiscord {
	f := &fakeDiscord{
		t:     t,
		clock: &fakeClock{t: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)},
		me:    user{ID: testBot, Username: testBotName, Bot: true},
		ch: channel{ID: testChannel, Type: guildText, GuildID: testGuild, Name: testName, Overwrites: []overwrite{
			{ID: testGuild, Type: 0, Deny: strconv.Itoa(viewChannel | 1<<11)},
		}},
		g:        guild{ID: testGuild, Name: testGuildNm},
		msgs:     map[string]*stored{},
		nextID:   200000000000000000,
		embedFor: func(string) bool { return false },
		gone:     map[string]bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v10/users/@me", func(w http.ResponseWriter, r *http.Request) { f.json(w, 200, f.me) })
	mux.HandleFunc("GET /api/v10/channels/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != f.ch.ID {
			f.json(w, 404, map[string]any{"code": 10003, "message": "Unknown Channel"})
			return
		}
		f.json(w, 200, f.ch)
	})
	mux.HandleFunc("GET /api/v10/guilds/{id}", func(w http.ResponseWriter, r *http.Request) { f.json(w, 200, f.g) })
	mux.HandleFunc("POST /api/v10/channels/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.limit429 > 0 {
			f.limit429--
			w.Header().Set("Retry-After", "3")
			f.jsonLocked(w, 429, map[string]any{"message": "You are being rate limited.", "retry_after": 3.0})
			return
		}
		if f.postErr != 0 {
			f.jsonLocked(w, f.postErr, map[string]any{"code": 50013, "message": "Missing Permissions"})
			return
		}
		var o outgoing
		if err := json.Unmarshal(f.reqs[len(f.reqs)-1].body, &o); err != nil {
			f.jsonLocked(w, 400, map[string]any{"code": 50035, "message": "Invalid Form Body"})
			return
		}
		f.nextID++
		m := &stored{message: message{ID: strconv.Itoa(f.nextID), ChannelID: r.PathValue("id"), Content: o.Content, Flags: o.Flags, Embeds: []embed{}}, created: f.clock.now()}
		f.msgs[m.ID] = m
		if f.badReply > 0 && len(f.msgs) >= f.badReply {
			w.WriteHeader(200)
			io.WriteString(w, "{not json")
			return
		}
		f.jsonLocked(w, 200, m.message)
	})
	mux.HandleFunc("GET /api/v10/channels/{cid}/messages/{mid}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		m, ok := f.msgs[r.PathValue("mid")]
		if !ok {
			f.jsonLocked(w, 404, map[string]any{"code": unknownMessage, "message": "Unknown Message"})
			return
		}
		out := m.message
		if m.Flags&flagSuppress == 0 && f.embedFor(m.Content) && !f.clock.now().Before(m.created.Add(5*time.Second)) {
			out.Embeds = []embed{{Type: "link", URL: "https://example.com/unfurled", Title: "Example"}}
		}
		f.jsonLocked(w, 200, out)
	})
	mux.HandleFunc("DELETE /api/v10/channels/{cid}/messages/{mid}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id := r.PathValue("mid")
		if _, ok := f.msgs[id]; !ok || f.gone[id] {
			f.jsonLocked(w, 404, map[string]any{"code": unknownMessage, "message": "Unknown Message"})
			return
		}
		delete(f.msgs, id)
		w.WriteHeader(204)
	})
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, request{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body, at: f.clock.now()})
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDiscord) json(w http.ResponseWriter, status int, v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jsonLocked(w, status, v)
}

func (f *fakeDiscord) jsonLocked(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (f *fakeDiscord) requests() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

func (f *fakeDiscord) count(method string) int {
	n := 0
	for _, r := range f.requests() {
		if r.method == method {
			n++
		}
	}
	return n
}

// fakeTTY is /dev/tty: it reads the typed input and records the prompt.
type fakeTTY struct {
	in     io.Reader
	prompt *bytes.Buffer
}

func (t fakeTTY) Read(p []byte) (int, error)  { return t.in.Read(p) }
func (t fakeTTY) Write(p []byte) (int, error) { return t.prompt.Write(p) }
func (t fakeTTY) Close() error                { return nil }

type harness struct {
	f              *fakeDiscord
	e              *env
	stdout, stderr *bytes.Buffer
	prompt         *bytes.Buffer
	dir            string // holds the token, payloads and out directories
}

// newHarness returns a fake Discord and an env whose terminal types typed.
func newHarness(t *testing.T, typed string) *harness {
	f := newFake(t)
	h := &harness{f: f, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, prompt: &bytes.Buffer{}, dir: outsideGit(t)}
	h.e = &env{
		stdout: h.stdout, stderr: h.stderr,
		http:    f.srv.Client(),
		apiBase: f.srv.URL + "/api/v10",
		openTTY: func() (io.ReadWriteCloser, error) { return fakeTTY{strings.NewReader(typed), h.prompt}, nil },
		now:     f.clock.now,
		sleep:   f.clock.sleep,
	}
	return h
}

// outsideGit returns a temporary directory, skipping the test when the
// temporary directory is itself inside a git work tree.
func outsideGit(t *testing.T) string {
	d := t.TempDir()
	if root, in := gitTree(d); in {
		t.Skipf("the temporary directory is inside the git work tree at %s", root)
	}
	return d
}

func (h *harness) token(t *testing.T, mode os.FileMode) string {
	p := filepath.Join(h.dir, "token")
	if err := os.WriteFile(p, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func (h *harness) payloads(t *testing.T, pf any) string {
	b, err := json.Marshal(pf)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(h.dir, "payloads.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (h *harness) out() string { return filepath.Join(h.dir, "out") }

func (h *harness) run(args ...string) int { return run(context.Background(), h.e, args) }

// sendArgs are the flags of a send with the given phase.
func (h *harness) sendArgs(t *testing.T, phase string) []string {
	return []string{"-payloads", h.payloads(t, samplePayloads()), "-token-file", h.token(t, 0o600),
		"-guild", testGuild, "-channel", testChannel, "-phase", phase, "-out", h.out()}
}

func (h *harness) cleanupArgs(t *testing.T) []string {
	return []string{"-cleanup", "-token-file", h.token(t, 0o600), "-guild", testGuild, "-channel", testChannel, "-out", h.out()}
}

// samplePayloads has one case of each kind TestHarnessPayloads writes.
func samplePayloads() payloadFile {
	return payloadFile{Version: 1, Cases: []payloadCase{
		{ID: "G01", Phase: "grammar", Source: "control: bare link", Content: "G01\nhttps://example.com/unfurls", Predicted: []predictedLink{{URL: "https://example.com/unfurls", Kind: "Bare", WouldEmbed: true}}},
		{ID: "G02", Phase: "grammar", Source: "find.txtar: autolink-tab", Markers: []string{"A1 a tab inside the target is kept"}, Content: "G02\n<https://example.com/x\ty> `a|b` <@1>", UserCopy: true, Predicted: []predictedLink{{URL: "https://example.com/x\ty", Kind: "Suppressed"}}},
		{ID: "R-0", Phase: "reply", Source: "original", Content: "R-0\noriginal", Predicted: []predictedLink{}},
		{ID: "R-1", Phase: "reply", Source: "subtext renders", Content: "https://example.com/harness/R-1\n-# tracking: utm\\_source", Flags: 4, ReplyTo: "R-0", ExpectNoEmbeds: true, Predicted: []predictedLink{{URL: "https://example.com/harness/R-1", Kind: "Bare", WouldEmbed: true}}},
		{ID: "R-3b", Phase: "reply", Source: "no SUPPRESS_EMBEDS", Content: "<https://example.com/harness/R-3b>\n-# [search](<https://duckduckgo.com/?q=%22x%22>) unfurls", ReplyTo: "R-0", ExpectNoEmbeds: true, Predicted: []predictedLink{}},
		{ID: "B-1", Phase: "bidi", Source: "raw U+202E", Content: "abc\u202edef https://example.com/\nghi jkl", Predicted: []predictedLink{{URL: "https://example.com/", Kind: "Bare", WouldEmbed: true}}},
	}}
}

func readRunFile(t *testing.T, dir string) *runRecord {
	t.Helper()
	rec, err := readRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// checkNoLeaks asserts the token appears nowhere the harness writes.
func (h *harness) checkNoLeaks(t *testing.T) {
	t.Helper()
	outs := map[string]string{"stdout": h.stdout.String(), "stderr": h.stderr.String(), "tty": h.prompt.String()}
	for _, name := range []string{runFile, observationsFile} {
		if b, err := os.ReadFile(filepath.Join(h.out(), name)); err == nil {
			outs[name] = string(b)
		}
	}
	for name, s := range outs {
		if strings.Contains(s, testToken) {
			t.Errorf("%s contains the token", name)
		}
	}
}

func TestSend(t *testing.T) {
	h := newHarness(t, testName+"\n")
	h.f.limit429 = 1
	h.f.embedFor = func(c string) bool { return strings.Contains(c, "unfurls") }
	if code := h.run(h.sendArgs(t, "all")...); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, h.stderr)
	}
	h.checkNoLeaks(t)

	reqs := h.f.requests()
	allowed := regexp.MustCompile(`^/api/v10/(users/@me|guilds/` + testGuild + `|channels/` + testChannel + `(/messages(/[0-9]+)?)?)$`)
	var posts []outgoing
	for i, r := range reqs {
		if r.auth != "Bot "+testToken {
			t.Errorf("request %d %s %s: Authorization %q", i, r.method, r.path, r.auth)
		}
		if !allowed.MatchString(r.path) || r.method == "DELETE" {
			t.Errorf("request %d: %s %s is outside the guild and channel", i, r.method, r.path)
		}
		if i > 0 {
			gap := r.at.Sub(reqs[i-1].at)
			if gap < sendGap {
				t.Errorf("request %d came %v after the previous one", i, gap)
			}
			if reqs[i-1].method == "POST" && i == 4 && gap < 3*time.Second {
				t.Errorf("the retry after a 429 came %v later, want Retry-After's 3 s", gap)
			}
		}
		if r.method == "POST" {
			var o map[string]json.RawMessage
			if err := json.Unmarshal(r.body, &o); err != nil {
				t.Fatal(err)
			}
			if got := string(o["allowed_mentions"]); got != `{"parse":[],"replied_user":false}` {
				t.Errorf("POST %d: allowed_mentions %s", i, got)
			}
			var out outgoing
			json.Unmarshal(r.body, &out)
			posts = append(posts, out)
		}
	}
	// Three preflight GETs, one 429 and six sends, then the reads.
	if len(posts) != 7 || posts[0].Content != posts[1].Content {
		t.Fatalf("%d POSTs; the first should be retried after the 429", len(posts))
	}
	posts = posts[1:]

	rec := readRunFile(t, h.out())
	if len(rec.Messages) != 6 || rec.ChannelID != testChannel || rec.BotID != testBot || rec.GuildID != testGuild {
		t.Fatalf("run.json: %+v", rec)
	}
	byCase := map[string]sentMessage{}
	for i, m := range rec.Messages {
		byCase[m.Case] = m
		pc := samplePayloads().Cases[i]
		if posts[i].Content != pc.Content || posts[i].Flags != pc.Flags || m.Case != pc.ID || !m.Polled || !m.ContentEqual {
			t.Errorf("message %d: sent %+v, recorded %+v", i, posts[i], m)
		}
		if ref := posts[i].MessageReference; (ref != nil) != (pc.ReplyTo != "") || ref != nil && (ref.MessageID != byCase[pc.ReplyTo].MessageID || !ref.FailIfNotExists) {
			t.Errorf("message %d: message_reference %+v", i, ref)
		}
	}
	if m := byCase["G01"]; len(m.Embeds) != 1 || len(m.FailedChecks) != 0 {
		t.Errorf("G01: %+v; the embed appears 5 s after the send", m)
	}
	if m := byCase["R-1"]; len(m.Embeds) != 0 || len(m.FailedChecks) != 0 || m.ReturnedFlags != 4 {
		t.Errorf("R-1: %+v", m)
	}
	if m := byCase["R-3b"]; len(m.FailedChecks) != 1 || !strings.Contains(m.FailedChecks[0], "embeds") {
		t.Errorf("R-3b: %+v; its embed must fail the no-embed check", m)
	}
	// R-1 expects no embed, so it is read again until 20 s after its send.
	var lastRead time.Time
	for _, r := range reqs {
		if r.method == "GET" && r.path == "/api/v10/channels/"+testChannel+"/messages/"+byCase["R-1"].MessageID {
			lastRead = r.at
		}
	}
	if lastRead.Sub(byCase["R-1"].SentAt) < embedWait {
		t.Errorf("R-1 was last read %v after its send, want at least %v", lastRead.Sub(byCase["R-1"].SentAt), embedWait)
	}

	for _, want := range []string{testBotName, testGuildNm, "#" + testName, "send 6 messages"} {
		if !strings.Contains(h.prompt.String(), want) {
			t.Errorf("the confirmation prompt %q does not show %q", h.prompt, want)
		}
	}
	if !strings.Contains(h.stdout.String(), "API check failed: R-3b") {
		t.Errorf("stdout does not report R-3b's failed check:\n%s", h.stdout)
	}
	checkObservations(t, h, filepath.Join(h.out(), observationsFile))
}

// checkObservations asserts observations.md can be committed: no Discord
// IDs, names or local paths, and the tables are there.
func checkObservations(t *testing.T, h *harness, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	// The local paths that could leak: the run's directory, the temporary
	// directory and the home directory.
	home, _ := os.UserHomeDir()
	for _, bad := range []string{testBotName, testGuildNm, testName, h.dir, filepath.Clean(os.TempDir()), home} {
		if len(bad) > 1 && strings.Contains(s, bad) { // a home of "/" would match every URL
			t.Errorf("observations.md contains %q", bad)
		}
	}
	if m := regexp.MustCompile(`[0-9]{15,}`).FindString(s); m != "" {
		t.Errorf("observations.md contains the ID-like number %s", m)
	}
	for _, want := range []string{
		"## Phase G: grammar", "## Phase R: reply format", "## Phase B: bidi", "## User copies",
		"| G01 | control: bare link | ` \"G01\\nhttps://example.com/unfurls\" ` |",
		"embed link ` \"https://example.com/unfurled\" `",
		"`` \"G02\\n<https://example.com/x\\ty> `a\\|b` <@1>\" ``", // a backtick in the content widens the span
		"unverified: A1 a tab inside the target is kept",
		"`a\\|b`", // a '|' in a cell is escaped
		"G02:\n\n```\nG02\n<https://example.com/x\ty> `a|b` <@1>\n```",
		"- R-3b: 1 embeds, want none",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("observations.md has no %q:\n%s", want, s)
		}
	}
	// Every table row has as many cells as its header.
	header := 0
	for _, l := range strings.Split(s, "\n") {
		if !strings.HasPrefix(l, "| ") {
			continue
		}
		n := strings.Count(strings.ReplaceAll(l, `\|`, ""), "|")
		if strings.HasPrefix(l, "| Case ") {
			header = n
		} else if n != header {
			t.Errorf("row has %d bars, header %d: %s", n, header, l)
		}
	}
}

func TestSendPhase(t *testing.T) {
	h := newHarness(t, testName+"\n")
	if code := h.run(h.sendArgs(t, "reply")...); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, h.stderr)
	}
	var cases []string
	for _, m := range readRunFile(t, h.out()).Messages {
		cases = append(cases, m.Case)
	}
	if !slices.Equal(cases, []string{"R-0", "R-1", "R-3b"}) {
		t.Errorf("-phase reply sent %v", cases)
	}
	if b, _ := os.ReadFile(filepath.Join(h.out(), observationsFile)); strings.Contains(string(b), "Phase G") {
		t.Error("observations.md has a Phase G table for a reply-only run")
	}
}

func TestSendRefusesExistingRun(t *testing.T) {
	h := newHarness(t, testName+"\n"+testName+"\n")
	if code := h.run(h.sendArgs(t, "bidi")...); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, h.stderr)
	}
	before := len(h.f.requests())
	if code := h.run(h.sendArgs(t, "bidi")...); code != exitFail || !strings.Contains(h.stderr.String(), "already holds a run.json") {
		t.Errorf("second run into the same -out: exit %d, stderr %s", code, h.stderr)
	}
	if len(h.f.requests()) != before {
		t.Error("the refused run made requests")
	}
}

func TestPreflightRefuses(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(f *fakeDiscord)
		want  string
	}{
		{"not a bot", func(f *fakeDiscord) { f.me.Bot = false }, "not a bot"},
		{"other guild", func(f *fakeDiscord) { f.ch.GuildID = "100000000000000009" }, "not in -guild"},
		{"voice channel", func(f *fakeDiscord) { f.ch.Type = 2 }, "not a text channel"},
		{"no overwrite", func(f *fakeDiscord) { f.ch.Overwrites = nil }, "does not deny View Channel"},
		{"send denied only", func(f *fakeDiscord) { f.ch.Overwrites[0].Deny = strconv.Itoa(1 << 11) }, "does not deny View Channel"},
		{"another role denied", func(f *fakeDiscord) { f.ch.Overwrites[0].ID = "100000000000000008" }, "does not deny View Channel"},
		{"member overwrite", func(f *fakeDiscord) { f.ch.Overwrites[0].Type = 1 }, "does not deny View Channel"},
		{"bad deny", func(f *fakeDiscord) { f.ch.Overwrites[0].Deny = "x" }, "does not deny View Channel"},
		{"role allows view", func(f *fakeDiscord) {
			f.ch.Overwrites = append(f.ch.Overwrites, overwrite{ID: testRole, Type: 0, Allow: "1024", Deny: "0"})
		}, "allows View Channel"},
		{"member allows view", func(f *fakeDiscord) {
			f.ch.Overwrites = append(f.ch.Overwrites, overwrite{ID: testUser, Type: 1, Allow: "3072", Deny: "0"})
		}, "allows View Channel"},
		{"bad allow", func(f *fakeDiscord) {
			f.ch.Overwrites = append(f.ch.Overwrites, overwrite{ID: testRole, Type: 0, Allow: "x", Deny: "0"})
		}, "unreadable allow"},
		{"guild mismatch", func(f *fakeDiscord) { f.g.ID = "100000000000000009" }, "another guild"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, testName+"\n")
			c.setup(h.f)
			if code := h.run(h.sendArgs(t, "all")...); code != exitFail || !strings.Contains(h.stderr.String(), c.want) {
				t.Errorf("exit %d, stderr %q, want %q", code, h.stderr, c.want)
			}
			if n := h.f.count("POST"); n != 0 || h.prompt.Len() != 0 {
				t.Errorf("%d POSTs, prompt %q after a failed preflight", n, h.prompt)
			}
			h.checkNoLeaks(t)
		})
	}
}

// TestPreflightAllows checks the View Channel allows the preflight accepts:
// the bot's own, and those -allow names.
func TestPreflightAllows(t *testing.T) {
	for _, c := range []struct {
		name  string
		extra []string
		ow    overwrite
	}{
		{"bot member", nil, overwrite{ID: testBot, Type: 1, Allow: "1024", Deny: "0"}},
		{"allowed role", []string{"-allow", testRole}, overwrite{ID: testRole, Type: 0, Allow: "1024", Deny: "0"}},
		{"allowed member", []string{"-allow", testRole + "," + testUser}, overwrite{ID: testUser, Type: 1, Allow: "1024", Deny: "0"}},
		{"role without view", nil, overwrite{ID: testRole, Type: 0, Allow: "2048", Deny: "0"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, testName+"\n")
			h.f.ch.Overwrites = append(h.f.ch.Overwrites, c.ow)
			if code := h.run(append(h.sendArgs(t, "grammar"), c.extra...)...); code != exitOK {
				t.Errorf("exit %d, stderr %q", code, h.stderr)
			}
			if h.f.count("POST") == 0 {
				t.Error("nothing sent")
			}
			h.checkNoLeaks(t)
		})
	}
}

// TestSendLostResponse checks that a POST whose response is lost after
// Discord created the message says the message may exist unrecorded.
func TestSendLostResponse(t *testing.T) {
	h := newHarness(t, testName+"\n")
	h.f.badReply = 2
	if code := h.run(h.sendArgs(t, "grammar")...); code != exitFail {
		t.Fatalf("exit %d, want %d", code, exitFail)
	}
	rec := readRunFile(t, h.out())
	if len(rec.Messages) != 1 || len(h.f.msgs) != 2 {
		t.Fatalf("%d recorded, %d created; want 1 and 2", len(rec.Messages), len(h.f.msgs))
	}
	if s := h.stderr.String(); !strings.Contains(s, "may have been created without being recorded") || !strings.Contains(s, "delete it by hand") {
		t.Errorf("stderr %q does not warn about the unrecorded message", s)
	}
	h.checkNoLeaks(t)
}

func TestConfirmRefuses(t *testing.T) {
	for _, c := range []struct {
		name, typed string
		ttyErr      error
		want        string
	}{
		{"no terminal", "", errors.New("device not configured"), "cannot open /dev/tty"},
		{"wrong name", "general\n", nil, "does not match"},
		{"prefix", "harness\n", nil, "does not match"},
		{"nothing typed", "", nil, "no confirmation"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, c.typed)
			if c.ttyErr != nil {
				h.e.openTTY = func() (io.ReadWriteCloser, error) { return nil, c.ttyErr }
			}
			if code := h.run(h.sendArgs(t, "all")...); code != exitFail || !strings.Contains(h.stderr.String(), c.want) {
				t.Errorf("exit %d, stderr %q, want %q", code, h.stderr, c.want)
			}
			if n := h.f.count("POST"); n != 0 {
				t.Errorf("%d POSTs without confirmation", n)
			}
		})
	}
}

func TestTokenFileRefused(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, h *harness) string
		want  string
	}{
		{"group readable", func(t *testing.T, h *harness) string { return h.token(t, 0o640) }, "mode 0640"},
		{"world readable", func(t *testing.T, h *harness) string { return h.token(t, 0o604) }, "mode 0604"},
		{"group writable", func(t *testing.T, h *harness) string { return h.token(t, 0o620) }, "mode 0620"},
		{"directory", func(t *testing.T, h *harness) string { return h.dir }, "not a regular file"},
		{"missing", func(t *testing.T, h *harness) string { return filepath.Join(h.dir, "none") }, "-token-file"},
		{"empty", func(t *testing.T, h *harness) string { return writeMode(t, filepath.Join(h.dir, "t"), " \n", 0o600) }, "empty"},
		{"two words", func(t *testing.T, h *harness) string {
			return writeMode(t, filepath.Join(h.dir, "t"), testToken+" "+testToken, 0o600)
		}, "more than one"},
		{"in a git tree", func(t *testing.T, h *harness) string {
			os.MkdirAll(filepath.Join(h.dir, "repo", ".git"), 0o700)
			return writeMode(t, filepath.Join(h.dir, "repo", "token"), testToken, 0o600)
		}, "inside a git work tree"},
		{"symlink into a git tree", func(t *testing.T, h *harness) string {
			os.MkdirAll(filepath.Join(h.dir, "repo", ".git"), 0o700)
			real := writeMode(t, filepath.Join(h.dir, "repo", "token"), testToken, 0o600)
			link := filepath.Join(h.dir, "link")
			if err := os.Symlink(real, link); err != nil {
				t.Skip(err)
			}
			return link
		}, "inside a git work tree"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, testName+"\n")
			args := h.sendArgs(t, "all")
			args[3] = c.setup(t, h)
			if code := h.run(args...); code != exitFail || !strings.Contains(h.stderr.String(), c.want) {
				t.Errorf("exit %d, stderr %q, want %q", code, h.stderr, c.want)
			}
			if n := len(h.f.requests()); n != 0 {
				t.Errorf("%d requests with a refused token file", n)
			}
			h.checkNoLeaks(t)
		})
	}
}

func writeMode(t *testing.T, path, content string, mode os.FileMode) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOutDirRefused(t *testing.T) {
	h := newHarness(t, testName+"\n")
	os.MkdirAll(filepath.Join(h.dir, "repo", ".git"), 0o700)
	args := h.sendArgs(t, "all")
	args[len(args)-1] = filepath.Join(h.dir, "repo", "out")
	if code := h.run(args...); code != exitFail || !strings.Contains(h.stderr.String(), "-out is inside a git work tree") {
		t.Errorf("exit %d, stderr %q", code, h.stderr)
	}
	if n := len(h.f.requests()); n != 0 {
		t.Errorf("%d requests with a refused -out", n)
	}
}

func TestUsage(t *testing.T) {
	h := newHarness(t, "")
	tok := h.token(t, 0o600)
	pl := h.payloads(t, samplePayloads())
	ok := []string{"-payloads", pl, "-token-file", tok, "-guild", testGuild, "-channel", testChannel, "-phase", "all", "-out", h.out()}
	with := func(i int, v string) []string { a := slices.Clone(ok); a[i] = v; return a }
	for _, c := range []struct {
		name string
		args []string
	}{
		{"no flags", nil},
		{"relative payloads", with(1, "payloads.json")},
		{"relative token", with(3, "token")},
		{"relative out", with(11, "out")},
		{"guild not an ID", with(5, "my-guild")},
		{"channel not an ID", with(7, "123")},
		{"no phase", with(9, "")},
		{"bad phase", with(9, "everything")},
		{"extra argument", append(slices.Clone(ok), "now")},
		{"cleanup with phase", append([]string{"-cleanup"}, ok[2:]...)},
		{"unknown flag", append(slices.Clone(ok), "-base", "https://example.com/")},
	} {
		h.stderr.Reset()
		if code := h.run(c.args...); code != exitUsage {
			t.Errorf("%s: exit %d, want %d (stderr %s)", c.name, code, exitUsage, h.stderr)
		}
	}
	if n := len(h.f.requests()); n != 0 {
		t.Errorf("%d requests on usage errors", n)
	}
}

func TestPayloadsRefused(t *testing.T) {
	good := samplePayloads().Cases
	for _, c := range []struct {
		name string
		pf   any
		want string
	}{
		{"unknown field", map[string]any{"version": 1, "cases": []map[string]any{{"id": "G01", "phase": "grammar", "content": "x", "flags": 0, "predicted": []any{}, "channel": "1"}}}, "unknown field"},
		{"version", payloadFile{Version: 2, Cases: good}, "version 2"},
		{"no cases", payloadFile{Version: 1}, "0 cases"},
		{"flags", payloadFile{Version: 1, Cases: []payloadCase{{ID: "G01", Phase: "grammar", Content: "x", Flags: 1 << 12}}}, "flags 4096"},
		{"empty content", payloadFile{Version: 1, Cases: []payloadCase{{ID: "G01", Phase: "grammar"}}}, "0 bytes"},
		{"long content", payloadFile{Version: 1, Cases: []payloadCase{{ID: "G01", Phase: "grammar", Content: strings.Repeat("x", 2001)}}}, "2001 bytes"},
		{"duplicate", payloadFile{Version: 1, Cases: []payloadCase{good[0], good[0]}}, "duplicate"},
		{"bad id", payloadFile{Version: 1, Cases: []payloadCase{{ID: "G 1", Phase: "grammar", Content: "x"}}}, "bad ID"},
		{"phase", payloadFile{Version: 1, Cases: []payloadCase{{ID: "G01", Phase: "other", Content: "x"}}}, "unknown phase"},
		{"reply forward", payloadFile{Version: 1, Cases: []payloadCase{good[3], good[2]}}, "not an earlier case"},
		{"reply across phases", payloadFile{Version: 1, Cases: []payloadCase{good[0], {ID: "R-1", Phase: "reply", Content: "x", ReplyTo: "G01"}}}, "not an earlier case"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, testName+"\n")
			args := h.sendArgs(t, "all")
			args[1] = h.payloads(t, c.pf)
			if code := h.run(args...); code != exitFail || !strings.Contains(h.stderr.String(), c.want) {
				t.Errorf("exit %d, stderr %q, want %q", code, h.stderr, c.want)
			}
			if n := len(h.f.requests()); n != 0 {
				t.Errorf("%d requests with refused payloads", n)
			}
		})
	}
}

func TestHTTPError(t *testing.T) {
	h := newHarness(t, testName+"\n")
	h.f.postErr = 403
	if code := h.run(h.sendArgs(t, "all")...); code != exitFail {
		t.Fatalf("exit %d", code)
	}
	msg := h.stderr.String()
	if !strings.Contains(msg, "HTTP 403") || !strings.Contains(msg, "Missing Permissions") || strings.Contains(msg, "Authorization") {
		t.Errorf("stderr %q", msg)
	}
	if rec := readRunFile(t, h.out()); len(rec.Messages) != 0 {
		t.Errorf("run.json records %d messages", len(rec.Messages))
	}
	h.checkNoLeaks(t)
}

func TestRedirectRefused(t *testing.T) {
	var elsewhere []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere = append(elsewhere, r.Header.Get("Authorization"))
	}))
	defer other.Close()
	h := newHarness(t, testName+"\n")
	h.e.apiBase = "" // replaced below: every request redirects
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	h.e.apiBase = redirect.URL
	if code := h.run(h.sendArgs(t, "all")...); code != exitFail || !strings.Contains(h.stderr.String(), "redirect refused") {
		t.Errorf("exit %d, stderr %q", code, h.stderr)
	}
	if len(elsewhere) != 0 {
		t.Errorf("the redirect target got %d requests", len(elsewhere))
	}
	h.checkNoLeaks(t)
}

func TestCleanup(t *testing.T) {
	h := newHarness(t, testName+"\n"+testName+"\n")
	if code := h.run(h.sendArgs(t, "all")...); code != exitOK {
		t.Fatalf("send: exit %d, stderr %s", code, h.stderr)
	}
	rec := readRunFile(t, h.out())
	// A message someone else sent to the channel, and one of the run's
	// messages deleted by hand meanwhile.
	h.f.mu.Lock()
	h.f.msgs["300000000000000000"] = &stored{message: message{ID: "300000000000000000", ChannelID: testChannel, Content: "not ours"}}
	h.f.gone[rec.Messages[0].MessageID] = true
	h.f.mu.Unlock()
	before := len(h.f.requests())
	h.stdout.Reset()
	if code := h.run(h.cleanupArgs(t)...); code != exitOK {
		t.Fatalf("cleanup: exit %d, stderr %s", code, h.stderr)
	}
	var deleted []string
	for _, r := range h.f.requests()[before:] {
		if r.method == "POST" {
			t.Errorf("cleanup sent a message")
		}
		if r.method == "DELETE" {
			deleted = append(deleted, r.path)
		}
	}
	var want []string
	for _, m := range rec.Messages {
		want = append(want, "/api/v10/channels/"+testChannel+"/messages/"+m.MessageID)
	}
	if !slices.Equal(deleted, want) {
		t.Errorf("deleted %v, want exactly the run's messages %v", deleted, want)
	}
	if _, ok := h.f.msgs["300000000000000000"]; !ok {
		t.Error("cleanup deleted a message the run did not send")
	}
	if !strings.Contains(h.stdout.String(), "deleted 5 messages (1 were already gone)") {
		t.Errorf("stdout %q", h.stdout)
	}
	for _, m := range readRunFile(t, h.out()).Messages {
		if !m.Deleted {
			t.Errorf("%s not marked deleted", m.Case)
		}
	}
	// A second cleanup has nothing to do and asks nothing.
	before = len(h.f.requests())
	h.stdout.Reset()
	if code := h.run(h.cleanupArgs(t)...); code != exitOK || !strings.Contains(h.stdout.String(), "nothing to delete") {
		t.Errorf("second cleanup: exit %d, stdout %q", code, h.stdout)
	}
	if h.f.count("DELETE") != len(want) {
		t.Error("the second cleanup deleted again")
	}
	h.checkNoLeaks(t)
}

func TestCleanupRefuses(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(rec *runRecord)
		want string
	}{
		{"other bot", func(rec *runRecord) { rec.BotID = "100000000000000007" }, "another bot"},
		{"other channel", func(rec *runRecord) { rec.ChannelID = "100000000000000006" }, "another guild or channel"},
		{"other guild", func(rec *runRecord) { rec.GuildID = "100000000000000005" }, "another guild or channel"},
		{"bad message ID", func(rec *runRecord) { rec.Messages[0].MessageID = "../../guilds/1" }, "no valid message ID"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, testName+"\n")
			if err := os.MkdirAll(h.out(), 0o700); err != nil {
				t.Fatal(err)
			}
			rec := &runRecord{Version: 1, Phase: "all", BotID: testBot, GuildID: testGuild, ChannelID: testChannel,
				Messages: []sentMessage{{Case: "G01", MessageID: "200000000000000001"}}}
			c.edit(rec)
			if err := writeRun(h.out(), rec); err != nil {
				t.Fatal(err)
			}
			if code := h.run(h.cleanupArgs(t)...); code != exitFail || !strings.Contains(h.stderr.String(), c.want) {
				t.Errorf("exit %d, stderr %q, want %q", code, h.stderr, c.want)
			}
			if n := h.f.count("DELETE"); n != 0 {
				t.Errorf("%d DELETEs", n)
			}
		})
	}
	t.Run("no run.json", func(t *testing.T) {
		h := newHarness(t, testName+"\n")
		if code := h.run(h.cleanupArgs(t)...); code != exitFail || len(h.f.requests()) != 0 {
			t.Errorf("exit %d, %d requests", code, len(h.f.requests()))
		}
	})
}

func TestRetryAfter(t *testing.T) {
	hdr := func(v string) http.Header { return http.Header{"Retry-After": {v}} }
	for _, c := range []struct {
		h    http.Header
		body string
		want time.Duration
		ok   bool
	}{
		{hdr("3"), "", 3 * time.Second, true},
		{hdr("2.5"), "", 2500 * time.Millisecond, true},
		{hdr("0.1"), "", sendGap, true},
		{http.Header{}, `{"retry_after": 4.25}`, 4250 * time.Millisecond, true},
		{http.Header{}, `{}`, 0, false},
		{hdr("-1"), "", 0, false},
	} {
		got, ok := retryAfter(c.h, []byte(c.body))
		if got != c.want || ok != c.ok {
			t.Errorf("retryAfter(%v, %s) = %v, %v; want %v, %v", c.h, c.body, got, ok, c.want, c.ok)
		}
	}
}

func TestRateLimitHeaders(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	c := &client{e: &env{now: clock.now, sleep: clock.sleep}}
	c.rateHeaders(http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset-After": {"7.5"}})
	if err := c.wait(context.Background()); err != nil || clock.now() != time.Unix(7, 5e8) {
		t.Errorf("waited until %v, want 7.5 s", clock.now())
	}
}

// TestStdlibOnly keeps the harness free of dependencies, as the tools
// module is, and away from the root module.
func TestStdlibOnly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatal(files, err)
	}
	for _, name := range files {
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range f.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			if first, _, _ := strings.Cut(p, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %s; the harness uses the standard library only", name, p)
			}
		}
	}
}

func TestPrintable(t *testing.T) {
	if got := printable("a\x1b[31mb\u202ec\n" + strings.Repeat("x", 300)); got != "a[31mbc"+strings.Repeat("x", 193)+"…" {
		t.Errorf("printable = %q", got)
	}
}
