package discordlink

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// txtarFile is one "-- name --" section of a txtar archive.
type txtarFile struct {
	Name string
	Data []byte
	Line int // line of the section marker
}

// parseTxtar splits data into its leading comment and its sections, as
// golang.org/x/tools/txtar does (without the dependency). It is a copy of the
// root package's helper, which test files here cannot import.
func parseTxtar(data []byte) (comment []byte, files []txtarFile) {
	lines := bytes.SplitAfter(data, []byte("\n"))
	cur := -1
	for i, l := range lines {
		s := strings.TrimRight(string(l), "\r\n")
		if strings.HasPrefix(s, "-- ") && strings.HasSuffix(s, " --") && len(s) > 6 {
			files = append(files, txtarFile{Name: strings.TrimSpace(s[3 : len(s)-3]), Line: i + 1})
			cur = len(files) - 1
			continue
		}
		if cur < 0 {
			comment = append(comment, l...)
		} else {
			files[cur].Data = append(files[cur].Data, l...)
		}
	}
	return comment, files
}

// parseStrExpr parses the string expressions the testdata files use for
// long values: one or more Go-quoted strings, each optionally repeated with
// *N, joined by " + ", as in "https://a.com/" + "x"*1980 + "?q". It returns
// the value and the unparsed rest of s (after one separating space, if any).
func parseStrExpr(s string) (val, rest string, err error) {
	var b strings.Builder
	for {
		q, err := strconv.QuotedPrefix(s)
		if err != nil {
			return "", "", fmt.Errorf("want a Go-quoted string at %q", s)
		}
		v, _ := strconv.Unquote(q)
		s = s[len(q):]
		n := 1
		if strings.HasPrefix(s, "*") {
			d := 1
			for d < len(s) && '0' <= s[d] && s[d] <= '9' {
				d++
			}
			if n, err = strconv.Atoi(s[1:d]); err != nil || n < 0 || n > 1<<24 {
				return "", "", fmt.Errorf("bad repeat count at %q", s)
			}
			s = s[d:]
		}
		for range n {
			b.WriteString(v)
		}
		if !strings.HasPrefix(s, " + ") {
			return b.String(), strings.TrimPrefix(s, " "), nil
		}
		s = s[3:]
	}
}

func TestTxtarParse(t *testing.T) {
	comment, files := parseTxtar([]byte("c1\nc2\n-- a --\nx\n-- b --\ny\nz\n"))
	if string(comment) != "c1\nc2\n" || len(files) != 2 || files[0].Name != "a" || files[1].Line != 5 || string(files[1].Data) != "y\nz\n" {
		t.Fatalf("got %q %+v", comment, files)
	}
}

func TestParseStrExpr(t *testing.T) {
	for _, c := range []struct{ in, val, rest string }{
		{`"a"`, "a", ""},
		{`"a" spoiler`, "a", "spoiler"},
		{`"ab"*3`, "ababab", ""},
		{`"x" + "y"*2 + "\n" label="l"`, "xyy\n", `label="l"`},
		{`"a"*0 + "b"`, "b", ""},
		{"`raw`", "raw", ""},
	} {
		val, rest, err := parseStrExpr(c.in)
		if err != nil || val != c.val || rest != c.rest {
			t.Errorf("parseStrExpr(%s) = %q, %q, %v; want %q, %q", c.in, val, rest, err, c.val, c.rest)
		}
	}
	for _, in := range []string{``, `a`, `"a`, `"a"*`, `"a"*-1`, `"a" + `, `"a" + b`} {
		if _, _, err := parseStrExpr(in); err == nil {
			t.Errorf("parseStrExpr(%s): no error", in)
		}
	}
}
