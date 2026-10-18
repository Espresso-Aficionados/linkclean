package discordlink

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Espresso-Aficionados/linkclean"
)

// The private-channel harness (tools/discordharness) settles the grammar
// points find.txtar marks as unverified and checks the reply format, by
// sending messages to a private Discord channel. This file builds what it
// sends: TestHarnessPayloads always builds and checks every payload in
// memory, and with -harness-out it writes them to payloads.json for the
// sender. Nothing here touches the network.
//
//	go test ./discordlink -run '^TestHarnessPayloads$' -harness-out "$DIR"
var harnessOut = flag.String("harness-out", "", "TestHarnessPayloads: absolute directory, outside any git work tree, to write payloads.json to")

// harnessFile is payloads.json. tools/discordharness reads the same shape;
// the two halves share no code, so a change here needs one there.
type harnessFile struct {
	Version int           `json:"version"`
	Cases   []harnessCase `json:"cases"`
}

// harnessCase is one message.
type harnessCase struct {
	ID     string `json:"id"`
	Phase  string `json:"phase"`  // "grammar", "reply" or "bidi"
	Source string `json:"source"` // where the content comes from
	// Markers are the find.txtar unverified markers ("U1 <note>") the case
	// settles.
	Markers []string `json:"markers,omitempty"`
	Content string   `json:"content"`
	Flags   int      `json:"flags"`              // message flags: 0, or 4 (SUPPRESS_EMBEDS)
	ReplyTo string   `json:"reply_to,omitempty"` // ID of an earlier case to reply to
	// UserCopy asks the maintainer to also paste the content by hand from a
	// user account and compare the rendering.
	UserCopy bool `json:"user_copy,omitempty"`
	// ExpectNoEmbeds: the API must report no embeds once the poll ends.
	ExpectNoEmbeds bool          `json:"expect_no_embeds,omitempty"`
	Predicted      []harnessLink `json:"predicted"` // Find(Content)
}

type harnessLink struct {
	URL        string `json:"url"`
	Kind       string `json:"kind"`
	Spoiler    bool   `json:"spoiler,omitempty"`
	Label      string `json:"label,omitempty"`
	WouldEmbed bool   `json:"would_embed"`
}

const (
	harnessMaxContent = 2000 // a bot message's content limit, counted here in bytes
	harnessRepo       = "github.com/Espresso-Aficionados/linkclean"
	harnessOriginal   = "R-0"
)

// harnessTestHosts are the hosts the testdata uses. Several are real
// domains, so every one is rewritten before anything is sent (Discord's
// unfurler would fetch them).
var harnessTestHosts = []string{"a.com", "b.com", "c.com", "h.com", "b.org", "c.net", "d.com", "e.co", "f.com", "g.com", "x.io"}

// harnessWikiPaths are the U1 golden URLs that need a real page with a
// balanced ')' in its path, so the embed shows where Discord ended the link.
var harnessWikiPaths = []string{"en.wikipedia.org/wiki/Foo_(bar)", "a.com/wiki/Foo_(bar)"}

// harnessRewrite rewrites the host after every "://" in s:
//   - a U1 "Foo_(bar)" URL becomes en.wikipedia.org/wiki/Mercury_(planet);
//   - with embed, a test host becomes this repository's URL with a query
//     string naming the case and the host (a page that unfurls reliably,
//     distinct per case so Discord's embed cache cannot answer for another);
//   - otherwise a test host h becomes example.com/h.
//
// Only letters, digits, '.', '/', '?', '=' and '-' are inserted, and the
// host's last character stays in place, so the grammar sees the same
// delimiters; TestHarnessPayloads checks that Find's answer maps exactly.
func harnessRewrite(s, id string, embed bool) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "://")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i+3])
		s = s[i+3:]
		done := false
		for _, p := range harnessWikiPaths {
			if rest, ok := strings.CutPrefix(s, p); ok {
				b.WriteString("en.wikipedia.org/wiki/Mercury_(planet)")
				s, done = rest, true
				break
			}
		}
		for _, h := range harnessTestHosts {
			if done {
				break
			}
			if rest, ok := strings.CutPrefix(s, h); ok {
				if embed {
					b.WriteString(harnessRepo + "?h=" + id + "-" + h)
				} else {
					b.WriteString("example.com/" + h)
				}
				s, done = rest, true
			}
		}
	}
}

// harnessGrammarRows are the find.txtar rows that pin backtracking, escape,
// nesting and line-crossing details, sent whether or not they carry a marker.
var harnessGrammarRows = []string{
	"em-wins-by-quality", "em-odd-run-closes", "strong-escaped-closer", "code-opener-backtracks",
	"spoiler-ignores-escapes", "strong-ignores-code", "masked-greedy-label", "masked-href-backtracking",
	"masked-empty-target-title", "em-star-escaped-space", "del-not-before-underscore", "masked-open-angle-only",
	"subtext-spoiler-across-lines", "code-crosses-lines", "autolink-newline", "mention-emoji-underscore",
	"cap-star-wrap-34", "cap-star-wrap-36", "cap-star-wrap-40",
	"cap-star-wrap-spoiler-34", "cap-star-wrap-spoiler-35", "cap-star-wrap-spoiler-36",
}

// harnessEmbedMarkers are the unverified points whose answer shows in the
// embed (or its absence), so their cases link to pages that unfurl.
var harnessEmbedMarkers = map[string]bool{"U1": true, "U2": true, "U4": true}

// harnessControls are known-good shapes, sent first.
var harnessControls = []struct {
	name, in string
	want     []Link
}{
	{"bare link", "https://a.com/bare", []Link{{URL: "https://a.com/bare", Kind: Bare}}},
	{"suppressed link", "<https://a.com/angle>", []Link{{URL: "https://a.com/angle", Kind: Suppressed}}},
	{"masked link", "[control](https://a.com/masked)", []Link{{URL: "https://a.com/masked", Kind: Masked, Label: "control"}}},
	{"spoilered link", "||https://a.com/spoiler||", []Link{{URL: "https://a.com/spoiler", Kind: Bare, Spoiler: true}}},
}

// harnessOracleClasses pick link-bearing generated oracle rows, two per
// rule class, in file order. The paired rules are picked by Find's answer
// (a masked, suppressed or spoilered link), the others by the input.
var harnessOracleClasses = []struct {
	name string
	has  func(in string, want []Link) bool
}{
	{"masked", harnessWantKind(func(l Link) bool { return l.Kind == Masked || l.Kind == MaskedSuppressed })},
	{"autolink", harnessWantKind(func(l Link) bool { return l.Kind == Suppressed })},
	{"spoiler", harnessWantKind(func(l Link) bool { return l.Spoiler })},
	{"strong", harnessHas("**")},
	{"em *", func(s string, _ []Link) bool { return strings.Contains(s, "*") && !strings.Contains(s, "**") }},
	{"underline", harnessHas("__")},
	{"em _", func(s string, _ []Link) bool { return strings.Contains(s, "_") && !strings.Contains(s, "__") }},
	{"del", harnessHas("~~")},
	{"code", harnessHas("`")},
	{"escape", harnessHas(`\`)},
	{"mention", harnessHas("<@", "<#")},
	{"emoji or timestamp", harnessHas("<:", "<t:")},
	{"paren", harnessHas(")")},
	{"line break", harnessHas("\n")},
	{"unicode space", harnessHas("\u00a0", "\u200b", "\u3000")},
}

func harnessHas(subs ...string) func(string, []Link) bool {
	return func(s string, _ []Link) bool {
		return slices.ContainsFunc(subs, func(sub string) bool { return strings.Contains(s, sub) })
	}
}

func harnessWantKind(f func(Link) bool) func(string, []Link) bool {
	return func(_ string, want []Link) bool { return slices.ContainsFunc(want, f) }
}

// harnessSendable reports why s cannot be sent as is, or "". JSON cannot
// carry invalid UTF-8, Discord's client rewrites CR and form feed before
// parsing (tools/discordref's README), so such an input would not test what
// Find predicts, and a message holds at most 2000 bytes here. A tab is
// rewritten too (to four spaces) but is allowed: whether Discord keeps it
// inside an autolink is marker A1's question. The oracle selection skips
// tabs.
func harnessSendable(s string) string {
	switch {
	case !utf8.ValidString(s):
		return "invalid UTF-8"
	case strings.ContainsAny(s, "\r\f"):
		return "CR or form feed"
	case len(s) > harnessMaxContent:
		return fmt.Sprintf("%d bytes", len(s))
	}
	return ""
}

// harnessSameLinks checks that Find's answer for the rewritten content is
// its answer for the original, rewritten.
func harnessSameLinks(orig, got []Link, id string, embed bool) error {
	if len(orig) != len(got) {
		return fmt.Errorf("%d links, the original has %d", len(got), len(orig))
	}
	for i, o := range orig {
		g := got[i]
		if g.URL != harnessRewrite(o.URL, id, embed) || g.Kind != o.Kind || g.Spoiler != o.Spoiler || g.Label != harnessRewrite(o.Label, id, embed) {
			return fmt.Errorf("link %d is %s, the original %s", i, formatLink(g), formatLink(o))
		}
	}
	return nil
}

func harnessPredict(content string) []harnessLink {
	out := []harnessLink{}
	for _, l := range Find(content) {
		out = append(out, harnessLink{URL: l.URL, Kind: kindName(l.Kind), Spoiler: l.Spoiler, Label: l.Label, WouldEmbed: l.WouldEmbed()})
	}
	return out
}

// harnessGrammar builds Phase G: controls, every find.txtar row with a
// marker, the harnessGrammarRows, and two oracle rows per rule class.
func harnessGrammar(t *testing.T) []harnessCase {
	var out []harnessCase
	add := func(source, in string, markers []string, embed bool, orig []Link) {
		id := fmt.Sprintf("G%02d", len(out)+1)
		content := id + "\n" + harnessRewrite(in, id, embed)
		if err := harnessSameLinks(orig, Find(content), id, embed); err != nil {
			t.Errorf("%s (%s): rewritten content %q: %v", id, source, content, err)
		}
		out = append(out, harnessCase{
			ID: id, Phase: "grammar", Source: source, Markers: markers, Content: content,
			UserCopy: len(markers) > 0, Predicted: harnessPredict(content),
		})
	}
	for _, c := range harnessControls {
		if got := Find(c.in); formatLinks(got) != formatLinks(c.want) {
			t.Errorf("control %s: Find(%q) =\n%swant\n%s", c.name, c.in, formatLinks(got), formatLinks(c.want))
		}
		add("control: "+c.name, c.in, nil, true, c.want)
	}

	cases := loadFindCases(t)
	byName := map[string]findCase{}
	for _, c := range cases {
		byName[c.name] = c
	}
	listed, sent := map[string]bool{}, map[string]bool{}
	for _, n := range harnessGrammarRows {
		if _, ok := byName[n]; !ok {
			t.Errorf("harnessGrammarRows: find.txtar has no case %q", n)
		}
		listed[n] = true
	}
	for _, c := range cases {
		if len(c.unverified) == 0 && !listed[c.name] {
			continue
		}
		var markers []string
		embed := false
		for _, m := range c.unverified {
			markers = append(markers, m.id+" "+m.note)
			embed = embed || harnessEmbedMarkers[m.id]
		}
		if why := harnessSendable(c.in); why != "" || c.shape != "" {
			t.Errorf("find.txtar:%d: %s must be sent but cannot be (%s%s)", c.line, c.name, why, c.shape)
			continue
		}
		add("find.txtar: "+c.name, c.in, markers, embed, c.out)
		sent[c.in] = true
	}

	// Generated rows first; a class the generator rarely produces (masked
	// links) is topped up with golden rows not already sent.
	_, rows := loadOracle(t)
	rows = slices.Concat(
		slices.DeleteFunc(slices.Clone(rows), func(r oracleRow) bool { return !strings.HasPrefix(r.id, "t") }),
		slices.DeleteFunc(slices.Clone(rows), func(r oracleRow) bool { return !strings.HasPrefix(r.id, "g") || sent[r.in] }))
	picked := map[string]bool{}
	for _, cl := range harnessOracleClasses {
		n := 0
		for _, r := range rows {
			if n == 2 {
				break
			}
			if len(r.want) == 0 || picked[r.id] || !cl.has(r.in, r.want) || harnessSendable(r.in) != "" || strings.Contains(r.in, "\t") {
				continue
			}
			// A row whose answer the rewrite would change is skipped, not
			// failed: the generated rows are not curated for it.
			if harnessSameLinks(r.want, Find("G00\n"+harnessRewrite(r.in, "G00", false)), "G00", false) != nil {
				continue
			}
			picked[r.id] = true
			n++
			add("oracle.txtar: "+r.id+" ("+cl.name+")", r.in, nil, false, r.want)
		}
		if n < 2 {
			t.Errorf("oracle.txtar: only %d sendable rows for class %s", n, cl.name)
		}
	}
	return out
}

// harnessEscAlnum is §15.3's search-query escaper: every byte other than
// A-Z, a-z and 0-9 becomes %XX.
func harnessEscAlnum(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// harnessAnnotate is an Annotate shaped like §15.3's line grammar: a
// what-line with the tracking names ('_', '[' and ']' escaped), then an
// aff-line with one entry per affiliate ID (network, the ID in a code span,
// the DuckDuckGo search link) and the other affiliate names as "also"
// (their count when compact). Both lines are wrapped in ||…|| for a
// spoilered link, and the aff-line is required. The IDs here are fixed and
// pass §15.3's display rule, so the rule itself is not reimplemented.
func harnessAnnotate(c Change, compact bool) ([]string, int) {
	wrap := func(s string) string {
		if c.Link.Spoiler {
			return "||" + s + "||"
		}
		return s
	}
	esc := strings.NewReplacer("_", `\_`, "[", `\[`, "]", `\]`)
	var tracking, entries, also []string
	for _, r := range c.Result.Removed {
		switch {
		case r.Category == linkclean.CatTracker:
			tracking = append(tracking, esc.Replace(r.Name))
		case r.Category == linkclean.CatAffiliate && r.Beneficiary.ID != "":
			id := r.Beneficiary.ID
			entries = append(entries, r.Beneficiary.Network+" `"+id+"` · [search](<https://duckduckgo.com/?q=%22"+harnessEscAlnum(id)+"%22>)")
		case r.Category == linkclean.CatAffiliate:
			also = append(also, esc.Replace(r.Name))
		}
	}
	var lines []string
	if len(tracking) > 0 {
		lines = append(lines, "-# "+wrap("tracking: "+strings.Join(tracking, ", ")))
	}
	if len(entries) == 0 {
		return lines, 0
	}
	aff := "\U0001F4B0 affiliate removed: " + strings.Join(entries, "; ")
	switch {
	case len(also) > 0 && compact:
		aff += fmt.Sprintf(" · also %d parameters", len(also))
	case len(also) > 0:
		aff += " · also " + strings.Join(also, ", ")
	}
	return append(lines, "-# "+wrap(aff)), 1
}

// harnessChange is a changed link as Process would report it.
func harnessChange(input, cleaned string, spoiler bool, removed ...linkclean.Removal) Change {
	return Change{
		Link:   Link{URL: input, Kind: Bare, Spoiler: spoiler},
		Result: linkclean.Result{Input: input, URL: cleaned, Changed: true, Removed: removed},
	}
}

func harnessTracker(name string) linkclean.Removal {
	return linkclean.Removal{Part: linkclean.PartQuery, Category: linkclean.CatTracker, Name: name}
}

func harnessAffiliate(name, id string) linkclean.Removal {
	r := linkclean.Removal{Part: linkclean.PartQuery, Category: linkclean.CatAffiliate, Name: name}
	if id != "" {
		r.Beneficiary = linkclean.Beneficiary{Network: "Amazon Associates", ID: id}
	}
	return r
}

// harnessReply builds Phase R: an original, then replies whose bodies come
// from Repost (WrapNone, MaxBodies 1, harnessAnnotate), sent like §15.3's
// replies: SUPPRESS_EMBEDS, no mentions. R-3b repeats R-3 without the flag,
// to see whether the masked link's angle brackets alone suppress its embed;
// its URL line is WrapAngle, so the search link is the only link that could
// embed.
func harnessReply(t *testing.T) []harnessCase {
	const base = "https://example.com/harness/"
	amazon := func(id string) []linkclean.Removal {
		return []linkclean.Removal{harnessAffiliate("tag", id), harnessAffiliate("linkCode", "")}
	}
	specs := []struct {
		id, source string
		change     Change
		wrap       Wrap
		flags      int
		check      func(body string) error
	}{
		{"R-1", "the -# line renders as small grey subtext, not as a literal -#", harnessChange(base+"R-1?utm_source=x", base+"R-1", false, harnessTracker("utm_source")), WrapNone, 4,
			harnessLines(`^https://\S+$`, `^-# tracking: utm\\_source$`)},
		{"R-2", "the ||…|| inside the subtext line stays hidden until clicked", harnessChange(base+"R-2?utm_source=x", base+"R-2", true, harnessTracker("utm_source")), WrapNone, 4,
			harnessLines(`^\|\|https://\S+\|\|$`, `^-# \|\|tracking: utm\\_source\|\|$`)},
		{"R-3", "search renders as one link, no embed (SUPPRESS_EMBEDS set)", harnessChange(base+"R-3/dp/B0EXAMPLE?tag=somecreator-20&linkCode=ll1", base+"R-3/dp/B0EXAMPLE", false, amazon("somecreator-20")...), WrapNone, 4,
			harnessLines(`^https://\S+$`, "^-# \U0001F4B0 affiliate removed: Amazon Associates `somecreator-20` · "+regexp.QuoteMeta("[search](<https://duckduckgo.com/?q=%22somecreator%2D20%22>)")+" · also linkCode$")},
		{"R-3b", "search renders as one link, no embed, without SUPPRESS_EMBEDS", harnessChange(base+"R-3b/dp/B0EXAMPLE?tag=somecreator-20&linkCode=ll1", base+"R-3b/dp/B0EXAMPLE", false, amazon("somecreator-20")...), WrapAngle, 0,
			harnessLines(`^<https://\S+>$`, "^-# \U0001F4B0 affiliate removed: Amazon Associates `somecreator-20` · "+regexp.QuoteMeta("[search](<https://duckduckgo.com/?q=%22somecreator%2D20%22>)")+" · also linkCode$")},
		{"R-4", "R9: search renders as one link, no embed, no raw _ outside the code span", harnessChange(base+"R-4/dp/B0EXAMPLE?tag=__u__", base+"R-4/dp/B0EXAMPLE", false, harnessAffiliate("tag", "__u__")), WrapNone, 4,
			func(body string) error {
				if err := harnessLines(`^https://\S+$`, "^-# \U0001F4B0 affiliate removed: Amazon Associates `__u__` · "+regexp.QuoteMeta("[search](<https://duckduckgo.com/?q=%22%5F%5Fu%5F%5F%22>)")+"$")(body); err != nil {
					return err
				}
				if strings.Count(body, "_") != 4 {
					return errors.New("a '_' outside the code span")
				}
				return nil
			}},
		{"R-5", "the spoilered URL line stays hidden; its target keeps %7C%7C", harnessChange(base+"R-5?q=a||b&utm_source=x", base+"R-5?q=a||b", true, harnessTracker("utm_source")), WrapNone, 4,
			func(body string) error {
				if err := harnessLines(`^\|\|https://\S+%7C%7Cb\|\|$`, `^-# \|\|tracking: utm\\_source\|\|$`)(body); err != nil {
					return err
				}
				if strings.Count(body, "|") != 8 {
					return errors.New("a raw '|' besides the spoiler bars")
				}
				return nil
			}},
	}
	out := []harnessCase{{
		ID: harnessOriginal, Phase: "reply", Source: "original the R cases reply to",
		Content: harnessOriginal + "\nharness original: the R cases reply to this message", Predicted: []harnessLink{},
	}}
	for _, s := range specs {
		bodies := Repost([]Change{s.change}, RepostOptions{Wrap: s.wrap, MaxBodies: 1, Annotate: harnessAnnotate})
		if len(bodies) != 1 {
			t.Errorf("%s: Repost returned %d bodies, want 1", s.id, len(bodies))
			continue
		}
		if err := s.check(bodies[0]); err != nil {
			t.Errorf("%s: body %q: %v", s.id, bodies[0], err)
		}
		out = append(out, harnessCase{
			ID: s.id, Phase: "reply", Source: s.source, Content: bodies[0], Flags: s.flags,
			ReplyTo: harnessOriginal, ExpectNoEmbeds: true, Predicted: harnessPredict(bodies[0]),
		})
	}
	return out
}

// harnessLines returns a check that body has exactly one line per pattern,
// each matching it.
func harnessLines(patterns ...string) func(string) error {
	return func(body string) error {
		lines := strings.Split(body, "\n")
		if len(lines) != len(patterns) {
			return fmt.Errorf("%d lines, want %d", len(lines), len(patterns))
		}
		for i, p := range patterns {
			if !regexp.MustCompile(p).MatchString(lines[i]) {
				return fmt.Errorf("line %d does not match %s", i+1, p)
			}
		}
		return nil
	}
}

// harnessBidi is Phase B: a raw U+202E on line 1, bypassing Repost, to see
// whether it reorders line 2.
func harnessBidi() []harnessCase {
	content := "abc\u202edef https://example.com/\nghi jkl"
	return []harnessCase{{ID: "B-1", Phase: "bidi", Source: "raw U+202E on line 1: does line 2 render reordered?", Content: content, Predicted: harnessPredict(content)}}
}

func harnessPayloads(t *testing.T) harnessFile {
	f := harnessFile{Version: 1}
	f.Cases = append(f.Cases, harnessGrammar(t)...)
	f.Cases = append(f.Cases, harnessReply(t)...)
	f.Cases = append(f.Cases, harnessBidi()...)
	return f
}

// harnessCheck checks what the sender also refuses, and that no test host
// is left to be fetched.
func harnessCheck(f harnessFile) []error {
	var errs []error
	bad := func(c harnessCase, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", c.ID, fmt.Sprintf(format, args...)))
	}
	hosts := make([]string, len(harnessTestHosts))
	for i, h := range harnessTestHosts {
		hosts[i] = regexp.QuoteMeta(h)
	}
	leftover := regexp.MustCompile(`(?i)://(` + strings.Join(hosts, "|") + `)`)
	seen := map[string]bool{}
	for _, c := range f.Cases {
		switch {
		case c.Content == "" || len(c.Content) > harnessMaxContent:
			bad(c, "content is %d bytes", len(c.Content))
		case !utf8.ValidString(c.Content):
			bad(c, "content is not valid UTF-8")
		case leftover.MatchString(c.Content):
			bad(c, "content links to a test host: %q", c.Content)
		}
		if seen[c.ID] {
			bad(c, "duplicate ID")
		}
		if c.Flags != 0 && c.Flags != 4 {
			bad(c, "flags %d", c.Flags)
		}
		if c.ReplyTo != "" && !seen[c.ReplyTo] {
			bad(c, "replies to %q, which is not an earlier case", c.ReplyTo)
		}
		switch c.Phase {
		case "grammar":
			if first, _, _ := strings.Cut(c.Content, "\n"); first != c.ID || !regexp.MustCompile(`^G\d\d$`).MatchString(c.ID) {
				bad(c, "a grammar case's first line must be its ID G01…G99")
			}
		case "reply", "bidi":
		default:
			bad(c, "unknown phase %q", c.Phase)
		}
		seen[c.ID] = true
	}
	return errs
}

func TestHarnessPayloads(t *testing.T) {
	f := harnessPayloads(t)
	for _, err := range harnessCheck(f) {
		t.Error(err)
	}
	count := map[string]int{}
	for _, c := range f.Cases {
		count[c.Phase]++
	}
	if count["grammar"] < 60 || count["reply"] != 7 || count["bidi"] != 1 {
		t.Errorf("phase sizes %v", count)
	}
	t.Logf("%d cases: %v", len(f.Cases), count)
	if t.Failed() || *harnessOut == "" {
		return
	}
	data, err := harnessMarshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := harnessWrite(*harnessOut, data); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote payloads.json")
}

func harnessMarshal(f harnessFile) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(f)
	return b.Bytes(), err
}

// harnessWrite writes payloads.json to dir, which must be absolute and
// outside any git work tree, so harness output is never committed by
// accident.
func harnessWrite(dir string, data []byte) error {
	if !filepath.IsAbs(dir) {
		return errors.New("-harness-out must be an absolute directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if root, ok := harnessGitTree(dir); ok {
		return fmt.Errorf("-harness-out is inside the git work tree at %s; use a directory outside any repository", root)
	}
	return os.WriteFile(filepath.Join(dir, "payloads.json"), data, 0o600)
}

// harnessGitTree reports the nearest ancestor of dir (itself included,
// symlinks resolved) that holds a .git entry.
func harnessGitTree(dir string) (string, bool) {
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d, true
		}
		if filepath.Dir(d) == d {
			return "", false
		}
	}
}

func TestHarnessRewrite(t *testing.T) {
	for _, c := range []struct {
		in, want string
		embed    bool
	}{
		{"x https://a.com/y", "x https://example.com/a.com/y", false},
		{"HTTPS://b.org http://x.io/p", "HTTPS://example.com/b.org http://example.com/x.io/p", false},
		{"https://a.com/y)", "https://" + harnessRepo + "?h=G07-a.com/y)", true},
		{"(see https://a.com/wiki/Foo_(bar))", "(see https://en.wikipedia.org/wiki/Mercury_(planet))", false},
		{"https://en.wikipedia.org/wiki/Foo_(bar)", "https://en.wikipedia.org/wiki/Mercury_(planet)", true},
		{"https:// a.com steam://run ftp://a.com", "https:// a.com steam://run ftp://example.com/a.com", false},
		{"https://example.org/", "https://example.org/", false},
	} {
		if got := harnessRewrite(c.in, "G07", c.embed); got != c.want {
			t.Errorf("harnessRewrite(%q, %v) = %q, want %q", c.in, c.embed, got, c.want)
		}
	}
}

func TestHarnessCheck(t *testing.T) {
	ok := harnessCase{ID: "G01", Phase: "grammar", Content: "G01\nhttps://example.com/"}
	for _, c := range []struct {
		name string
		cs   []harnessCase
	}{
		{"too long", []harnessCase{{ID: "B-1", Phase: "bidi", Content: strings.Repeat("x", harnessMaxContent+1)}}},
		{"empty", []harnessCase{{ID: "B-1", Phase: "bidi"}}},
		{"invalid UTF-8", []harnessCase{{ID: "B-1", Phase: "bidi", Content: "\xff"}}},
		{"test host", []harnessCase{{ID: "B-1", Phase: "bidi", Content: "HTTPS://d.com/x"}}},
		{"duplicate", []harnessCase{ok, ok}},
		{"flags", []harnessCase{{ID: "B-1", Phase: "bidi", Content: "x", Flags: 1 << 12}}},
		{"reply to later", []harnessCase{{ID: "R-1", Phase: "reply", Content: "x", ReplyTo: "R-0"}, {ID: "R-0", Phase: "reply", Content: "y"}}},
		{"first line", []harnessCase{{ID: "G01", Phase: "grammar", Content: "G02\nx"}}},
		{"phase", []harnessCase{{ID: "X", Phase: "other", Content: "x"}}},
	} {
		if errs := harnessCheck(harnessFile{Cases: c.cs}); len(errs) == 0 {
			t.Errorf("%s: no error", c.name)
		}
	}
	if errs := harnessCheck(harnessFile{Cases: []harnessCase{ok}}); len(errs) != 0 {
		t.Errorf("valid case: %v", errs)
	}
}

func TestHarnessWrite(t *testing.T) {
	tmp := t.TempDir()
	if err := harnessWrite("rel/dir", []byte("{}")); err == nil {
		t.Error("relative directory accepted")
	}
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := harnessWrite(filepath.Join(repo, "out"), []byte("{}")); err == nil {
		t.Error("directory inside a git work tree accepted")
	}
	if root, in := harnessGitTree(tmp); in {
		t.Skipf("the temporary directory is inside the git work tree at %s", root)
	}
	out := filepath.Join(tmp, "out")
	if err := harnessWrite(out, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(out, "payloads.json")); err != nil || string(b) != "{}" {
		t.Errorf("payloads.json = %q, %v", b, err)
	}
}

// TestHarnessPayloadsJSON checks the encoded file round-trips and carries
// every field the sender reads.
func TestHarnessPayloadsJSON(t *testing.T) {
	f := harnessPayloads(t)
	data, err := harnessMarshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var back harnessFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&back); err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(f.Cases, back.Cases, func(a, b harnessCase) bool {
		return a.ID == b.ID && a.Content == b.Content && a.Flags == b.Flags && a.ReplyTo == b.ReplyTo && len(a.Predicted) == len(b.Predicted)
	}) {
		t.Error("payloads.json does not round-trip")
	}
	for _, key := range []string{`"version": 1`, `"id"`, `"phase"`, `"content"`, `"flags"`, `"reply_to"`, `"expect_no_embeds"`, `"predicted"`, `"user_copy"`} {
		if !bytes.Contains(data, []byte(key)) {
			t.Errorf("payloads.json has no %s", key)
		}
	}
}
