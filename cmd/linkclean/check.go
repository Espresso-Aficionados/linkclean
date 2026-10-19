package main

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Espresso-Aficionados/linkclean"
)

// checkCase is one "<id> <url>" line of -check's input.
type checkCase struct {
	line    int
	id, url string
}

// parseCheckLine splits one trimmed, non-blank, non-'#' line. Its grammar must
// track parseNegatives in the root package's txtar_test.go, which reads the
// same file for the Golden test.
func parseCheckLine(line string) (id, u string, ok bool) {
	id, u, ok = strings.Cut(line, " ")
	if !ok || strings.ContainsAny(u, " \t") {
		return "", "", false
	}
	return id, u, true
}

// runCheck is -check: it reads and parses all of stdin, then checks that
// every case comes back unchanged under AffiliateKeep and AffiliateStrip with
// otherwise the zero Config, as the Golden test does. Any input error exits
// 2 with nothing on stdout.
func runCheck(e env) int {
	var cases []checkCase
	lr := &lineReader{r: bufio.NewReaderSize(e.Stdin, 64<<10)}
	for {
		line, long, err := lr.next(nil)
		if err == io.EOF {
			break
		}
		if err != nil {
			return errorf(e, "reading stdin: %v", err)
		}
		if long {
			return errorf(e, "stdin:%d: line longer than %d bytes", lr.n, maxLine)
		}
		s := strings.TrimSpace(string(line))
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		id, u, ok := parseCheckLine(s)
		if !ok {
			return errorf(e, "stdin:%d: want \"<id> <url>\", have %q", lr.n, s)
		}
		cases = append(cases, checkCase{lr.n, id, u})
	}
	if len(cases) == 0 {
		return errorf(e, "-check: no cases on stdin")
	}

	keep, err := linkclean.New(linkclean.Config{})
	if err != nil {
		return ruleDataError(e, err)
	}
	strip, err := linkclean.New(linkclean.Config{Affiliate: linkclean.AffiliateStrip})
	if err != nil {
		return ruleDataError(e, err)
	}
	w := bufio.NewWriter(e.Stdout)
	failed := 0
	for _, c := range cases {
		bad := false
		for _, p := range []struct {
			name string
			c    *linkclean.Cleaner
		}{{"keep", keep}, {"strip", strip}} {
			got := p.c.Clean(c.url)
			r := p.c.Explain(c.url)
			if got == c.url && r.URL == c.url && !r.Changed {
				continue
			}
			bad = true
			var note string
			if got == c.url {
				got = r.URL
				if got == c.url {
					note = " (Explain.Changed)"
				}
			}
			fmt.Fprintf(w, "FAIL line %d id %s %s: %s -> %s%s\n",
				c.line, strconv.Quote(c.id), p.name, strconv.Quote(c.url), strconv.Quote(got), note)
		}
		if bad {
			failed++
		}
	}
	code := exitOK
	if failed > 0 {
		fmt.Fprintf(w, "FAIL: %d of %d cases changed\n", failed, len(cases))
		code = exitFail
	} else {
		fmt.Fprintf(w, "ok: %d cases unchanged under keep and strip\n", len(cases))
	}
	if err := w.Flush(); err != nil {
		return errorf(e, "writing output: %v", err)
	}
	return code
}
