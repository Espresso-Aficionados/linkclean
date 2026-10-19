package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"testing/iotest"
)

// ctNegatives returns negative.txt, ending in "\n", and its case count,
// counted here independently of the CLI.
func ctNegatives(t *testing.T) (string, int) {
	t.Helper()
	b, err := os.ReadFile("../../testdata/golden/negative.txt")
	if err != nil {
		t.Fatal(err)
	}
	data := string(b)
	if !strings.HasSuffix(data, "\n") {
		data += "\n"
	}
	n := 0
	for line := range strings.SplitSeq(data, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			n++
		}
	}
	if n < 40 {
		t.Fatalf("negative.txt has %d cases, want at least 40", n)
	}
	return data, n
}

func ctFail(line int, id, policy, u, got string) string {
	return fmt.Sprintf("FAIL line %d id %q %s: %q -> %q\n", line, id, policy, u, got)
}

// TestCheckNegativeGolden: the real case file passes.
func TestCheckNegativeGolden(t *testing.T) {
	data, n := ctNegatives(t)
	got := ctRun(strings.NewReader(data), "-check")
	ctCheck(t, got, fmt.Sprintf("ok: %d cases unchanged under keep and strip\n", n), "", 0)
}

// TestCheckMutated: -check evaluates both policies: a case that only
// AffiliateStrip changes fails, and so does one both change.
func TestCheckMutated(t *testing.T) {
	data, n := ctNegatives(t)
	last := strings.Count(data, "\n") + 1 // the physical line of an appended case

	t.Run("strip only", func(t *testing.T) {
		got := ctRun(strings.NewReader(data+"X1 "+ctAmazon+"\n"), "-check")
		want := ctFail(last, "X1", "strip", ctAmazon, ctAmazonStrip) +
			fmt.Sprintf("FAIL: 1 of %d cases changed\n", n+1)
		ctCheck(t, got, want, "", 1)
	})
	t.Run("keep and strip", func(t *testing.T) {
		got := ctRun(strings.NewReader(data+"X2 "+ctGoogle+"\n"), "-check")
		want := ctFail(last, "X2", "keep", ctGoogle, ctGoogleOut) +
			ctFail(last, "X2", "strip", ctGoogle, ctGoogleOut) +
			fmt.Sprintf("FAIL: 1 of %d cases changed\n", n+1)
		ctCheck(t, got, want, "", 1)
	})
	t.Run("input order", func(t *testing.T) {
		in := "X2 " + ctGoogle + "\n" + data + "X1 " + ctAmazon + "\r\n"
		got := ctRun(strings.NewReader(in), "-check")
		want := ctFail(1, "X2", "keep", ctGoogle, ctGoogleOut) +
			ctFail(1, "X2", "strip", ctGoogle, ctGoogleOut) +
			ctFail(last+1, "X1", "strip", ctAmazon, ctAmazonStrip) +
			fmt.Sprintf("FAIL: 2 of %d cases changed\n", n+2)
		ctCheck(t, got, want, "", 1)
	})
}

// TestCheckErrors: input errors exit 2 with empty stdout, even after a
// failing case (the whole input is parsed before anything is evaluated).
func TestCheckErrors(t *testing.T) {
	malformed := func(line int, text string) string {
		return fmt.Sprintf("linkclean: stdin:%d: want \"<id> <url>\", have %q\n", line, text)
	}
	const noCases = "linkclean: -check: no cases on stdin\n"
	rows := []struct {
		name, stdin, err string
	}{
		{"empty", "", noCases},
		{"blank lines", "\n \t\n\r\n", noCases},
		{"comments only", "# a\n\n  # b c\n#\n", noCases},
		{"id only", "X1\n", malformed(1, "X1")},
		{"bare URL", ctAmazon + "\n", malformed(1, ctAmazon)},
		{"double space", "12  https://a/\n", malformed(1, "12  https://a/")},
		{"space in URL", "12 https://a/ b\n", malformed(1, "12 https://a/ b")},
		{"tab in URL", "12 https://a/\tb\n", malformed(1, "12 https://a/\tb")},
		{"tab separator", "12\thttps://a/\n", malformed(1, "12\thttps://a/")},
		{"physical line number", "# c\n\nX1 https://a/\n  X2  \n", malformed(4, "X2")},
		{"first error only", "X1\nX2\n", malformed(1, "X1")},
		{"failing case then malformed", "X1 " + ctAmazon + "\nX2\n", malformed(2, "X2")},
		{"line over 1 MiB", "X1 " + ctLong(ctMaxLine+1-3) + "\n",
			"linkclean: stdin:1: line longer than 1048576 bytes\n"},
		{"failing case then line over 1 MiB", "X1 " + ctAmazon + "\nX2 " + ctLong(ctMaxLine) + "\n",
			"linkclean: stdin:2: line longer than 1048576 bytes\n"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			ctCheck(t, ctRun(strings.NewReader(r.stdin), "-check"), "", r.err, 2)
		})
	}
	t.Run("read error", func(t *testing.T) {
		in := io.MultiReader(strings.NewReader("X1 "+ctAmazon+"\n"), iotest.ErrReader(ctErrBoom))
		ctCheck(t, ctRun(in, "-check"), "", "linkclean: reading stdin: boom\n", 2)
	})
}

// TestCheckGrammar: -check reads exactly parseNegatives' grammar
// (txtar_test.go): trim with strings.TrimSpace, skip "" and "#...", cut at the
// first ' ', and reject a line without one or with ' ' or '\t' in the URL.
// The expectations were derived by reading parseNegatives. Accepted lines use
// a URL that only AffiliateStrip changes, so the FAIL line shows how the line
// was split.
func TestCheckGrammar(t *testing.T) {
	const nbsp = "\xc2\xa0"
	accept := []struct{ line, id string }{
		{"a " + ctAmazon, "a"},
		{"\ta " + ctAmazon + "\r", "a"},
		{"  a " + ctAmazon + "  ", "a"},
		{nbsp + "a " + ctAmazon + nbsp, "a"}, // TrimSpace trims Unicode space
		{"a" + nbsp + "b " + ctAmazon, "a" + nbsp + "b"},
		{"x\x1b " + ctAmazon, "x\x1b"},
		{"a#b " + ctAmazon, "a#b"}, // '#' only starts a comment as the first byte
	}
	for _, a := range accept {
		t.Run(fmt.Sprintf("accept %q", a.line), func(t *testing.T) {
			got := ctRun(strings.NewReader(a.line+"\n"), "-check")
			want := ctFail(1, a.id, "strip", ctAmazon, ctAmazonStrip) + "FAIL: 1 of 1 cases changed\n"
			ctCheck(t, got, want, "", 1)
		})
	}
	// A non-URL is a case too; Clean leaves it unchanged.
	t.Run("accept a b", func(t *testing.T) {
		ctCheck(t, ctRun(strings.NewReader("a b\n"), "-check"), "ok: 1 cases unchanged under keep and strip\n", "", 0)
	})
	for _, line := range []string{
		"a",
		"a b c",
		"a b\tc",
		"a  b",
		"a" + nbsp + "b",
		"a\tb",
		"a\vb",
		ctAmazon,
	} {
		t.Run(fmt.Sprintf("reject %q", line), func(t *testing.T) {
			want := fmt.Sprintf("linkclean: stdin:1: want \"<id> <url>\", have %q\n", line)
			ctCheck(t, ctRun(strings.NewReader(line+"\n"), "-check"), "", want, 2)
		})
	}
	for _, line := range []string{"# a b", "  # a b", "#", "  ", "\t\r"} {
		t.Run(fmt.Sprintf("skip %q", line), func(t *testing.T) {
			ctCheck(t, ctRun(strings.NewReader(line+"\n"), "-check"), "", "linkclean: -check: no cases on stdin\n", 2)
		})
	}
}
