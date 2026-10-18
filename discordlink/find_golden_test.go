package discordlink

import (
	"fmt"
	"math/rand/v2"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// findCase is one section of testdata/find.txtar.
type findCase struct {
	name       string
	line       int
	in         string
	shape      string // "<name> <n>" for a shape row
	out        []Link
	unverified []marker
	uncapped   []Link // the uncapped grammar's answer, when it differs
	hasUncap   bool
}

type marker struct{ id, note string }

// unverifiedIDs are the grammar points the private-channel harness settles.
var unverifiedIDs = map[string]string{
	"U1": "is ')' balanced?",
	"U2": "do masked links embed?",
	"U3": "upper-case scheme",
	"U4": "server unfurl vs client link",
	"L1": "label filter",
	"L2": "[l](<url) kind",
	"L3": "a non-http target is consumed",
	"A1": "whitespace in an autolink target",
	"B1": "block rules",
}

func readTestdata(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// loadFindCases parses testdata/find.txtar (format in its header).
func loadFindCases(t testing.TB) []findCase {
	t.Helper()
	_, files := parseTxtar(readTestdata(t, "find.txtar"))
	if len(files) == 0 {
		t.Fatal("find.txtar: no cases")
	}
	seen := map[string]bool{}
	var cases []findCase
	for _, f := range files {
		c := findCase{name: f.Name, line: f.Line}
		if seen[c.name] {
			t.Fatalf("find.txtar:%d: duplicate case %q", f.Line, c.name)
		}
		seen[c.name] = true
		haveIn := false
		for i, l := range strings.Split(string(f.Data), "\n") {
			bad := func(format string, args ...any) {
				t.Fatalf("find.txtar:%d: %s: %s", f.Line+1+i, c.name, fmt.Sprintf(format, args...))
			}
			switch {
			case l == "" || strings.HasPrefix(l, "# "):
			case strings.HasPrefix(l, "in "):
				v, rest, err := parseStrExpr(l[3:])
				if err != nil || rest != "" || haveIn {
					bad("bad or repeated in line (%v, rest %q)", err, rest)
				}
				c.in, haveIn = v, true
			case strings.HasPrefix(l, "shape "):
				fs := strings.Fields(l)
				if len(fs) != 3 {
					bad("bad shape line %q", l)
				}
				n, err := strconv.Atoi(fs[2])
				sh, ok := shapeByName(fs[1])
				if err != nil || !ok || n < minShapeLen || haveIn {
					bad("bad shape line %q", l)
				}
				c.in, c.shape, haveIn = sh.gen(n), fs[1]+" "+fs[2], true
			case strings.HasPrefix(l, "out "):
				lk, err := parseLink(l[4:])
				if err != nil {
					bad("%v", err)
				}
				c.out = append(c.out, lk)
			case strings.HasPrefix(l, "// unverified: "):
				id, note, _ := strings.Cut(l[len("// unverified: "):], " ")
				c.unverified = append(c.unverified, marker{id, strings.TrimSpace(note)})
			case l == "// uncapped: none":
				c.hasUncap = true
			case strings.HasPrefix(l, "// uncapped: "):
				lk, err := parseLink(l[len("// uncapped: "):])
				if err != nil {
					bad("%v", err)
				}
				c.uncapped, c.hasUncap = append(c.uncapped, lk), true
			case strings.HasPrefix(l, "// verified "):
				// A marker the harness settled: kept as a record.
			default:
				bad("unknown line %q", l)
			}
		}
		if !haveIn {
			t.Fatalf("find.txtar:%d: %s: no in or shape line", f.Line, c.name)
		}
		cases = append(cases, c)
	}
	return cases
}

// oracleRow is one section of testdata/oracle.txtar.
type oracleRow struct {
	id, in    string
	line      int
	raw, want []Link
}

func loadOracle(t testing.TB) (header string, rows []oracleRow) {
	t.Helper()
	comment, files := parseTxtar(readTestdata(t, "oracle.txtar"))
	for _, f := range files {
		r := oracleRow{id: f.Name, line: f.Line}
		haveIn, same, none := false, false, false
		for i, l := range strings.Split(string(f.Data), "\n") {
			bad := func(err any) { t.Fatalf("oracle.txtar:%d: %s: %v", f.Line+1+i, r.id, err) }
			switch {
			case l == "" || strings.HasPrefix(l, "# "):
			case strings.HasPrefix(l, "in "):
				v, rest, err := parseStrExpr(l[3:])
				if err != nil || rest != "" || haveIn {
					bad(fmt.Sprintf("bad or repeated in line (%v)", err))
				}
				r.in, haveIn = v, true
			case strings.HasPrefix(l, "raw "):
				lk, err := parseLink(l[4:])
				if err != nil {
					bad(err)
				}
				r.raw = append(r.raw, lk)
			case l == "want same":
				same = true
			case l == "want none":
				none = true
			case strings.HasPrefix(l, "want "):
				lk, err := parseLink(l[5:])
				if err != nil {
					bad(err)
				}
				r.want = append(r.want, lk)
			default:
				bad(fmt.Sprintf("unknown line %q", l))
			}
		}
		switch {
		case !haveIn:
			t.Fatalf("oracle.txtar:%d: %s: no in line", f.Line, r.id)
		case same && (none || r.want != nil || r.raw == nil):
			t.Fatalf("oracle.txtar:%d: %s: want same needs raw lines and no other want", f.Line, r.id)
		case none && (r.want != nil || r.raw == nil):
			t.Fatalf("oracle.txtar:%d: %s: want none needs raw lines and no other want", f.Line, r.id)
		case !same && !none && r.raw != nil && r.want == nil:
			t.Fatalf("oracle.txtar:%d: %s: raw lines without want", f.Line, r.id)
		}
		if same {
			r.want = r.raw
		}
		rows = append(rows, r)
	}
	return string(comment), rows
}

// formatLinks renders links without offsets, one per line.
func formatLinks(ls []Link) string {
	var b strings.Builder
	for _, l := range ls {
		b.WriteString(formatLink(l))
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		return "(none)\n"
	}
	return b.String()
}

// findStatus reports whether Find implements the grammar. Until it does,
// Find is a placeholder that knows only bare links and spoilers (or a stub
// that panics "not implemented"), and the checks against it skip; the
// reference checks still run.
var findStatus = sync.OnceValues(func() (ok bool, why string) {
	defer func() {
		if e := recover(); e != nil {
			if s, isStr := e.(string); isStr && strings.Contains(s, "not implemented") {
				ok, why = false, "Find is not implemented yet"
				return
			}
			panic(e)
		}
	}()
	// The placeholder reports both of these as bare links; the grammar
	// reports one Suppressed link.
	got := formatLinks(Find("<https://a.com/x> `https://b.com/y`"))
	if got == formatLinks([]Link{{Kind: Bare, URL: "https://a.com/x>"}, {Kind: Bare, URL: "https://b.com/y`"}}) {
		return false, "Find is still the placeholder (bare links and spoilers only)"
	}
	return true, ""
})

// requireFind skips t unless Find implements the grammar.
func requireFind(t testing.TB) {
	t.Helper()
	if ok, why := findStatus(); !ok {
		t.Skip(why)
	}
}

// checkLinks checks the invariants every Find result satisfies (FuzzFind's
// list).
func checkLinks(t testing.TB, content string, links []Link) {
	t.Helper()
	inside := map[int]bool{} // positions strictly inside a valid multi-byte rune
	for p := 0; p < len(content); {
		r, n := utf8.DecodeRuneInString(content[p:])
		if r != utf8.RuneError || n > 1 {
			for q := p + 1; q < p+n; q++ {
				inside[q] = true
			}
		}
		p += n
	}
	prevEnd := 0
	for i, l := range links {
		switch {
		case l.Start < prevEnd || l.Start >= l.End || l.End > len(content):
			t.Fatalf("link %d %+v: out of bounds, empty or overlapping (previous end %d, len %d)", i, l, prevEnd, len(content))
		case content[l.Start:l.End] != l.URL:
			t.Fatalf("link %d %+v: content[Start:End] = %q", i, l, content[l.Start:l.End])
		case !strings.HasPrefix(l.URL, "http://") && !strings.HasPrefix(l.URL, "https://"):
			t.Fatalf("link %d %+v: not http(s)", i, l)
		case l.Kind < Bare || l.Kind > MaskedSuppressed:
			t.Fatalf("link %d %+v: bad kind", i, l)
		case l.Label != "" && l.Kind != Masked && l.Kind != MaskedSuppressed:
			t.Fatalf("link %d %+v: label on an unmasked link", i, l)
		case inside[l.Start] || inside[l.End]:
			t.Fatalf("link %d %+v: an edge falls inside a UTF-8 sequence", i, l)
		}
		prevEnd = l.End
	}
}

// refMaxBytes bounds the inputs the uncapped reference runs on in tests: a
// 4 KB '*' run nests about 2,000 deep uncapped, which the naive reference
// parses in quadratic time per level.
const refMaxBytes = 1024

func TestFindGolden(t *testing.T) {
	cases := loadFindCases(t)
	t.Run("ref", func(t *testing.T) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel() // the naive reference is slow on the 4 KB shapes
				got := findRef(c.in, maxDepth)
				checkLinks(t, c.in, got)
				if g, w := formatLinks(got), formatLinks(c.out); g != w {
					t.Errorf("find.txtar:%d: %s: findRef(in, %d):\n%swant:\n%s", c.line, c.name, maxDepth, g, w)
				}
				want := c.out
				if c.hasUncap {
					want = c.uncapped
				} else if len(c.in) > refMaxBytes && c.shape != "" {
					return
				}
				if g, w := formatLinks(findRef(c.in, 0)), formatLinks(want); g != w {
					t.Errorf("find.txtar:%d: %s: findRef(in, 0) (uncapped):\n%swant:\n%s", c.line, c.name, g, w)
				}
			})
		}
	})
	t.Run("find", func(t *testing.T) {
		requireFind(t)
		for _, c := range cases {
			got := Find(c.in)
			checkLinks(t, c.in, got)
			if g, w := formatLinks(got), formatLinks(c.out); g != w {
				t.Errorf("find.txtar:%d: %s: Find:\n%swant:\n%s", c.line, c.name, g, w)
			}
		}
	})
}

func TestFindOracle(t *testing.T) {
	header, rows := loadOracle(t)
	if len(rows) < 1000 {
		t.Fatalf("oracle.txtar: %d rows, want about 1,500", len(rows))
	}
	if !regexp.MustCompile(`(?m)^simple-markdown: version \d+\.\d+\.\d+, commit [0-9a-f]{40}, simple-markdown\.js sha256 [0-9a-f]{64}$`).MatchString(header) {
		t.Error("oracle.txtar: the header does not pin simple-markdown's version, commit and sha256")
	}
	gen, linked := 0, 0
	for _, r := range rows {
		if strings.HasPrefix(r.id, "t") {
			gen++
			if r.raw != nil {
				linked++
			}
		}
	}
	if gen == 0 || linked*10 < gen*7 {
		t.Errorf("oracle.txtar: %d of %d generated rows have links, want at least 70%%", linked, gen)
	}
	t.Run("ref", func(t *testing.T) {
		for _, r := range rows {
			w := formatLinks(r.want)
			for _, d := range []int{0, maxDepth} {
				got := findRef(r.in, d)
				checkLinks(t, r.in, got)
				if g := formatLinks(got); g != w {
					t.Errorf("oracle.txtar:%d: %s %q: findRef(in, %d):\n%swant:\n%s", r.line, r.id, r.in, d, g, w)
				}
			}
		}
	})
	t.Run("find", func(t *testing.T) {
		requireFind(t)
		for _, r := range rows {
			got := Find(r.in)
			checkLinks(t, r.in, got)
			if g, w := formatLinks(got), formatLinks(r.want); g != w {
				t.Errorf("oracle.txtar:%d: %s %q: Find:\n%swant:\n%s", r.line, r.id, r.in, g, w)
			}
		}
	})
}

// agreeFragments are the template generator's pieces for TestFindAgree:
// schemes, hosts and paths, every delimiter the grammar knows, mention
// shapes, and whitespace the URL rule treats specially.
var agreeFragments = [][]string{
	{"https://", "http://", "HTTPS://", "https:/", "<https://", "](https://", "](<https://"},
	{"a.com", "b.org/p", "x.io/a_b", "c.net/(z)", "d.com/?q=1&r=2", "e.co/%7C", "f.com/x.", "g.com/\u00e9", "h.com/x)"},
	{"(", ")", "[", "]", "<", ">", "||", "**", "__", "~~", "*", "_", "~", "|", "\\", "`", "``", "```", "\"", "'", "](", "](<", ">)", " \"t\")", ":", "/"},
	{"<@1>", "<@!1>", "<@&1>", "<#1>", "<:a_b:1>", "<a:x:1>", "<t:1:R>", "</a b:1>", "<1:browse>", "-# ", "> ", "# "},
	{"a", "word", "1", "#", "-", " ", " ", "\n", "\t", "\u00a0", "\u200b", "\u3000", "\u00e9", "\U0001f600", "\xff"},
}

// agreeInputs returns n generated inputs, deterministic for a seed.
func agreeInputs(seed uint64, n int) []string {
	rng := rand.New(rand.NewPCG(seed, 0x6c696e6b))
	out := make([]string, 0, n)
	for range n {
		var b strings.Builder
		for range 2 + rng.IntN(14) {
			switch g := rng.IntN(10); {
			case g < 3:
				b.WriteString(agreeFragments[0][rng.IntN(len(agreeFragments[0]))])
				b.WriteString(agreeFragments[1][rng.IntN(len(agreeFragments[1]))])
			case g < 7:
				b.WriteString(agreeFragments[2][rng.IntN(len(agreeFragments[2]))])
			case g < 8:
				b.WriteString(agreeFragments[3][rng.IntN(len(agreeFragments[3]))])
			default:
				b.WriteString(agreeFragments[4][rng.IntN(len(agreeFragments[4]))])
			}
		}
		out = append(out, b.String())
	}
	return out
}

// TestFindRefInvariants checks the reference against Find's invariants on
// the generated corpus, whether or not Find is ready.
func TestFindRefInvariants(t *testing.T) {
	n := 10000
	if raceEnabled {
		n = 1000 // the regexp-based reference is slow under the race detector
	}
	linked := 0
	for i, in := range agreeInputs(1, n) {
		got := findRef(in, maxDepth)
		checkLinks(t, in, got)
		if len(got) > 0 {
			linked++
		}
		if i%10 == 0 && !slices.Equal(got, findRef(in, maxDepth)) {
			t.Fatalf("findRef(%q) is not deterministic", in)
		}
	}
	if linked < n/2 {
		t.Errorf("only %d of %d generated inputs have links", linked, n)
	}
}

// TestFindAgree compares Find with the reference, offsets included, on every
// golden and oracle input and 10,000 generated ones (1,000 under -race).
func TestFindAgree(t *testing.T) {
	requireFind(t)
	var inputs []string
	for _, c := range loadFindCases(t) {
		inputs = append(inputs, c.in)
	}
	_, rows := loadOracle(t)
	for _, r := range rows {
		inputs = append(inputs, r.in)
	}
	n := 10000
	if raceEnabled {
		n = 1000 // the regexp-based reference is slow under the race detector
	}
	inputs = append(inputs, agreeInputs(1, n)...)
	fails := 0
	for _, in := range inputs {
		got, want := Find(in), findRef(in, maxDepth)
		if !slices.Equal(got, want) {
			if fails++; fails <= 20 {
				t.Errorf("%q:\nFind    %+v\nfindRef %+v", in, got, want)
			}
		}
	}
	if fails > 0 {
		t.Errorf("%d of %d inputs disagree", fails, len(inputs))
	}
}

// TestUnverifiedInventory checks the "// unverified:" markers in find.txtar:
// every ID is known and every marker has a note. The IDs in use must be
// exactly the entries of doc.go's "Unverified grammar" section, so a marker
// the harness settles takes its doc entry with it.
func TestUnverifiedInventory(t *testing.T) {
	used := map[string][]string{}
	for _, c := range loadFindCases(t) {
		for _, m := range c.unverified {
			if _, ok := unverifiedIDs[m.id]; !ok {
				t.Errorf("find.txtar:%d: %s: unknown unverified ID %q", c.line, c.name, m.id)
			}
			if m.note == "" {
				t.Errorf("find.txtar:%d: %s: marker %s has no note", c.line, c.name, m.id)
			}
			used[m.id] = append(used[m.id], c.name)
		}
	}
	ids := slices.Sorted(func(yield func(string) bool) {
		for id := range used {
			if !yield(id) {
				return
			}
		}
	})
	for _, id := range ids {
		t.Logf("%s (%s): %d cases: %s", id, unverifiedIDs[id], len(used[id]), strings.Join(used[id], ", "))
	}

	doc, err := os.ReadFile("doc.go")
	if err != nil {
		t.Fatal(err)
	}
	_, sec, ok := strings.Cut(string(doc), "\n// # Unverified grammar\n")
	if !ok {
		t.Fatal("doc.go: no Unverified grammar section")
	}
	if i := strings.Index(sec, "\n// # "); i >= 0 {
		sec = sec[:i]
	}
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^//   - ([A-Z][0-9]): `).FindAllStringSubmatch(sec, -1) {
		documented[m[1]] = true
	}
	for _, id := range ids {
		if !documented[id] {
			t.Errorf("doc.go: Unverified grammar has no entry for %s, which find.txtar uses", id)
		}
	}
	for id := range documented {
		if used[id] == nil {
			t.Errorf("doc.go: Unverified grammar documents %s, which no find.txtar case uses", id)
		}
	}
}
