// Command linkclean removes tracking parameters from URLs, offline, with the
// rules embedded in package linkclean.
//
//	linkclean [-affiliate keep|strip] [-explain | -json] [--] [URL ...]
//	linkclean -check < cases
//	linkclean -licenses | -version
//
// With URL arguments, each argument is one input and standard input is not
// read. Without them, each line of standard input is one input. Inputs are
// trimmed with strings.TrimSpace (which also removes a '\r' before '\n').
// Flags come before the URLs: a later argument that starts with '-' is a usage
// error unless "--" ends the flags. For lists of URLs, use standard input; put
// "--" before URL arguments that may start with '-' (xargs linkclean --).
//
// The command never uses the network and does not resolve short links;
// package unshorten is not part of it.
//
// # Modes
//
// Plain (the default) writes Clean of each input and a newline. For standard
// input it is a line-for-line filter: output line i belongs to input line i,
// blank and '#' lines included (Clean returns them unchanged). An argument
// that contains a newline after trimming comes back unchanged and spans
// several output lines. Plain output is byte-faithful URL data, not
// terminal-safe text: inputs with C0 controls or DEL come back unchanged, but
// C1 controls and Unicode format or bidi characters are not ambiguous to the
// engine and pass through to the cleaned URL. Use -explain to view untrusted
// input on a terminal.
//
// -explain writes a human-readable record per input; its format is not a
// stable interface. -json writes one JSON object per input (JSON Lines) and
// implies -explain's content. Fields may be added in a minor release; any
// other change to the schema or its words needs a minor release with a
// CHANGELOG line. Blank and '#' lines of standard input get no record, and
// "line" is the physical line number (or the argument's 1-based index), the
// key for correlating records with the caller's input. Both show the cleaned
// URL, the flags (changed, skipped, abandoned, signed), and per hop, removal
// and kept entry the category, part, parameter name, enclosing parameter,
// wrapper host, rule ID and reason; for affiliate entries also the network and
// the affiliate ID. They never show the input or the removed values
// (Removal.Value and Text, Hop.From and To). Affiliate IDs are attacker-
// controlled text and are escaped: -explain quotes every string with
// strconv.Quote, and -json writes bytes that are not valid UTF-8 as %XX in
// derived fields and \u-escapes every non-printable character. A -json "url"
// that is not valid UTF-8 (only possible when the input is not) is replaced
// by an error object. The cleaned URL still carries whatever cleaning kept,
// such as an affiliate tag under -affiliate keep, so the output is not
// automatically safe to log.
//
// -affiliate strip also removes affiliate tags and unwraps affiliate
// redirects; the default, keep, leaves them.
//
// -check reads "<id> <url>" lines from standard input (blank and '#' lines are
// ignored) and checks that every URL comes back unchanged under both affiliate
// policies. It prints a FAIL line per changed case and one summary line.
//
// -licenses prints the license and attribution notices a copy of the binary
// must carry; -version prints the build's version and the rule data's
// sources.
//
// # Limits
//
// A line of standard input longer than 1 MiB is never evaluated: plain mode
// copies it unchanged (untrimmed, without its line terminator) and reports
// it on standard error, -explain and -json write an error record for it, and
// -check rejects the input.
//
// # Exit status
//
//	0   success
//	1   a -check case changed; a -explain or -json error record was written;
//	    or a line over 1 MiB was copied unchanged
//	2   an input, output or rule-data error
//	64  a usage error
//
// Under go run every non-zero status becomes 1, with "exit status N" on
// standard error.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Espresso-Aficionados/linkclean"
)

const (
	exitOK    = 0
	exitFail  = 1
	exitError = 2
	exitUsage = 64

	// maxLine is the longest stdin line, without its terminator, that is
	// evaluated.
	maxLine = 1 << 20
)

// env is everything run touches outside its arguments.
type env struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

func main() { os.Exit(run(env{os.Stdin, os.Stdout, os.Stderr}, os.Args[1:])) }

const usageText = `usage: linkclean [-affiliate keep|strip] [-explain | -json] [--] [URL ...]
       linkclean -check < cases
       linkclean -licenses | -version

linkclean cleans each URL argument, or each line of standard input when there
are none, and writes one result per input. It never uses the network and does
not resolve short links. For lists of URLs, use standard input; put -- before
URL arguments that may start with "-". Plain output is URL data and may
contain Unicode control or bidi characters from the input (-json escapes them,
but its decoded strings carry them); use -explain to view untrusted input on a
terminal.

-explain and -json report what was removed and why: parameter names, rule IDs
and affiliate networks, never the input or the removed values; affiliate IDs
are shown (they are attacker-controlled text and are escaped).

Exit status: 0 success; 1 a -check case changed, a -explain/-json input could
not be reported, or a line over 1 MiB was copied unchanged; 2 an input, output
or rule-data error; 64 a usage error.

flags:
`

// output modes of a clean run.
const (
	modePlain = iota
	modeExplain
	modeJSON
)

// run returns the exit code; it never calls os.Exit.
func run(e env, args []string) int {
	fs := flag.NewFlagSet("linkclean", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // errors are reported below, with the "linkclean: " prefix
	fs.Usage = func() {}
	explain := fs.Bool("explain", false, "describe each input's changes in human-readable text")
	jsonOut := fs.Bool("json", false, "write one JSON object per input (JSON Lines); implies -explain's content")
	affiliate := fs.String("affiliate", "keep", "affiliate policy: keep leaves affiliate tags, strip removes them")
	check := fs.Bool("check", false, `read "<id> <url>" cases from standard input; exit 1 if any changes under either affiliate policy`)
	licenses := fs.Bool("licenses", false, "print the license and attribution notices")
	version := fs.Bool("version", false, "print the version and the rule data sources")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			fs.SetOutput(e.Stderr)
			fmt.Fprint(e.Stderr, usageText)
			fs.PrintDefaults()
			return exitOK
		}
		// The flag package echoes the offending argument raw.
		return usagef(e, "%s", printable(err.Error()))
	}
	inputs := fs.Args()
	if n := len(args) - len(inputs); n == 0 || args[n-1] != "--" {
		for _, a := range inputs {
			if strings.HasPrefix(a, "-") {
				return usagef(e, "flag-like argument %q after URL arguments; put flags first, or use -- before URLs", a)
			}
		}
	}

	var only string // the flag that excludes every other
	switch {
	case *check:
		only = "check"
	case *licenses:
		only = "licenses"
	case *version:
		only = "version"
	}
	if only != "" {
		var other string
		fs.Visit(func(f *flag.Flag) {
			if f.Name != only && other == "" {
				other = f.Name
			}
		})
		switch {
		case other != "":
			return usagef(e, "-%s cannot be combined with -%s", only, other)
		case len(inputs) > 0:
			return usagef(e, "-%s takes no arguments", only)
		}
		switch only {
		case "check":
			return runCheck(e)
		case "licenses":
			return runAbout(e, writeLicenses)
		default:
			return runAbout(e, writeVersion)
		}
	}

	var cfg linkclean.Config
	switch *affiliate {
	case "keep":
	case "strip":
		cfg.Affiliate = linkclean.AffiliateStrip
	default:
		return usagef(e, "-affiliate must be keep or strip, have %q", *affiliate)
	}
	mode := modePlain
	switch {
	case *jsonOut:
		mode = modeJSON
	case *explain:
		mode = modeExplain
	}
	c, err := linkclean.New(cfg)
	if err != nil {
		return ruleDataError(e, err)
	}
	return runClean(e, c, mode, inputs)
}

// usagef reports a usage error and returns exitUsage.
func usagef(e env, format string, a ...any) int {
	fmt.Fprintf(e.Stderr, "linkclean: "+format+"\n", a...)
	return exitUsage
}

// errorf reports an input, output or rule-data error and returns exitError.
func errorf(e env, format string, a ...any) int {
	fmt.Fprintf(e.Stderr, "linkclean: "+format+"\n", a...)
	return exitError
}

// ruleDataError reports an error from linkclean.New and returns exitError:
// one line per joined error, each escaped with printable, because rule IDs
// are upstream pattern text and -check runs on unreviewed rule data. The
// errors already start with "linkclean: ", which is not repeated.
func ruleDataError(e env, err error) int {
	for _, line := range strings.Split(err.Error(), "\n") {
		fmt.Fprintf(e.Stderr, "linkclean: %s\n", printable(strings.TrimPrefix(line, "linkclean: ")))
	}
	return exitError
}

// printable returns s with every byte that is not valid UTF-8 and every rune
// that strconv.IsPrint rejects written as its Go escape (\x1b, \u202e, \xff),
// so a message that echoes untrusted text cannot drive a terminal. Other
// runes, quotes and backslashes included, are kept as is.
func printable(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case strconv.IsPrint(r):
			b.WriteString(s[i : i+size])
		default:
			q := strconv.QuoteRune(r)
			b.WriteString(q[1 : len(q)-1])
		}
		i += size
	}
	return b.String()
}

// cleanRun writes one result per input to w.
type cleanRun struct {
	c    *linkclean.Cleaner
	mode int
	w    *bufio.Writer
	buf  []byte
	code int // exitOK, or exitFail once an error record was written
}

// input writes the result for the trimmed input s at position n.
func (p *cleanRun) input(n int, s string) error {
	var err error
	switch p.mode {
	case modePlain:
		p.buf = append(append(p.buf[:0], p.c.Clean(s)...), '\n')
	case modeExplain:
		p.buf = appendExplain(p.buf[:0], n, p.c.Explain(s))
	case modeJSON:
		var ok bool
		p.buf, ok, err = appendJSON(p.buf[:0], n, p.c.Explain(s))
		if err != nil {
			return err
		}
		if !ok {
			p.code = exitFail
		}
	}
	_, err = p.w.Write(p.buf)
	return err
}

// tooLong writes the error record for a line over maxLine (-explain and
// -json only).
func (p *cleanRun) tooLong(n int) error {
	p.code = exitFail
	if p.mode == modeJSON {
		p.buf = appendJSONError(p.buf[:0], n, errLineTooLong)
	} else {
		p.buf = appendExplainError(p.buf[:0], n, errLineTooLong)
	}
	_, err := p.w.Write(p.buf)
	return err
}

func runClean(e env, c *linkclean.Cleaner, mode int, args []string) int {
	p := &cleanRun{c: c, mode: mode, w: bufio.NewWriterSize(e.Stdout, 64<<10)}
	if len(args) > 0 {
		for i, a := range args {
			if err := p.input(i+1, strings.TrimSpace(a)); err != nil {
				return errorf(e, "writing output: %v", err)
			}
		}
		if err := p.w.Flush(); err != nil {
			return errorf(e, "writing output: %v", err)
		}
		return p.code
	}

	in := &flushReader{r: e.Stdin, w: p.w}
	lr := &lineReader{r: bufio.NewReaderSize(in, 64<<10)}
	var spill func([]byte)
	var raw rawCopy
	if mode == modePlain {
		raw.w = p.w
		spill = raw.write
	}
	for {
		line, long, err := lr.next(spill)
		if err == io.EOF {
			break
		}
		if in.err != nil {
			return errorf(e, "writing output: %v", in.err)
		}
		if err != nil {
			p.w.Flush()
			return errorf(e, "reading stdin: %v", err)
		}
		switch {
		case long && mode == modePlain:
			err = raw.end()
			p.code = exitFail
			fmt.Fprintf(e.Stderr, "linkclean: stdin:%d: line longer than %d bytes, copied unchanged\n", lr.n, maxLine)
		case long:
			err = p.tooLong(lr.n)
		default:
			s := strings.TrimSpace(string(line))
			if mode != modePlain && (s == "" || strings.HasPrefix(s, "#")) {
				continue
			}
			err = p.input(lr.n, s)
		}
		if err != nil {
			return errorf(e, "writing output: %v", err)
		}
	}
	if err := p.w.Flush(); err != nil {
		return errorf(e, "writing output: %v", err)
	}
	return p.code
}

// flushReader flushes w before every read from r, so each result already
// written to w reaches stdout before reading more input can block, also
// when part of the next line is already buffered. A flush error is kept in
// err and also returned from Read.
type flushReader struct {
	r   io.Reader
	w   *bufio.Writer
	err error
}

func (f *flushReader) Read(p []byte) (int, error) {
	if f.err = f.w.Flush(); f.err != nil {
		return 0, f.err
	}
	return f.r.Read(p)
}

// lineReader splits standard input into lines without holding more than
// about maxLine bytes of one line. bufio.Scanner is not used: its token limit
// would abort the whole run.
type lineReader struct {
	r   *bufio.Reader
	buf []byte
	n   int // physical number of the last line returned
}

// next returns the next line, without its '\n', or io.EOF when there are no
// more lines; a final line without '\n' is a line, and empty input has none.
// A line whose content, without its terminator ("\n" or "\r\n"), is longer
// than maxLine is not returned: next reports long and passes the line's raw
// bytes, terminator included, to spill chunk by chunk (spill may be nil to
// discard them). Any other error is a read error.
func (lr *lineReader) next(spill func([]byte)) (line []byte, long bool, err error) {
	lr.buf = lr.buf[:0]
	started := false
	for {
		chunk, err := lr.r.ReadSlice('\n')
		if err != nil && err != bufio.ErrBufferFull && err != io.EOF {
			return nil, false, err
		}
		if len(chunk) > 0 {
			started = true
		}
		if err == io.EOF && !started {
			return nil, false, io.EOF
		}
		switch {
		case long:
			if spill != nil {
				spill(chunk)
			}
		default:
			lr.buf = append(lr.buf, chunk...)
			// Content plus a possible '\r' fits in maxLine+1 bytes.
			if err == bufio.ErrBufferFull && len(lr.buf) > maxLine+1 {
				long = true
				if spill != nil {
					spill(lr.buf)
				}
				lr.buf = lr.buf[:0]
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		lr.n++
		if long {
			return nil, true, nil
		}
		line = lr.buf
		content := line
		if n := len(content); n > 0 && content[n-1] == '\n' {
			line = line[:n-1]
			content = line
			if n := len(content); n > 0 && content[n-1] == '\r' {
				content = content[:n-1]
			}
		}
		if len(content) > maxLine {
			if spill != nil {
				spill(lr.buf)
			}
			return nil, true, nil
		}
		return line, false, nil
	}
}

// rawCopy writes a long line's raw bytes without its terminator ("\n" or
// "\r\n"). A chunk's final '\r' is held back until the next chunk shows
// whether '\n' follows. Write errors are sticky in the bufio.Writer and
// surface from end.
type rawCopy struct {
	w  *bufio.Writer
	cr bool // a '\r' is held back
}

func (c *rawCopy) write(p []byte) {
	nl := len(p) > 0 && p[len(p)-1] == '\n'
	if nl {
		p = p[:len(p)-1]
	}
	if c.cr && (!nl || len(p) > 0) {
		c.w.WriteByte('\r')
	}
	c.cr = false
	if n := len(p); n > 0 && p[n-1] == '\r' {
		p = p[:n-1]
		c.cr = !nl
	}
	c.w.Write(p)
}

// end finishes the line: a held '\r' was content (the line ended at EOF), and
// the output line ends with '\n'.
func (c *rawCopy) end() error {
	if c.cr {
		c.w.WriteByte('\r')
		c.cr = false
	}
	return c.w.WriteByte('\n')
}
