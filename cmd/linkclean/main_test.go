package main

// Black-box contract tests: they call only run, env and moduleVersion.
// Byte-exact rows use only inputs that overlay rules decide, so a Brave data
// refresh cannot change them; everything else is compared with the library.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
	"unicode/utf8"

	"github.com/Espresso-Aficionados/linkclean"
)

const (
	ctAmazon      = "https://www.amazon.com/dp/B0?tag=foo-20"
	ctAmazonStrip = "https://www.amazon.com/dp/B0"
	ctGoogle      = "https://www.google.com/url?q=https%3A%2F%2Fexample.com%2F"
	ctGoogleOut   = "https://example.com/"

	ctKeepRule   = "linkclean/overlay:amazon-affiliate-keep"
	ctStripRule  = "linkclean/overlay:amazon-affiliate-strip"
	ctGoogleRule = "linkclean/overlay:google-url"

	ctMaxLine = 1 << 20
)

// The record bodies of the overlay inputs, after "line <n>\n" (-explain) or
// after `{"line":<n>,"error":"",` (-json).
const (
	ctGoogleExplain = `out "https://example.com/"` + "\n" +
		"changed\n" +
		`hop category=redirect rule="` + ctGoogleRule + `" host="www.google.com"` + "\n\n"
	ctAmazonKeepExplain = `out "` + ctAmazon + `"` + "\n" +
		`kept reason=affiliate name="tag" rule="` + ctKeepRule + `"` + "\n\n"
	ctAmazonStripExplain = `out "` + ctAmazonStrip + `"` + "\n" +
		"changed\n" +
		`removed part=query category=affiliate name="tag" rule="` + ctStripRule + `" network="Amazon Associates" id="foo-20"` + "\n\n"

	ctGoogleJSON = `"url":"https://example.com/","changed":true,"skipped":"","abandoned":false,"signed":false,` +
		`"hops":[{"category":"redirect","rule":"` + ctGoogleRule + `","fromHost":"www.google.com","network":"","id":""}],` +
		`"removed":[],"kept":[]}`
	ctAmazonKeepJSON = `"url":"` + ctAmazon + `","changed":false,"skipped":"","abandoned":false,"signed":false,` +
		`"hops":[],"removed":[],"kept":[{"name":"tag","reason":"affiliate","rule":"` + ctKeepRule + `"}]}`
	ctAmazonStripJSON = `"url":"` + ctAmazonStrip + `","changed":true,"skipped":"","abandoned":false,"signed":false,` +
		`"hops":[],"removed":[{"part":"query","category":"affiliate","name":"tag","nestedIn":"",` +
		`"rule":"` + ctStripRule + `","network":"Amazon Associates","id":"foo-20"}],"kept":[]}`
)

var ctErrBoom = errors.New("boom")

func ctExplain(line int, body string) string { return "line " + strconv.Itoa(line) + "\n" + body }

func ctJSON(line int, body string) string {
	return `{"line":` + strconv.Itoa(line) + `,"error":"",` + body + "\n"
}

func ctJSONError(line int, msg string) string {
	return `{"line":` + strconv.Itoa(line) + `,"error":` + strconv.Quote(msg) + "}\n"
}

// ctResult is the observable outcome of one run.
type ctResult struct {
	stdout, stderr string
	code           int
}

func ctRunWith(stdin io.Reader, stdout io.Writer, args ...string) (string, int) {
	var errb bytes.Buffer
	code := run(env{Stdin: stdin, Stdout: stdout, Stderr: &errb}, args)
	return errb.String(), code
}

func ctRun(stdin io.Reader, args ...string) ctResult {
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	var out bytes.Buffer
	errs, code := ctRunWith(stdin, &out, args...)
	return ctResult{out.String(), errs, code}
}

// ctShort abbreviates long strings in failure messages.
func ctShort(s string) string {
	if len(s) <= 300 {
		return strconv.Quote(s)
	}
	return fmt.Sprintf("%q...(%d bytes)...%q", s[:120], len(s), s[len(s)-120:])
}

func ctCheck(t *testing.T, got ctResult, wantOut, wantErr string, wantCode int) {
	t.Helper()
	if got.code != wantCode {
		t.Errorf("exit code = %d, want %d (stderr %s)", got.code, wantCode, ctShort(got.stderr))
	}
	if got.stdout != wantOut {
		t.Errorf("stdout =\n%s\nwant\n%s", ctShort(got.stdout), ctShort(wantOut))
	}
	if got.stderr != wantErr {
		t.Errorf("stderr = %s, want %s", ctShort(got.stderr), ctShort(wantErr))
	}
}

// ctAssertTerminalSafe fails if s is not valid UTF-8 or holds a rune that
// strconv.IsPrint rejects, other than '\n'.
func ctAssertTerminalSafe(t *testing.T, what, s string) {
	t.Helper()
	for i, r := range s {
		if r == utf8.RuneError {
			if _, size := utf8.DecodeRuneInString(s[i:]); size == 1 {
				t.Errorf("%s: invalid UTF-8 byte %#x at %d", what, s[i], i)
				continue
			}
		}
		if r != '\n' && !strconv.IsPrint(r) {
			t.Errorf("%s: raw non-printable rune %U at %d", what, r, i)
		}
	}
}

// ctUnreadable fails the test if run reads stdin.
type ctUnreadable struct{ t *testing.T }

func (u ctUnreadable) Read([]byte) (int, error) {
	u.t.Error("stdin was read although URL arguments were given")
	return 0, io.EOF
}

// ctFailWriter accepts n bytes, then fails every write with ctErrBoom.
type ctFailWriter struct{ n int }

func (w *ctFailWriter) Write(p []byte) (int, error) {
	if len(p) <= w.n {
		w.n -= len(p)
		return len(p), nil
	}
	k := w.n
	w.n = 0
	return k, ctErrBoom
}

// TestRun: invocation forms, trimming, line handling per mode, the affiliate
// policy and invalid UTF-8, end to end. Every row runs twice and must give the
// same result.
func TestRun(t *testing.T) {
	const (
		c1   = "https://example.com/\x9b31m?utm_source=x"      // raw C1 byte
		bidi = "https://ex\xe2\x80\xaeample.com/?utm_source=x" // U+202E
		bad  = "https://example.com/\xff?a=1"                  // invalid UTF-8
	)
	clean := linkclean.Clean
	type row struct {
		name     string
		args     []string
		stdin    string
		noStdin  bool // stdin must not be read
		readErr  bool // stdin fails with ctErrBoom after its text
		out, err string
		code     int
	}
	rows := []row{
		// Plain mode, arguments.
		{name: "arg keep is the default", args: []string{ctAmazon}, out: ctAmazon + "\n"},
		{name: "arg strip", args: []string{"-affiliate", "strip", ctAmazon}, out: ctAmazonStrip + "\n"},
		{name: "arg keep explicit", args: []string{"-affiliate", "keep", ctAmazon}, out: ctAmazon + "\n"},
		{name: "double-dash flag form", args: []string{"--affiliate=strip", ctAmazon}, out: ctAmazonStrip + "\n"},
		{name: "args in order", args: []string{ctGoogle, ctAmazon, "mailto:a@b"},
			out: ctGoogleOut + "\n" + ctAmazon + "\n" + "mailto:a@b\n"},
		{name: "args ignore stdin", args: []string{ctGoogle}, noStdin: true, out: ctGoogleOut + "\n"},
		{name: "arg trimmed", args: []string{"  " + ctAmazon + "  "}, out: ctAmazon + "\n"},
		{name: "arg trimmed strip", args: []string{"-affiliate", "strip", " \t" + ctAmazon + "\r\n"}, out: ctAmazonStrip + "\n"},
		{name: "empty arg", args: []string{""}, out: "\n"},
		{name: "-- then flag-like", args: []string{"--", "-json"}, out: clean("-json") + "\n"},
		{name: "-- then lone dash", args: []string{"--", "-"}, out: clean("-") + "\n"},
		{name: "-- then URL", args: []string{"-affiliate", "strip", "--", ctAmazon, "-explain"},
			out: ctAmazonStrip + "\n" + clean("-explain") + "\n"},
		{name: "C1 passes through", args: []string{c1}, out: clean(c1) + "\n"},

		// Plain mode, stdin.
		{name: "stdin lines", stdin: ctGoogle + "\n\n# x\nmailto:a@b\n  " + ctAmazon + "\r\n",
			out: ctGoogleOut + "\n\n# x\nmailto:a@b\n" + ctAmazon + "\n"},
		{name: "stdin strip crlf", args: []string{"-affiliate", "strip"}, stdin: "  " + ctAmazon + "\r\n" + ctGoogle + "\r\n",
			out: ctAmazonStrip + "\n" + ctGoogleOut + "\n"},
		{name: "stdin final line without newline", stdin: ctAmazon + "\n" + ctGoogle, out: ctAmazon + "\n" + ctGoogleOut + "\n"},
		{name: "stdin empty", stdin: "", out: ""},
		{name: "stdin one empty line", stdin: "\n", out: "\n"},
		{name: "stdin blank lines", stdin: " \t \n\r\n", out: "\n\n"},
		{name: "stdin bidi passes through", stdin: bidi + "\n", out: clean(bidi) + "\n"},
		{name: "stdin invalid UTF-8 is byte-faithful", stdin: bad + "\n", out: clean(bad) + "\n"},
		{name: "stdin read error", stdin: ctGoogle + "\n", readErr: true,
			out: ctGoogleOut + "\n", err: "linkclean: reading stdin: boom\n", code: 2},

		// -explain.
		{name: "explain arg", args: []string{"-explain", ctGoogle}, out: ctExplain(1, ctGoogleExplain)},
		{name: "explain keep", args: []string{"-explain", ctAmazon}, out: ctExplain(1, ctAmazonKeepExplain)},
		{name: "explain strip shows the affiliate id", args: []string{"-explain", "-affiliate", "strip", ctAmazon},
			out: ctExplain(1, ctAmazonStripExplain)},
		{name: "explain args numbered", args: []string{"-explain", "mailto:a@b", ctGoogle},
			out: ctExplain(1, "out \"mailto:a@b\"\nskipped unparseable\n\n") + ctExplain(2, ctGoogleExplain)},
		{name: "explain other scheme", args: []string{"-explain", "ftp://example.com/"},
			out: ctExplain(1, "out \"ftp://example.com/\"\nskipped scheme\n\n")},
		{name: "explain -- flag-like", args: []string{"-explain", "--", "-json"},
			out: ctExplain(1, "out \"-json\"\nskipped unparseable\n\n")},
		{name: "explain stdin skips blank and comment lines", args: []string{"-explain"}, stdin: "\n# x\n" + ctGoogle + "\n  \n  # y\r\nmailto:a@b",
			out: ctExplain(3, ctGoogleExplain) + ctExplain(6, "out \"mailto:a@b\"\nskipped unparseable\n\n")},
		{name: "explain stdin only comments", args: []string{"-explain"}, stdin: "# a\n\n", out: ""},
		{name: "explain quotes invalid UTF-8", args: []string{"-explain"}, stdin: "https://example.com/\xff\n",
			out: ctExplain(1, "out \"https://example.com/\\xff\"\n\n")},
		{name: "explain read error", args: []string{"-explain"}, stdin: ctGoogle + "\n", readErr: true,
			out: ctExplain(1, ctGoogleExplain), err: "linkclean: reading stdin: boom\n", code: 2},

		// -json.
		{name: "json arg", args: []string{"-json", ctGoogle}, out: ctJSON(1, ctGoogleJSON)},
		{name: "json double-dash form", args: []string{"--json", ctGoogle}, out: ctJSON(1, ctGoogleJSON)},
		{name: "explain json equals json", args: []string{"-explain", "-json", ctGoogle}, out: ctJSON(1, ctGoogleJSON)},
		{name: "json keep", args: []string{"-json", ctAmazon}, out: ctJSON(1, ctAmazonKeepJSON)},
		{name: "json strip shows the affiliate id", args: []string{"-json", "-affiliate", "strip", ctAmazon},
			out: ctJSON(1, ctAmazonStripJSON)},
		{name: "json stdin skips blank and comment lines", args: []string{"-json"}, stdin: "# c\n\nmailto:a@b\n" + ctGoogle,
			out: ctJSON(3, `"url":"mailto:a@b","changed":false,"skipped":"unparseable","abandoned":false,"signed":false,"hops":[],"removed":[],"kept":[]}`) +
				ctJSON(4, ctGoogleJSON)},
		{name: "json invalid url is an error object", args: []string{"-json"}, stdin: bad + "\n" + ctGoogle + "\n",
			out: ctJSONError(1, "not valid UTF-8") + ctJSON(2, ctGoogleJSON), code: 1},
		{name: "json invalid id is %XX", args: []string{"-json", "-affiliate", "strip", "https://www.amazon.com/dp/B0?tag=%FF"},
			out: ctJSON(1, strings.Replace(ctAmazonStripJSON, `"id":"foo-20"`, `"id":"%FF"`, 1))},
		{name: "json read error", args: []string{"-json"}, stdin: ctGoogle + "\n", readErr: true,
			out: ctJSON(1, ctGoogleJSON), err: "linkclean: reading stdin: boom\n", code: 2},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			once := func() ctResult {
				var in io.Reader = strings.NewReader(r.stdin)
				switch {
				case r.noStdin:
					in = ctUnreadable{t}
				case r.readErr:
					in = io.MultiReader(in, iotest.ErrReader(ctErrBoom))
				}
				return ctRun(in, r.args...)
			}
			got := once()
			ctCheck(t, got, r.out, r.err, r.code)
			for _, a := range r.args {
				if a == "-explain" || a == "-json" || a == "--json" {
					ctAssertTerminalSafe(t, "stdout", got.stdout)
					break
				}
				if a == "--" {
					break
				}
			}
			// The output is a pure function of the arguments and the input.
			if again := once(); again != got {
				t.Errorf("second run differs: %+v, first %+v", again, got)
			}
		})
	}
}

// TestRunTerminalSafe: -explain and -json never write a raw control, bidi or
// invalid UTF-8 byte, whatever the input holds.
func TestRunTerminalSafe(t *testing.T) {
	inputs := []string{
		"https://example.com/\x9b31m?utm_source=x",
		"https://ex\xe2\x80\xaeample.com/?utm_source=x",
		"https://example.com/\xc2\x9b?\xe2\x80\xae=1&utm_source=x",
		"https://example.com/\xff?a=1",
		"https://example.com/\x1b[31m?a=1",
		"https://example.com/a\x7fb?utm_source=1", // DEL: ambiguous, url unchanged
		"https://example.com/?a=1&\xe2\x80\xa8=2#\xf3\xa0\x80\x81",
	}
	stdin := strings.Join(inputs, "\n") + "\n"
	for _, mode := range [][]string{{"-explain"}, {"-json"}, {"-explain", "-affiliate", "strip"}, {"-json", "-affiliate", "strip"}} {
		got := ctRun(strings.NewReader(stdin), mode...)
		if got.code != 0 && got.code != 1 {
			t.Errorf("%v: exit code %d, stderr %q", mode, got.code, got.stderr)
		}
		ctAssertTerminalSafe(t, fmt.Sprint(mode), got.stdout)
		if mode[0] == "-json" {
			if n := strings.Count(got.stdout, "\n"); n != len(inputs) {
				t.Errorf("%v: %d lines, want one per input (%d)", mode, n, len(inputs))
			}
		} else if n := strings.Count(got.stdout, "\nline "); n != len(inputs)-1 {
			t.Errorf("%v: %d records, want %d", mode, n+1, len(inputs))
		}
	}
}

// ctHostileID is an affiliate ID holding a bidi override, a C1 control, ESC,
// an invalid UTF-8 byte, DEL and a newline, as Beneficiary.ID decodes it from
// ctHostile (an all-ASCII URL, so the engine cleans it).
const (
	ctHostile   = "https://www.amazon.com/dp/B0?tag=x%E2%80%AE%C2%9B%1B%FF%7F%0Ay"
	ctHostileID = "x\xe2\x80\xae\xc2\x9b\x1b\xff\x7f\ny"
)

// TestAffiliateIDEscaped: the affiliate ID is shown under
// -affiliate strip in -explain and -json, escaped, and no raw control or bidi
// byte of it reaches stdout.
func TestAffiliateIDEscaped(t *testing.T) {
	// The premise: the engine decodes the ID as above.
	strip, err := linkclean.New(linkclean.Config{Affiliate: linkclean.AffiliateStrip})
	if err != nil {
		t.Fatal(err)
	}
	res := strip.Explain(ctHostile)
	if len(res.Removed) != 1 || res.Removed[0].Beneficiary.ID != ctHostileID || res.URL != ctAmazonStrip {
		t.Fatalf("premise: Explain(%q) = %+v, want one removal with ID %q", ctHostile, res, ctHostileID)
	}

	t.Run("explain", func(t *testing.T) {
		got := ctRun(nil, "-explain", "-affiliate", "strip", ctHostile)
		want := ctExplain(1, strings.Replace(ctAmazonStripExplain, `id="foo-20"`, "id="+strconv.Quote(ctHostileID), 1))
		ctCheck(t, got, want, "", 0)
		ctAssertTerminalSafe(t, "stdout", got.stdout)
		if !strings.Contains(got.stdout, `id="x\u202e\u009b\x1b\xff\x7f\ny"`) {
			t.Errorf("stdout lacks the strconv.Quote form of the ID: %q", got.stdout)
		}
	})
	t.Run("json", func(t *testing.T) {
		got := ctRun(strings.NewReader(ctHostile+"\n"), "-json", "-affiliate", "strip")
		if got.code != 0 || got.stderr != "" {
			t.Errorf("exit code %d, stderr %q; want 0 and none", got.code, got.stderr)
		}
		ctAssertTerminalSafe(t, "stdout", got.stdout)
		if strings.Count(got.stdout, "\n") != 1 || !strings.HasSuffix(got.stdout, "\n") {
			t.Fatalf("want exactly one JSON line, got %q", got.stdout)
		}
		low := strings.ToLower(got.stdout)
		for _, esc := range []string{`\u202e`, `\u009b`, `\u001b`, `%ff`, `\u007f`, `\n`} {
			if !strings.Contains(low, esc) {
				t.Errorf("stdout lacks the escape %s: %q", esc, got.stdout)
			}
		}
		var rec struct {
			Line    int    `json:"line"`
			Error   string `json:"error"`
			URL     string `json:"url"`
			Removed []struct {
				Category, Network, ID string
			} `json:"removed"`
		}
		if err := json.Unmarshal([]byte(got.stdout), &rec); err != nil {
			t.Fatalf("decode %q: %v", got.stdout, err)
		}
		wantID := "x\xe2\x80\xae\xc2\x9b\x1b%FF\x7f\ny" // the invalid byte as %XX; the rest decodes back
		if rec.Line != 1 || rec.Error != "" || rec.URL != ctAmazonStrip || len(rec.Removed) != 1 ||
			rec.Removed[0].Category != "affiliate" || rec.Removed[0].Network != "Amazon Associates" ||
			rec.Removed[0].ID != wantID {
			t.Errorf("record = %+v, want url %q and one affiliate removal with id %q", rec, ctAmazonStrip, wantID)
		}
	})
	t.Run("keep shows no id", func(t *testing.T) {
		for _, mode := range []string{"-explain", "-json"} {
			got := ctRun(nil, mode, ctHostile)
			if got.code != 0 || strings.Contains(got.stdout, "id=") || strings.Contains(got.stdout, `"id"`) {
				t.Errorf("%s under keep: code %d, stdout %q; want 0 and no id", mode, got.code, got.stdout)
			}
			ctAssertTerminalSafe(t, mode, got.stdout)
		}
	})
}

// TestRunWriteError: a failing stdout gives exit 2 and one message, in
// every mode.
func TestRunWriteError(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		stdin string
	}{
		{"plain args", []string{ctGoogle, ctAmazon}, ""},
		{"plain stdin", nil, strings.Repeat(ctGoogle+"\n", 3000)},
		{"explain", []string{"-explain", ctGoogle}, ""},
		{"json", []string{"-json"}, ctGoogle + "\n"},
		{"check pass", []string{"-check"}, "X1 https://example.com/\n"},
		{"check fail", []string{"-check"}, "X1 " + ctAmazon + "\n"},
		{"licenses", []string{"-licenses"}, ""},
		{"version", []string{"-version"}, ""},
	}
	for _, c := range cases {
		for _, n := range []int{0, 7} {
			t.Run(fmt.Sprintf("%s/after %d bytes", c.name, n), func(t *testing.T) {
				errs, code := ctRunWith(strings.NewReader(c.stdin), &ctFailWriter{n: n}, c.args...)
				if code != 2 || errs != "linkclean: writing output: boom\n" {
					t.Errorf("code %d, stderr %q; want 2 and %q", code, errs, "linkclean: writing output: boom\n")
				}
			})
		}
	}
}

// ctSyncBuffer is a bytes.Buffer safe for one writer and one poller.
type ctSyncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *ctSyncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *ctSyncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestRunStreams: a result is written once its line is complete, before run
// blocks reading more input, so interactive or tail -f use sees it before the
// next line arrives. That holds when the next line is already partly read
// (a producer or the pipe delivered part of it with the complete line), and
// when that next line is over the cap and being copied or discarded.
func TestRunStreams(t *testing.T) {
	tails := []struct {
		name, tail string
		code       int // after the tail's line ends at EOF
	}{
		{"whole lines", "", 0},
		{"partial next line", "https://partial.example/", 0},
		{"partial long line", strings.Repeat("a", ctMaxLine+10), 1},
	}
	for _, mode := range [][]string{nil, {"-explain"}, {"-json"}} {
		for _, tt := range tails {
			t.Run(fmt.Sprint(mode)+"/"+tt.name, func(t *testing.T) {
				pr, pw := io.Pipe()
				out := &ctSyncBuffer{}
				done := make(chan int, 1)
				go func() {
					_, code := ctRunWith(pr, out, mode...)
					done <- code
				}()
				wrote := make(chan error, 1)
				go func() {
					_, err := io.WriteString(pw, ctGoogle+"\n"+tt.tail)
					wrote <- err
				}()
				for deadline := time.Now().Add(10 * time.Second); !strings.Contains(out.String(), ctGoogleOut); {
					if time.Now().After(deadline) {
						pw.Close()
						<-done
						t.Fatal("no output for a complete line while stdin stays open")
					}
					time.Sleep(time.Millisecond)
				}
				if err := <-wrote; err != nil {
					t.Fatal(err)
				}
				pw.Close()
				if code := <-done; code != tt.code {
					t.Errorf("exit code %d, want %d", code, tt.code)
				}
			})
		}
	}
}

// ctUsageSynopsis is the fixed first part of the usage text.
const ctUsageSynopsis = "usage: linkclean [-affiliate keep|strip] [-explain | -json] [--] [URL ...]\n" +
	"       linkclean -check < cases\n" +
	"       linkclean -licenses | -version\n" +
	"\n"

// TestRunUsage: flag compatibility, misplaced flags and help.
func TestRunUsage(t *testing.T) {
	misplaced := func(arg string) string {
		return fmt.Sprintf("linkclean: flag-like argument %q after URL arguments; put flags first, or use -- before URLs\n", arg)
	}
	rows := []struct {
		args []string
		err  string // exact stderr if not ""
	}{
		{args: []string{"-explain", "-check"}},
		{args: []string{"-json", "-check"}},
		{args: []string{"-explain", "-licenses"}},
		{args: []string{"-explain", "-version"}},
		{args: []string{"-json", "-licenses"}},
		{args: []string{"-json", "-version"}},
		{args: []string{"-affiliate", "strip", "-check"}},
		{args: []string{"-affiliate", "keep", "-check"}},
		{args: []string{"-check", "-affiliate=keep"}},
		{args: []string{"-affiliate", "strip", "-licenses"}},
		{args: []string{"-affiliate", "keep", "-licenses"}},
		{args: []string{"-affiliate", "strip", "-version"}},
		{args: []string{"-check", "-licenses"}},
		{args: []string{"-check", "-version"}},
		{args: []string{"-licenses", "-version"}},
		{args: []string{"-bogus"}},
		{args: []string{"-bogus", ctAmazon}},
		{args: []string{"-affiliate", "bogus", ctAmazon}},
		{args: []string{"-affiliate", "Strip", ctAmazon}},
		{args: []string{"-affiliate", "", ctAmazon}},
		{args: []string{"-affiliate", "--", ctAmazon}},
		{args: []string{"-affiliate"}},
		{args: []string{"-explain=maybe", ctAmazon}},
		{args: []string{"-check", "x"}},
		{args: []string{"-check", "--", "x"}},
		{args: []string{"-licenses", "x"}},
		{args: []string{"-licenses", "--", "x"}},
		{args: []string{"-version", "x"}},
		{args: []string{"https://x", "-json"}, err: misplaced("-json")},
		{args: []string{ctAmazon, "-affiliate", "strip"}, err: misplaced("-affiliate")},
		{args: []string{"-explain", ctAmazon, "--json"}, err: misplaced("--json")},
		{args: []string{ctAmazon, "--", ctGoogle}, err: misplaced("--")},
		{args: []string{"-"}, err: misplaced("-")},
		{args: []string{ctAmazon, "-"}, err: misplaced("-")},
	}
	for _, r := range rows {
		t.Run(strings.Join(r.args, " "), func(t *testing.T) {
			got := ctRun(strings.NewReader(ctAmazon+"\n"), r.args...)
			if got.code != 64 {
				t.Errorf("exit code %d, want 64 (stdout %q, stderr %q)", got.code, got.stdout, got.stderr)
			}
			if got.stdout != "" {
				t.Errorf("stdout = %q, want empty", got.stdout)
			}
			if r.err != "" && got.stderr != r.err {
				t.Errorf("stderr = %q, want %q", got.stderr, r.err)
			}
			if got.stderr == "" {
				t.Error("stderr is empty, want a usage error")
			}
		})
	}

	// -h/-help print the usage to stderr and exit 0. The bytes of
	// PrintDefaults are not pinned: they vary between Go releases.
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	for _, h := range []string{"-h", "-help", "--help"} {
		t.Run(h, func(t *testing.T) {
			got := ctRun(nil, h)
			if got.code != 0 || got.stdout != "" {
				t.Errorf("code %d, stdout %q; want 0 and empty", got.code, got.stdout)
			}
			if !strings.HasPrefix(got.stderr, ctUsageSynopsis) {
				t.Errorf("usage does not start with the synopsis:\n%s", got.stderr)
			}
			for _, s := range []string{
				"It never uses the network and does not resolve short links.",
				"put -- before URL arguments that may start with \"-\".",
				"use -explain to view untrusted input on a terminal.",
				// What -explain and -json show.
				"never the input or the removed values",
				"affiliate IDs are shown",
				"Exit status: 0 success; 1 a -check case changed, a -explain/-json input could not be reported, " +
					"or a line over 1 MiB was copied unchanged; 2 an input, output or rule-data error; 64 a usage error.",
			} {
				if !strings.Contains(norm(got.stderr), s) {
					t.Errorf("usage lacks %q", s)
				}
			}
			if !strings.Contains(got.stderr, "\nflags:\n") {
				t.Error(`usage lacks the "flags:" line`)
			}
			for _, f := range []string{"affiliate", "explain", "json", "check", "licenses", "version"} {
				if !strings.Contains(got.stderr, "-"+f) {
					t.Errorf("usage does not name -%s", f)
				}
			}
		})
	}
}

// ctCases returns the URLs of the golden files: every negative.txt case and
// every positive.txtar in: line.
func ctCases(t *testing.T) []string {
	t.Helper()
	var urls []string
	neg, err := os.ReadFile("../../testdata/golden/negative.txt")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(neg), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		_, u, _ := strings.Cut(line, " ")
		urls = append(urls, u)
	}
	pos, err := os.Open("../../testdata/golden/positive.txtar")
	if err != nil {
		t.Fatal(err)
	}
	defer pos.Close()
	sc := bufio.NewScanner(pos)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "in:"); ok {
			urls = append(urls, strings.TrimPrefix(v, " "))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(urls) < 150 {
		t.Fatalf("found %d golden URLs, want at least 150", len(urls))
	}
	return urls
}

// TestPlainMatchesLibrary: plain output line i is Clean of input line i,
// under both policies, for every golden URL.
func TestPlainMatchesLibrary(t *testing.T) {
	urls := ctCases(t)
	for _, p := range []struct {
		name string
		args []string
		cfg  linkclean.Config
	}{
		{"keep", nil, linkclean.Config{}},
		{"strip", []string{"-affiliate", "strip"}, linkclean.Config{Affiliate: linkclean.AffiliateStrip}},
	} {
		t.Run(p.name, func(t *testing.T) {
			c, err := linkclean.New(p.cfg)
			if err != nil {
				t.Fatal(err)
			}
			got := ctRun(strings.NewReader(strings.Join(urls, "\n")+"\n"), p.args...)
			if got.code != 0 || got.stderr != "" {
				t.Fatalf("code %d, stderr %q", got.code, got.stderr)
			}
			lines := strings.Split(strings.TrimSuffix(got.stdout, "\n"), "\n")
			if len(lines) != len(urls) {
				t.Fatalf("%d output lines for %d inputs", len(lines), len(urls))
			}
			changed := 0
			for i, u := range urls {
				want := c.Clean(strings.TrimSpace(u))
				if lines[i] != want {
					t.Errorf("line %d: %q -> %q, want %q", i+1, u, lines[i], want)
				}
				if want != u {
					changed++
				}
			}
			if changed == 0 {
				t.Error("no golden URL changed: the comparison is vacuous")
			}
		})
	}
}

// ctLong returns an http URL of exactly n bytes, longer than MaxURLLength.
func ctLong(n int) string {
	const p = "https://example.com/?a="
	return p + strings.Repeat("a", n-len(p))
}

// TestLongLines: the 1 MiB line cap.
func TestLongLines(t *testing.T) {
	exact, over := ctLong(ctMaxLine), ctLong(ctMaxLine+1)
	nine, big := ctLong(9000), ctLong(100<<10)
	copied := func(n int) string {
		return fmt.Sprintf("linkclean: stdin:%d: line longer than 1048576 bytes, copied unchanged\n", n)
	}
	// A line over the cap with '\r' bytes inside it, at every 4096th byte, so
	// some sit at the end of any power-of-two read chunk: none is a
	// terminator, so all are copied.
	cr := []byte(over)
	for i := 4095; i < len(cr)-1; i += 4096 {
		cr[i] = '\r'
	}
	crs := string(cr)
	tooLong := ctExplain(1, "error \"line too long\"\n\n")

	rows := []struct {
		name     string
		args     []string
		stdin    string
		out, err string
		code     int
	}{
		{name: "exactly 1 MiB is processed", stdin: exact + "\n", out: linkclean.Clean(exact) + "\n"},
		{name: "exactly 1 MiB at EOF is processed", stdin: exact, out: linkclean.Clean(exact) + "\n"},
		{name: "explain exactly 1 MiB", args: []string{"-explain"}, stdin: exact + "\n",
			out: ctExplain(1, "out "+strconv.Quote(exact)+"\nskipped toolong\n\n")},
		{name: "over the cap is copied", stdin: over + "\n" + ctGoogle + "\n",
			out: over + "\n" + ctGoogleOut + "\n", err: copied(1), code: 1},
		{name: "over the cap at EOF", stdin: ctGoogle + "\n" + over,
			out: ctGoogleOut + "\n" + over + "\n", err: copied(2), code: 1},
		{name: "over the cap crlf", stdin: over + "\r\n", out: over + "\n", err: copied(1), code: 1},
		{name: "over the cap keeps surrounding spaces", stdin: "  " + exact + " \t\r\n",
			out: "  " + exact + " \t\n", err: copied(1), code: 1},
		{name: "over the cap keeps inner CRs", stdin: crs + "\r\n" + ctAmazon + "\n",
			out: crs + "\n" + ctAmazon + "\n", err: copied(1), code: 1},
		{name: "two lines over the cap", stdin: over + "\n\n" + over + "\n",
			out: over + "\n\n" + over + "\n", err: copied(1) + copied(3), code: 1},
		{name: "explain over the cap", args: []string{"-explain"}, stdin: over + "\n" + ctGoogle + "\n",
			out: tooLong + ctExplain(2, ctGoogleExplain), code: 1},
		{name: "explain over the cap at EOF", args: []string{"-explain"}, stdin: over,
			out: tooLong, code: 1},
		{name: "json over the cap", args: []string{"-json"}, stdin: over + "\r\n" + ctGoogle + "\n",
			out: ctJSONError(1, "line too long") + ctJSON(2, ctGoogleJSON), code: 1},
		{name: "check over the cap", args: []string{"-check"}, stdin: "X1 " + over[3:] + "\n",
			err: "linkclean: stdin:1: line longer than 1048576 bytes\n", code: 2},
		{name: "9000 bytes is skipped toolong", args: []string{"-explain"}, stdin: nine + "\n",
			out: ctExplain(1, "out "+strconv.Quote(nine)+"\nskipped toolong\n\n")},
		{name: "9000 bytes plain", stdin: nine + "\n", out: linkclean.Clean(nine) + "\n"},
		{name: "longer than 64 KiB is read", stdin: big + "\n" + ctGoogle + "\n",
			out: linkclean.Clean(big) + "\n" + ctGoogleOut + "\n"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			ctCheck(t, ctRun(strings.NewReader(r.stdin), r.args...), r.out, r.err, r.code)
		})
	}
}
