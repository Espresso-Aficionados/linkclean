package discordlink

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Espresso-Aficionados/linkclean"
)

// skipIfNotImplemented runs f and skips t if f panics with A0's
// "not implemented" placeholder (Process and Offline before A3 merges).
func skipIfNotImplemented(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			if s, ok := r.(string); ok && strings.Contains(s, "not implemented") {
				t.Skipf("skipped: %s", s)
			}
			panic(r)
		}
	}()
	f()
}

// annotation is one Annotate result in a repost.txtar case.
type annotation struct {
	lines []string
	req   int
}

// repostCase is one section of testdata/repost.txtar.
type repostCase struct {
	name          string
	line          int
	opts          RepostOptions
	cs            []Change
	full, compact map[int]annotation
	overflow      *string
	overflowAt    map[int]string
	bodies        []string
	nobodies      bool
	calls         map[int][2]int
}

// parseRepostCase parses one section; see the file's header for the format.
func parseRepostCase(f txtarFile) (*repostCase, error) {
	c := &repostCase{name: f.Name, line: f.Line, full: map[int]annotation{}, compact: map[int]annotation{}, overflowAt: map[int]string{}, calls: map[int][2]int{}}
	for n, l := range strings.Split(string(f.Data), "\n") {
		l = strings.TrimRight(l, "\r")
		if l == "" || strings.HasPrefix(l, "# ") {
			continue
		}
		err := c.parseLine(l)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", f.Line+1+n, err)
		}
	}
	if (len(c.bodies) > 0) == c.nobodies {
		return nil, fmt.Errorf("want exactly one of body lines and nobodies")
	}
	return c, nil
}

func (c *repostCase) parseLine(l string) error {
	verb, rest, _ := strings.Cut(l, " ")
	strs := func(s string) ([]string, error) {
		var out []string
		for s != "" {
			v, r, err := parseStrExpr(s)
			if err != nil {
				return nil, err
			}
			out, s = append(out, v), r
		}
		return out, nil
	}
	intArg := func(s string) (int, string, error) {
		a, r, _ := strings.Cut(s, " ")
		n, err := strconv.Atoi(a)
		return n, r, err
	}
	switch verb {
	case "opt":
		for rest != "" {
			kv, r, _ := strings.Cut(rest, " ")
			k, v, _ := strings.Cut(kv, "=")
			var err error
			switch k {
			case "wrap":
				switch v {
				case "angle":
					c.opts.Wrap = WrapAngle
				case "none":
					c.opts.Wrap = WrapNone
				default:
					return fmt.Errorf("bad wrap %q", v)
				}
			case "nospoiler":
				c.opts.NoSpoiler = true
			case "maxlen":
				c.opts.MaxLen, err = strconv.Atoi(v)
			case "maxbodies":
				c.opts.MaxBodies, err = strconv.Atoi(v)
			case "unexamined":
				c.opts.Unexamined, err = strconv.Atoi(v)
			case "header":
				// The value is a string expression and may contain spaces.
				c.opts.Header, r, err = parseStrExpr(strings.TrimPrefix(rest, "header="))
			default:
				return fmt.Errorf("unknown opt %q", k)
			}
			if err != nil {
				return fmt.Errorf("opt %s: %v", k, err)
			}
			rest = r
		}
	case "change":
		u, r, err := parseStrExpr(rest)
		if err != nil {
			return err
		}
		ch := Change{Link: Link{URL: u, Start: len(c.cs), Kind: Bare}, Result: linkclean.Result{Input: u, URL: u, Changed: true}}
		switch r {
		case "":
		case "spoiler":
			ch.Link.Spoiler = true
		default:
			return fmt.Errorf("trailing %q", r)
		}
		c.cs = append(c.cs, ch)
	case "full", "compact":
		if len(c.cs) == 0 {
			return fmt.Errorf("%s before any change", verb)
		}
		req, r, err := intArg(rest)
		if err != nil {
			return err
		}
		lines, err := strs(r)
		if err != nil {
			return err
		}
		m := c.full
		if verb == "compact" {
			m = c.compact
		}
		m[len(c.cs)-1] = annotation{lines, req}
	case "overflow":
		s, r, err := parseStrExpr(rest)
		if err != nil || r != "" {
			return fmt.Errorf("overflow: %v %q", err, r)
		}
		c.overflow = &s
	case "overflow-at":
		k, r, err := intArg(rest)
		if err != nil {
			return err
		}
		s, r, err := parseStrExpr(r)
		if err != nil || r != "" {
			return fmt.Errorf("overflow-at: %v %q", err, r)
		}
		c.overflowAt[k] = s
	case "body":
		s, r, err := parseStrExpr(rest)
		if err != nil || r != "" {
			return fmt.Errorf("body: %v %q", err, r)
		}
		c.bodies = append(c.bodies, s)
	case "nobodies":
		c.nobodies = true
	case "calls":
		f := strings.Fields(rest)
		if len(f) != 3 {
			return fmt.Errorf("calls wants 3 numbers")
		}
		var n [3]int
		for i, s := range f {
			var err error
			if n[i], err = strconv.Atoi(s); err != nil {
				return err
			}
		}
		c.calls[n[0]] = [2]int{n[1], n[2]}
	default:
		return fmt.Errorf("unknown line %q", l)
	}
	return nil
}

// options returns the case's RepostOptions with Annotate and Overflow
// built from its full, compact and overflow lines. Annotate identifies a
// change by Link.Start, its index, and returns a fresh copy of its lines.
func (c *repostCase) options() RepostOptions {
	o := c.opts
	if len(c.full)+len(c.compact) > 0 {
		o.Annotate = func(ch Change, compact bool) ([]string, int) {
			m := c.full
			if compact {
				m = c.compact
			}
			a := m[ch.Link.Start]
			return slices.Clone(a.lines), a.req
		}
	}
	if c.overflow != nil || len(c.overflowAt) > 0 {
		o.Overflow = func(omitted, unexamined int) string {
			s, ok := c.overflowAt[omitted]
			if !ok && c.overflow != nil {
				s = *c.overflow
			}
			s = strings.ReplaceAll(s, "{o}", strconv.Itoa(omitted))
			return strings.ReplaceAll(s, "{u}", strconv.Itoa(unexamined))
		}
	}
	return o
}

func TestRepostGolden(t *testing.T) {
	data, err := os.ReadFile("testdata/repost.txtar")
	if err != nil {
		t.Fatal(err)
	}
	_, files := parseTxtar(data)
	if len(files) == 0 {
		t.Fatal("no cases")
	}
	seen := map[string]bool{}
	for _, f := range files {
		c, err := parseRepostCase(f)
		if err != nil {
			t.Errorf("repost.txtar:%d %s: %v", f.Line, f.Name, err)
			continue
		}
		if seen[c.name] {
			t.Errorf("repost.txtar:%d: duplicate case %s", c.line, c.name)
		}
		seen[c.name] = true
		t.Run(c.name, func(t *testing.T) {
			o := c.options()
			got, st := repost(c.cs, o)
			if !slices.Equal(got, c.bodies) || (got == nil) != c.nobodies {
				t.Errorf("repost.txtar:%d: bodies\n%s\nwant\n%s", c.line, quoteBodies(got), quoteBodies(c.bodies))
			}
			if g := Repost(c.cs, o); !slices.Equal(g, got) {
				t.Errorf("Repost and repost disagree: %q vs %q", g, got)
			}
			for i, want := range c.calls {
				if i >= len(c.cs) {
					t.Fatalf("calls for change %d of %d", i, len(c.cs))
				}
				if g := [2]int{st.annotate[i], st.compact[i]}; g != want {
					t.Errorf("change %d: Annotate calls (full, compact) = %v, want %v", i, g, want)
				}
			}
			checkRepostBasics(t, c.cs, o, got, st)
		})
	}
}

func quoteBodies(bs []string) string {
	if bs == nil {
		return "  (nil)"
	}
	var b strings.Builder
	for _, s := range bs {
		fmt.Fprintf(&b, "  %d bytes: %q\n", len(s), s)
	}
	return b.String()
}

// checkRepostBasics checks the invariants that hold for any input: every
// body fits, MaxBodies holds, bodies are nil iff nothing was emitted, the
// reservation never failed, and each Annotate mode ran at most once per
// Change, compact only after a full call.
func checkRepostBasics(t *testing.T, cs []Change, o RepostOptions, bodies []string, st repostStats) {
	t.Helper()
	maxLen := o.MaxLen
	if maxLen <= 0 {
		maxLen = 2000
	}
	for i, b := range bodies {
		if len(b) > maxLen {
			t.Errorf("body %d: %d bytes > MaxLen %d", i, len(b), maxLen)
		}
	}
	if o.MaxBodies > 0 && len(bodies) > o.MaxBodies {
		t.Errorf("%d bodies > MaxBodies %d", len(bodies), o.MaxBodies)
	}
	if (bodies == nil) != (st.emitted == 0) {
		t.Errorf("%d bodies with %d blocks emitted", len(bodies), st.emitted)
	}
	if bodies != nil && len(bodies) == 0 {
		t.Errorf("empty non-nil bodies")
	}
	if st.overflowDropped != 0 {
		t.Errorf("overflow line dropped %d times", st.overflowDropped)
	}
	if st.emitted+st.omitted != len(cs) {
		t.Errorf("emitted %d + omitted %d != %d changes", st.emitted, st.omitted, len(cs))
	}
	for i := range cs {
		if st.annotate[i] > 1 || st.compact[i] > 1 || st.compact[i] > st.annotate[i] {
			t.Errorf("change %d: %d full and %d compact Annotate calls", i, st.annotate[i], st.compact[i])
		}
		if o.Annotate == nil && st.annotate[i] != 0 {
			t.Errorf("change %d: Annotate calls counted without Annotate", i)
		}
	}
}

func TestRepostZeroOptions(t *testing.T) {
	ch := func(u string, spoiler bool) Change {
		return Change{Link: Link{URL: u, Kind: Bare, Spoiler: spoiler}, Result: linkclean.Result{URL: u, Changed: true}}
	}
	got := Repost([]Change{ch("https://a.com/x", false), ch("https://b.com/y|", true)}, RepostOptions{})
	if want := []string{"<https://a.com/x>\n||<https://b.com/y%7C>||"}; !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	// MaxLen 0 means 2000: a 2000-byte line fits, a 2001-byte one does not.
	long := "https://a.com/" + strings.Repeat("x", 2000-2-14)
	if got := Repost([]Change{ch(long, false)}, RepostOptions{}); len(got) != 1 || len(got[0]) != 2000 {
		t.Errorf("2000-byte line: %d bodies", len(got))
	}
	if got := Repost([]Change{ch(long+"x", false)}, RepostOptions{}); got != nil {
		t.Errorf("2001-byte line: %d bodies, want nil", len(got))
	}
	// No body limit: 30 lines of about 100 bytes split into two bodies.
	var cs []Change
	for i := range 30 {
		cs = append(cs, ch(fmt.Sprintf("https://a.com/%02d/%s", i, strings.Repeat("x", 80)), false))
	}
	if got := Repost(cs, RepostOptions{}); len(got) != 2 || strings.Count(got[0], "\n")+strings.Count(got[1], "\n") != 28 {
		t.Errorf("30 links: %d bodies", len(got))
	}
	if got := Repost(nil, RepostOptions{}); got != nil {
		t.Errorf("no changes: %q", got)
	}
	// Negative values count as their defaults.
	if got := Repost([]Change{ch("https://a.com/x", false)}, RepostOptions{MaxLen: -1, MaxBodies: -1, Unexamined: -5, Overflow: func(o, u int) string { return fmt.Sprintf("-O %d %d", o, u) }}); !slices.Equal(got, []string{"<https://a.com/x>"}) {
		t.Errorf("negative options: %q", got)
	}
}

func TestRepostOverflowCalls(t *testing.T) {
	cs := []Change{
		{Result: linkclean.Result{URL: "https://a.com/x", Changed: true}},
		{Result: linkclean.Result{URL: "https://b.com/y", Changed: true}},
	}
	for _, u := range []int{0, 3} {
		var calls []int
		o := RepostOptions{Unexamined: u, Overflow: func(o, _ int) string { calls = append(calls, o); return "-O" }}
		Repost(cs, o)
		want := []int{1, 2}
		if u > 0 {
			want = []int{0, 1, 2}
		}
		if !slices.Equal(calls, want) {
			t.Errorf("Unexamined %d: Overflow called for %v, want %v", u, calls, want)
		}
	}
	called := false
	Repost(nil, RepostOptions{Unexamined: 3, Overflow: func(int, int) string { called = true; return "-O" }})
	if called {
		t.Errorf("Overflow called with no changes")
	}
}

func TestRepostDoesNotMutate(t *testing.T) {
	lines := []string{"-# a\r\nb", "-# c\nd"}
	keep := slices.Clone(lines)
	cs := []Change{{Result: linkclean.Result{URL: "https://a.com/x", Changed: true}}}
	got := Repost(cs, RepostOptions{Wrap: WrapNone, Annotate: func(Change, bool) ([]string, int) { return lines, 2 }})
	if want := []string{"https://a.com/x\n-# ab\n-# cd"}; !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if !slices.Equal(lines, keep) {
		t.Errorf("Annotate's slice was changed: %q", lines)
	}
}

// TestProcessRepostR8 is the §15.3 R8 shape end to end, with synthetic
// wording: 12 distinct links, of which Process examines 10, and a reply
// that shows as many blocks as fit and counts the rest in its overflow line.
func TestProcessRepostR8(t *testing.T) {
	c, err := linkclean.New(linkclean.Config{
		Sources: []linkclean.RuleSet{},
		Extra:   []linkclean.Rule{{ID: "synthetic", Action: linkclean.Strip, Include: []string{"*://*/*"}, Params: []string{"lcsyn"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var links []string
	for i := range 12 {
		links = append(links, fmt.Sprintf("https://a.com/p%c/%s?lcsyn=1", 'a'+i, strings.Repeat("x", 183)))
	}
	content := "look: " + strings.Join(links, " and ")
	var cs []Change
	var unexamined int
	skipIfNotImplemented(t, func() { cs, unexamined = Process(context.Background(), content, Offline(c), Options{}) })
	if len(cs) != 10 || unexamined != 2 {
		t.Fatalf("Process: %d changes, %d unexamined; want 10, 2", len(cs), unexamined)
	}
	for i, ch := range cs {
		if want := strings.TrimSuffix(links[i], "?lcsyn=1"); ch.Result.URL != want {
			t.Fatalf("change %d: %q, want %q", i, ch.Result.URL, want)
		}
	}
	o := RepostOptions{
		Wrap:       WrapNone,
		MaxBodies:  1,
		Unexamined: unexamined,
		Annotate: func(c Change, _ bool) ([]string, int) {
			return []string{"-# removed " + strconv.Itoa(len(c.Result.Removed)) + " " + strings.Repeat("n", 16)}, 1
		},
		Overflow: func(omitted, unexamined int) string {
			return "-# … and " + strconv.Itoa(omitted) + " more · " + strconv.Itoa(unexamined) + " not checked"
		},
	}
	bodies := Repost(cs, o)
	if len(bodies) != 1 {
		t.Fatalf("%d bodies, want 1", len(bodies))
	}
	b := bodies[0]
	lines := strings.Split(b, "\n")
	// Blocks are 200 + 1 + 29 = 230 bytes; the reservation is that of
	// "… and 10 more · 2 not checked", so 8 blocks fit and 2 are omitted.
	if got, want := lines[len(lines)-1], "-# … and 2 more · 2 not checked"; got != want {
		t.Errorf("overflow line %q, want %q", got, want)
	}
	if n := strings.Count(b, "https://"); n != 8 || len(b) > 2000 {
		t.Errorf("%d URL lines in %d bytes, want 8 in ≤ 2000", n, len(b))
	}
	t.Logf("R8 body: %d bytes", len(b))
}
