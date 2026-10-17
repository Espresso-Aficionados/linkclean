package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/gittest"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
)

// mainNetGuard replaces http.DefaultTransport for the whole test binary: it
// records every request and refuses it, and TestMain fails the run if it
// recorded any. Tests reach HTTP only through Env.HTTP with an httptest
// client.
type mainNetGuard struct {
	mu   sync.Mutex
	uses []string
}

func (g *mainNetGuard) RoundTrip(r *http.Request) (*http.Response, error) {
	g.mu.Lock()
	g.uses = append(g.uses, r.Method+" "+r.URL.String())
	g.mu.Unlock()
	return nil, errors.New("rulegen tests: network use refused: " + r.Method + " " + r.URL.String())
}

func (g *mainNetGuard) recorded() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.uses...)
}

var mainGuard = &mainNetGuard{}

// TestMain refuses the network and isolates git for every test: a
// temporary HOME, no global or system git config, only file:// transport
// (gittest.Process).
func TestMain(m *testing.M) {
	http.DefaultTransport = mainGuard
	cleanup, err := gittest.Process()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	cleanup()
	if uses := mainGuard.recorded(); len(uses) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: tests used the network through http.DefaultTransport:\n\t%s\n", strings.Join(uses, "\n\t"))
		code = 1
	}
	os.Exit(code)
}

// newTestEnv returns an Env for a command under test: output into the
// returned buffers, isolated git, an HTTP client that refuses every request
// (replace it with an httptest server's client), Getenv reading vars, a
// fixed clock, a Sleep that only honours cancellation, and the real go
// command.
func newTestEnv(t testing.TB, vars map[string]string) (e *Env, stdout, stderr *bytes.Buffer) {
	t.Helper()
	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	e = &Env{
		Stdout:  stdout,
		Stderr:  stderr,
		Git:     repo.Exec{Env: gittest.Isolate(t)},
		HTTP:    &http.Client{Transport: &mainNetGuard{}},
		APIBase: "http://api.invalid",
		Getenv:  func(k string) string { return vars[k] },
		Now:     func() time.Time { return time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC) },
		Sleep:   func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		Go:      goCommand,
	}
	return e, stdout, stderr
}

func TestRun(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{"no_args", nil, exitUsage, "", "usage: rulegen"},
		{"unknown", []string{"nope"}, exitUsage, "", `unknown command "nope"`},
		{"help", []string{"help"}, exitOK, "  help\n", ""},
		{"dash_h", []string{"-h"}, exitOK, "  help\n", ""},
		{"help_args", []string{"help", "x"}, exitUsage, "", "unexpected arguments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, stdout, stderr := newTestEnv(t, nil)
			if code := run(ctx, e, tt.args); code != tt.code {
				t.Errorf("exit %d, want %d", code, tt.code)
			}
			if !strings.Contains(stdout.String(), tt.stdout) || tt.stdout == "" && stdout.Len() > 0 {
				t.Errorf("stdout %q, want %q", stdout, tt.stdout)
			}
			if !strings.Contains(stderr.String(), tt.stderr) || tt.stderr == "" && stderr.Len() > 0 {
				t.Errorf("stderr %q, want %q", stderr, tt.stderr)
			}
		})
	}
}

func TestHelpListsCommands(t *testing.T) {
	e, stdout, _ := newTestEnv(t, nil)
	if code := run(context.Background(), e, []string{"help"}); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	var listed []string
	for line := range strings.SplitSeq(stdout.String(), "\n") {
		if name, ok := strings.CutPrefix(line, "  "); ok {
			listed = append(listed, name)
		}
	}
	if len(listed) != len(commands) {
		t.Errorf("help lists %q; registered %d commands", listed, len(commands))
	}
	for i, n := range listed {
		if commands[n] == nil {
			t.Errorf("help lists unregistered %q", n)
		}
		if i > 0 && listed[i-1] >= n {
			t.Errorf("help not sorted: %q", listed)
		}
	}
}

func TestRegister(t *testing.T) {
	const name = "maintest-dispatch"
	var got []string
	register(name, func(_ context.Context, e *Env, args []string) int {
		got = args
		return 7
	})
	defer delete(commands, name)
	e, _, _ := newTestEnv(t, nil)
	if code := run(context.Background(), e, []string{name, "-x", "y"}); code != 7 || strings.Join(got, " ") != "-x y" {
		t.Errorf("dispatch: exit %d, args %q", code, got)
	}
	defer func() {
		if recover() == nil {
			t.Error("duplicate register did not panic")
		}
	}()
	register(name, nil)
}

func TestFlags(t *testing.T) {
	tests := []struct {
		args []string
		code int
		ok   bool
	}{
		{[]string{"-root", "r"}, exitOK, true},
		{[]string{"-h"}, exitOK, false},
		{[]string{"-nope"}, exitUsage, false},
		{[]string{"-root", "r", "extra"}, exitUsage, false},
	}
	for _, tt := range tests {
		e, _, _ := newTestEnv(t, nil)
		fs := newFlags(e, "x")
		fs.String("root", "", "")
		if code, ok := parseFlags(fs, tt.args); code != tt.code || ok != tt.ok {
			t.Errorf("parseFlags(%q) = %d, %v; want %d, %v", tt.args, code, ok, tt.code, tt.ok)
		}
	}
	e, _, stderr := newTestEnv(t, nil)
	fs := newFlags(e, "x")
	fs.String("root", "", "")
	fs.String("out", "o", "")
	parseFlags(fs, nil)
	if requireFlags(fs, "root", "out") || !strings.Contains(stderr.String(), "rulegen x: -root is required") || strings.Contains(stderr.String(), "-out") {
		t.Errorf("requireFlags: %q", stderr)
	}
}

func TestNetworkGuard(t *testing.T) {
	if http.DefaultTransport != mainGuard {
		t.Fatal("http.DefaultTransport is not the guard")
	}
	if c := defaultEnv().HTTP; c.Transport != nil {
		t.Error("defaultEnv's HTTP client bypasses http.DefaultTransport")
	}
	g := &mainNetGuard{}
	req, _ := http.NewRequest("GET", "https://example.invalid/x", nil)
	if _, err := (&http.Client{Transport: g}).Do(req); err == nil || !strings.Contains(err.Error(), "network use refused") {
		t.Errorf("guard error = %v", err)
	}
	if uses := g.recorded(); len(uses) != 1 || uses[0] != "GET https://example.invalid/x" {
		t.Errorf("recorded %q", uses)
	}
}

func TestGitIsolation(t *testing.T) {
	for k, want := range map[string]string{
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_ALLOW_PROTOCOL":  "file",
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s=%q, want %q", k, got, want)
		}
	}
	home := os.Getenv("HOME")
	if !strings.HasPrefix(filepath.Base(home), "rulegen-home-") {
		t.Errorf("HOME=%q is not the temporary home", home)
	}
	// The process-wide isolation also covers defaultEnv's git.
	ctx := context.Background()
	if out, err := defaultEnv().Git.Run(ctx, "", nil, "config", "--global", "--list"); err == nil && len(bytes.TrimSpace(out)) > 0 {
		t.Errorf("global git config visible: %s", out)
	}
	if _, err := defaultEnv().Git.Run(ctx, t.TempDir(), nil, "ls-remote", "https://github.com/brave/adblock-lists"); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("https transport: %v, want a refusal", err)
	}
	// go still finds its caches.
	out, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil || strings.HasPrefix(string(out), home) {
		t.Errorf("GOMODCACHE %q under the temporary HOME (%v)", out, err)
	}
}

func TestDefaultEnv(t *testing.T) {
	e := defaultEnv()
	if e.Stdout == nil || e.Stderr == nil || e.Git == nil || e.Getenv == nil || e.Now == nil || e.APIBase != "https://api.github.com" {
		t.Errorf("defaultEnv = %+v", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := e.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) || time.Since(start) > time.Minute {
		t.Errorf("Sleep on a cancelled context: %v", err)
	}
	if err := e.Sleep(context.Background(), time.Millisecond); err != nil {
		t.Errorf("Sleep: %v", err)
	}
	stdout, _, err := e.Go(context.Background(), t.TempDir(), []string{"GOFLAGS="}, "env", "GOFLAGS")
	if err != nil || strings.TrimSpace(string(stdout)) != "" {
		t.Errorf("Go env GOFLAGS = %q, %v", stdout, err)
	}
	if r := e.Repo("/x"); r.Dir != "/x" || r.Git == nil {
		t.Errorf("Repo = %+v", r)
	}
}
