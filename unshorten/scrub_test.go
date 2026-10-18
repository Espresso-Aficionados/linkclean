package unshorten_test

// The livenet recorder's scrubber. It turns what one live link's requests
// returned into fixture drafts that are safe to read, review and commit:
// every short token, sharer ID and content ID becomes a synthetic TOKENn,
// consistently across the request, the Location header, the body and
// meta.expect; a destination that is platform content moves to example.net
// with its path shape kept; body excerpts lose nonces, IDs, JWTs, form
// tokens and long opaque runs. Then refusals runs over each draft as it would
// be written, and anything still shaped like a cookie, an IP literal outside
// the documentation ranges, an Authorization header, an email, a string from
// the deny list or an original token refuses the whole link: nothing of it
// is written.
//
// This file has no build tag, so CI tests the scrubber; the suite that feeds
// it (livenet_test.go) never runs in CI.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// scrubRules names the rule set in every draft's header. Change it whenever
// a rule below changes.
const scrubRules = "rules v1"

// rewriteHost replaces the host of a destination that is platform content.
const rewriteHost = "example.net"

// minToken is the shortest path segment or fragment that becomes a token.
// Shorter segments are kept as written: they carry too little to identify
// anyone, and replacing them everywhere in a body would wreck it.
const minToken = 4

// structural path segments are kept as written: the fixed words of the
// registry patterns and of the destinations' path shapes. Every other
// segment of minToken bytes or more is a token.
var structural = map[string]bool{
	"album": true, "artist": true, "comments": true, "d": true, "dp": true, "episode": true, "error": true,
	"feed": true, "gp": true, "oembed": true, "p": true, "photo": true, "pin": true, "playlist": true,
	"posts": true, "preview": true, "product": true, "r": true, "redirect": true, "s": true,
	"share.google": true, "show": true, "status": true, "t": true, "track": true, "update": true,
	"url_shortener": true, "video": true, "watch": true,
}

// scrubber holds one link's token map, so every draft of the link (and the
// replayed input) uses the same TOKENn for the same original.
type scrubber struct {
	registry map[string]bool // registry hosts: kept, as are their path words
	keepDest bool            // the destination is the maintainer's own target: host and path kept
	tok      map[string]string
	n        int
	hosts    []string // destination hosts rewritten to example.net
}

func newScrubber(registry map[string]bool, keepDest bool) *scrubber {
	return &scrubber{registry: registry, keepDest: keepDest, tok: map[string]string{}}
}

// add maps every non-empty form in forms (one original, written in several
// ways) to one token.
func (s *scrubber) add(forms ...string) {
	t := ""
	for _, f := range forms {
		if v, ok := s.tok[f]; ok && f != "" {
			t = v
			break
		}
	}
	if t == "" {
		s.n++
		t = "TOKEN" + strconv.Itoa(s.n)
	}
	for _, f := range forms {
		if f != "" {
			s.tok[f] = t
		}
	}
}

// addURL registers the tokens of raw, an absolute URL as requested, served
// or returned. A URL on a registry host keeps its host and structural path
// words. Another URL is a destination: a homepage (empty or "/" path, no
// query) is kept whole; otherwise, unless keepDest, its host is rewritten
// and its path tokenized like a registry URL's. Query values that look like
// tokens are tokenized everywhere, and a query value that is itself a URL is
// scanned in turn. A raw value that does not parse as an absolute URL is one
// token.
func (s *scrubber) addURL(raw string) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" {
		if len(raw) >= minToken {
			s.add(raw)
		}
		return
	}
	host := strings.ToLower(u.Hostname())
	dest := !s.registry[host]
	if dest && (u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == "" {
		return
	}
	if dest && !s.keepDest && !slices.Contains(s.hosts, host) {
		s.hosts = append(s.hosts, host)
	}
	if !dest || !s.keepDest {
		for seg := range strings.SplitSeq(u.EscapedPath(), "/") {
			s.addSegment(seg)
		}
	}
	for kv := range strings.SplitSeq(u.RawQuery, "&") {
		_, v, _ := strings.Cut(kv, "=")
		s.addQueryValue(v)
	}
	if f := u.EscapedFragment(); len(f) >= minToken {
		s.add(f, u.Fragment)
	}
}

func (s *scrubber) addSegment(seg string) {
	seg = strings.TrimPrefix(seg, "@") // TikTok's /@user: the '@' is path shape
	if len(seg) < minToken || structural[strings.ToLower(seg)] {
		return
	}
	plain, err := url.PathUnescape(seg)
	if err != nil {
		plain = ""
	}
	s.add(seg, plain)
}

func (s *scrubber) addQueryValue(v string) {
	plain, err := url.QueryUnescape(v)
	if err != nil {
		plain = v
	}
	if l := strings.ToLower(plain); strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") {
		s.addURL(plain)
		return
	}
	if tokenLike(plain) {
		s.add(v, plain)
	}
}

// tokenLike reports whether a query value is opaque enough to be a token: six
// bytes or more and not just lowercase letters, '_' and '.'. So
// utm_source=ig_web_copy_link and _r=1 stay, si=… and share_id=… go.
func tokenLike(v string) bool {
	if len(v) < 6 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; !('a' <= c && c <= 'z' || c == '_' || c == '.') {
			return true
		}
	}
	return false
}

// originals returns every original the scrubber replaces, for the residual
// check.
func (s *scrubber) originals() []string {
	out := make([]string, 0, len(s.tok))
	for o := range s.tok {
		out = append(out, o)
	}
	slices.Sort(out)
	return out
}

// text replaces every original, in every escaped form a URL takes in a
// header, a query value, HTML or JSON, with its token, then rewrites the
// destination hosts.
func (s *scrubber) text(in string) string {
	type pair struct{ old, new string }
	var pairs []pair
	for o, t := range s.tok {
		for _, f := range []string{o, url.QueryEscape(o), url.PathEscape(o), html.EscapeString(o), strings.ReplaceAll(o, "/", `\/`)} {
			pairs = append(pairs, pair{f, t})
		}
	}
	// strings.Replacer prefers earlier pairs at one position: longest first.
	slices.SortFunc(pairs, func(a, b pair) int {
		if d := len(b.old) - len(a.old); d != 0 {
			return d
		}
		return strings.Compare(a.old, b.old)
	})
	var args []string
	for i, p := range pairs {
		if i > 0 && p.old == pairs[i-1].old {
			continue
		}
		args = append(args, p.old, p.new)
	}
	out := in
	if len(args) > 0 {
		out = strings.NewReplacer(args...).Replace(in)
	}
	for _, h := range s.hosts {
		re := regexp.MustCompile(`(?i)(//|\\/\\/)` + regexp.QuoteMeta(h) + `([/:?#"'\\<>\s&]|$)`)
		out = re.ReplaceAllString(out, "${1}"+rewriteHost+"${2}")
	}
	return out
}

// body scrubs an excerpt: text's replacements everywhere, then the pattern
// rules everywhere except inside the protected strings (the extracted URL
// in its scrubbed forms), which must survive so the replay still extracts
// it.
func (s *scrubber) body(b []byte, protect ...string) []byte {
	t := s.text(string(b))
	var spans [][2]int
	for _, p := range protect {
		for _, f := range []string{p, html.EscapeString(p), strings.ReplaceAll(p, "/", `\/`), strings.ReplaceAll(p, "&", `\u0026`)} {
			if f == "" {
				continue
			}
			for i := 0; ; {
				j := strings.Index(t[i:], f)
				if j < 0 {
					break
				}
				spans = append(spans, [2]int{i + j, i + j + len(f)})
				i += j + len(f)
			}
		}
	}
	slices.SortFunc(spans, func(a, b [2]int) int { return a[0] - b[0] })
	var out strings.Builder
	at := 0
	for _, sp := range spans {
		if sp[0] < at {
			if sp[1] > at {
				out.WriteString(t[at:sp[1]])
				at = sp[1]
			}
			continue
		}
		out.WriteString(applyRules(t[at:sp[0]]))
		out.WriteString(t[sp[0]:sp[1]])
		at = sp[1]
	}
	out.WriteString(applyRules(t[at:]))
	return []byte(out.String())
}

// formTokenNames are the names of anti-forgery and session form fields.
const formTokenNames = `lsd|jazoest|fb_dtsg(?:_ag)?|csrf[a-z_-]*|csrfmiddlewaretoken|xsrf[a-z_-]*|authenticity_token|_token|__requestverificationtoken`

var (
	ruleJWT      = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*`)
	ruleUUID     = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	ruleNonce    = regexp.MustCompile(`(?i)(\bnonce\s*=\s*)("[^"]*"|'[^']*')`)
	ruleDataID   = regexp.MustCompile(`(?i)(\bdata-(?:[a-z0-9-]+-)?id\s*=\s*)("[^"]*"|'[^']*')`)
	ruleURN      = regexp.MustCompile(`urn:li:([A-Za-z_]+):[^"'&;,\s<>()\\]+`)
	ruleURNTrack = regexp.MustCompile(`(?i)(;(?:trk|tracking|token)[a-z]*=)[^&"'\s<>;\\]+`)
	ruleInput    = regexp.MustCompile(`(?i)<input\b[^>]*>`)
	ruleInputTok = regexp.MustCompile(`(?i)\bname\s*=\s*["']?(?:` + formTokenNames + `)["'\s/>]`)
	ruleValue    = regexp.MustCompile(`(?i)(\bvalue\s*=\s*)("[^"]*"|'[^']*')`)
	ruleFormJS   = regexp.MustCompile(`(?i)(["']?\b(?:` + formTokenNames + `|token|dtsg|async_get_token)["']?\s*[:=,]\s*\[?\s*(?:\{\s*"token"\s*:\s*)?["'])[^"']*(["'])`)
	ruleFormQS   = regexp.MustCompile(`(?i)\b(` + formTokenNames + `)=[^&"'\s<>\\]+`)
	ruleLongRun  = regexp.MustCompile(`[A-Za-z0-9+/_-]{32,}={0,2}`)
)

// applyRules applies the body pattern rules (rules v1).
func applyRules(s string) string {
	s = ruleJWT.ReplaceAllString(s, "REDACTED-JWT")
	s = ruleUUID.ReplaceAllString(s, "00000000-0000-0000-0000-000000000000")
	s = ruleNonce.ReplaceAllString(s, `${1}"REDACTED"`)
	s = ruleDataID.ReplaceAllString(s, `${1}"REDACTED"`)
	s = ruleURN.ReplaceAllString(s, "urn:li:${1}:REDACTED")
	s = ruleURNTrack.ReplaceAllString(s, "${1}REDACTED")
	s = ruleInput.ReplaceAllStringFunc(s, func(tag string) string {
		if !ruleInputTok.MatchString(tag) {
			return tag
		}
		return ruleValue.ReplaceAllString(tag, `${1}"REDACTED"`)
	})
	s = ruleFormJS.ReplaceAllString(s, "${1}REDACTED${2}")
	s = ruleFormQS.ReplaceAllString(s, "${1}=REDACTED")
	return ruleLongRun.ReplaceAllStringFunc(s, func(m string) string {
		if strings.ContainsAny(m, "0123456789") && strings.ContainsFunc(m, func(r rune) bool { return 'A' <= r && r <= 'Z' || 'a' <= r && r <= 'z' }) {
			return "REDACTED"
		}
		return m
	})
}

var (
	refCookie   = regexp.MustCompile(`(?i)[A-Za-z0-9_.!#$%&'*+^|~-]+=[^;\s]*;\s*(?:path|domain|expires|max-age|secure|httponly|samesite)\b`)
	refSetCook  = regexp.MustCompile(`(?i)\bset-cookie\s*:`)
	refAuth     = regexp.MustCompile(`(?i)authorization|\bbearer\s+[A-Za-z0-9._~+/-]{8,}`)
	refEmail    = regexp.MustCompile(`(?i)[a-z0-9._%+-]+(?:@|%40)[a-z0-9-]+(?:\.[a-z0-9-]+)*\.([a-z]{2,})\b`)
	refWordRun  = regexp.MustCompile(`[0-9A-Za-z_:.%]+`)
	refV4Shape  = regexp.MustCompile(`^[0-9]{1,3}(?:\.[0-9]{1,3}){3}$`)
	refPlaceTok = regexp.MustCompile(`TOKEN[0-9]+|REDACTED|` + regexp.QuoteMeta(rewriteHost))
)

// imageExt are "TLDs" of image names such as logo@2x.png, which are not
// emails.
var imageExt = map[string]bool{"png": true, "jpg": true, "jpeg": true, "gif": true, "svg": true, "webp": true, "avif": true, "ico": true}

// docRanges are the documentation address ranges (RFC 5737, RFC 3849): the
// only IP literals a draft may hold.
var docRanges = []netip.Prefix{
	netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("2001:db8::/32"),
}

// refusals returns why text must not be written, as "<rule> at byte <n>"
// entries that never quote the text: a cookie, a Set-Cookie header, an
// Authorization header or bearer token, an email, an IP literal outside the
// documentation ranges, a deny-list string (in any case and in its escaped
// forms), or a residual original (an original the scrubber replaced, still
// present in some form it did not anticipate).
func refusals(text string, deny, residual []string) []string {
	var out []string
	at := func(rule string, i int) { out = append(out, rule+" at byte "+strconv.Itoa(i)) }
	for _, r := range []struct {
		name string
		re   *regexp.Regexp
	}{{"cookie", refCookie}, {"Set-Cookie header", refSetCook}, {"Authorization", refAuth}} {
		for _, m := range r.re.FindAllStringIndex(text, -1) {
			at(r.name, m[0])
		}
	}
	for _, m := range refEmail.FindAllStringSubmatchIndex(text, -1) {
		if !imageExt[strings.ToLower(text[m[2]:m[3]])] {
			at("email", m[0])
		}
	}
	for _, i := range ipLiterals(text) {
		at("IP literal", i)
	}
	// The deny list and the residual originals are also looked for in the
	// percent-decoded and the HTML-unescaped text, where an offset is one of
	// that view.
	views := []struct{ name, s string }{{"", text}}
	for _, v := range []struct{ name, s string }{{" (percent-decoded)", pctDecode(text)}, {" (HTML-unescaped)", html.UnescapeString(text)}} {
		if v.s != text {
			views = append(views, v)
		}
	}
	for _, d := range deny {
	view:
		for _, v := range views {
			lower := strings.ToLower(v.s)
			for _, f := range escapedForms(d) {
				if i := strings.Index(lower, strings.ToLower(f)); i >= 0 {
					at("deny-list entry"+v.name, i)
					break view
				}
			}
		}
	}
	// Blank out what the scrubber inserted, so an original that happens to
	// be part of a placeholder is not reported.
	mask := func(s string) string {
		return refPlaceTok.ReplaceAllStringFunc(s, func(m string) string { return strings.Repeat(" ", len(m)) })
	}
	for _, o := range residual {
	rview:
		for _, v := range views {
			masked := mask(v.s)
			for _, f := range escapedForms(o) {
				if i := strings.Index(masked, f); i >= 0 {
					at("residual token"+v.name, i)
					break rview
				}
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// pctDecode decodes every valid %XX in s and leaves the rest as written.
func pctDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	unhex := func(c byte) (byte, bool) {
		switch {
		case '0' <= c && c <= '9':
			return c - '0', true
		case 'a' <= c && c <= 'f':
			return c - 'a' + 10, true
		case 'A' <= c && c <= 'F':
			return c - 'A' + 10, true
		}
		return 0, false
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi, ok1 := unhex(s[i+1])
			lo, ok2 := unhex(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func escapedForms(s string) []string {
	return slices.Compact([]string{s, url.QueryEscape(s), url.PathEscape(s), html.EscapeString(s), strings.ReplaceAll(s, "/", `\/`)})
}

// ipLiterals returns the offsets of the IP literals in text outside
// docRanges. "::" alone (C++ scope, CSS) is not an address worth refusing.
func ipLiterals(text string) []int {
	var out []int
	bad := func(a netip.Addr) bool {
		a = a.Unmap().WithZone("")
		for _, p := range docRanges {
			if p.Contains(a) {
				return false
			}
		}
		return true
	}
	for _, m := range refWordRun.FindAllStringIndex(text, -1) {
		w := strings.Trim(text[m[0]:m[1]], ".")
		found := false
		if strings.Count(w, ":") >= 2 && w != "::" {
			if a, err := netip.ParseAddr(w); err == nil && bad(a) {
				found = true
			}
		}
		for p := range strings.SplitSeq(w, ":") {
			if p = strings.Trim(p, "."); refV4Shape.MatchString(p) {
				if a, err := netip.ParseAddr(p); err == nil && bad(a) {
					found = true
				}
			}
		}
		if found {
			out = append(out, m[0])
		}
	}
	return out
}

// draft is one recorded request as a fixture draft:
// testdata/responses/<Host>/<Case>.txtar once reviewed.
type draft struct {
	Host, Case  string
	Comment     string // header lines, without "# "
	Meta        draftMeta
	Status      int
	Location    string
	ContentType string
	Body        []byte
}

type draftMeta struct {
	Entry     string      `json:"entry"`
	Strategy  string      `json:"strategy"`
	Request   string      `json:"request"`
	Expect    draftExpect `json:"expect"`
	SetCookie bool        `json:"set_cookie"`
	Body      string      `json:"body"` // "none" or "excerpt"
	BodyLen   int         `json:"body_len"`
}

type draftExpect struct {
	Next    string `json:"next"`
	Failure string `json:"failure"`
	Status  int    `json:"status"`
}

func (d *draft) path() string { return d.Host + "/" + d.Case + ".txtar" }

func isMarker(line string) bool {
	return strings.HasPrefix(line, "-- ") && strings.HasSuffix(line, " --") && len(line) > 6
}

// marshal writes d in the fixture format. A body that does not end in a
// newline gets one, as txtar requires; nothing extracts across it.
func (d *draft) marshal() ([]byte, error) {
	var b bytes.Buffer
	for l := range strings.SplitSeq(d.Comment, "\n") {
		b.WriteString("# " + l + "\n")
	}
	var meta bytes.Buffer
	enc := json.NewEncoder(&meta)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d.Meta); err != nil {
		return nil, err
	}
	b.WriteString("-- meta.json --\n")
	b.Write(meta.Bytes())
	b.WriteString("-- response --\n")
	fmt.Fprintf(&b, "HTTP/1.1 %d\n", d.Status)
	for _, h := range [][2]string{{"Location", d.Location}, {"Content-Type", d.ContentType}} {
		if strings.ContainsAny(h[1], "\r\n") {
			return nil, fmt.Errorf("%s: %s holds a line break", d.path(), h[0])
		}
		if h[1] != "" {
			b.WriteString(h[0] + ": " + h[1] + "\n")
		}
	}
	b.WriteString("-- body --\n")
	for l := range strings.SplitSeq(string(d.Body), "\n") {
		if isMarker(strings.TrimRight(l, "\r")) {
			return nil, fmt.Errorf("%s: the body holds a txtar section marker", d.path())
		}
	}
	b.Write(d.Body)
	if len(d.Body) > 0 && d.Body[len(d.Body)-1] != '\n' {
		b.WriteByte('\n')
	}
	return b.Bytes(), nil
}

// parseDraft reads what marshal wrote (Host and Case are not in the file).
func parseDraft(data []byte) (*draft, error) {
	d := &draft{}
	var comment []string
	var names []string
	sections := map[string][]byte{}
	cur := ""
	for l := range strings.SplitAfterSeq(string(data), "\n") {
		s := strings.TrimRight(l, "\r\n")
		switch {
		case isMarker(s):
			cur = strings.TrimSpace(s[3 : len(s)-3])
			names = append(names, cur)
		case cur == "":
			if s != "" {
				comment = append(comment, strings.TrimPrefix(strings.TrimPrefix(s, "#"), " "))
			}
		default:
			sections[cur] = append(sections[cur], l...)
		}
	}
	if !slices.Equal(names, []string{"meta.json", "response", "body"}) {
		return nil, fmt.Errorf("draft sections are %q, want meta.json, response, body", names)
	}
	d.Comment = strings.Join(comment, "\n")
	dec := json.NewDecoder(bytes.NewReader(sections["meta.json"]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d.Meta); err != nil {
		return nil, fmt.Errorf("draft meta.json: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(sections["response"]), "\n"), "\n")
	code, ok := strings.CutPrefix(lines[0], "HTTP/1.1 ")
	st, err := strconv.Atoi(code)
	if !ok || err != nil || st < 100 || st > 999 {
		return nil, errors.New("draft response: bad status line")
	}
	d.Status = st
	for _, l := range lines[1:] {
		k, v, ok := strings.Cut(l, ": ")
		switch {
		case ok && k == "Location" && d.Location == "":
			d.Location = v
		case ok && k == "Content-Type" && d.ContentType == "":
			d.ContentType = v
		default:
			return nil, errors.New("draft response: only one Location and one Content-Type header are allowed")
		}
	}
	d.Body = sections["body"]
	return d, nil
}

// excerpt cuts the part of body a fixture keeps: for JSON the whole body
// (the strategy parses all of it); else the extracted element (elem, in any
// escaped form) with 512 bytes either side, widened to the enclosing tags;
// else, when elem is absent, the first 1 KiB. Cuts fall on UTF-8 boundaries.
func excerpt(body []byte, contentType, elem string) []byte {
	if strings.Contains(strings.ToLower(contentType), "json") {
		return body
	}
	start, end := 0, min(len(body), 1024)
	if elem != "" {
		for _, f := range []string{elem, html.EscapeString(elem), strings.ReplaceAll(elem, "/", `\/`)} {
			if i := bytes.Index(body, []byte(f)); i >= 0 {
				start, end = max(0, i-512), min(len(body), i+len(f)+512)
				if j := bytes.LastIndexByte(body[max(0, start-2048):start], '<'); j >= 0 && start > 0 {
					start = max(0, start-2048) + j
				}
				break
			}
		}
	}
	if j := bytes.IndexByte(body[end:min(len(body), end+2048)], '>'); j >= 0 {
		end += j + 1
	}
	for start < end && !utf8.RuneStart(body[start]) {
		start++
	}
	for end < len(body) && end > start && !utf8.RuneStart(body[end]) {
		end--
	}
	return body[start:end]
}

// liveExchange is one request the recorder saw, joined with its RequestInfo.
type liveExchange struct {
	Entry, Strategy, Failure string // from the RequestInfo
	Hop                      int
	Request                  string // the URL as sent
	Proto                    string
	Status                   int // 0: no response
	Location, ContentType    string
	SetCookie                bool
	Body                     []byte // what the Fetcher read
	BodyRead                 bool
	BodyLen                  int // bytes read, or the Content-Length when larger
}

// liveOutcome is one hop's outcome as the Resolver stored it.
type liveOutcome struct {
	Next, Strategy string
	Status         int
	Failure        string
}

// liveCapture is everything one link's resolution sent and received.
type liveCapture struct {
	Case, Date, UA, Input string
	Exchanges             []liveExchange
	Hops                  []liveOutcome // per hop index; a hop past the end was transient (never stored)
}

type draftFile struct {
	Path string // <host>/<case>.txtar
	Data []byte
}

// scrubbedLink is a link's drafts, ready to write once the replay agrees.
type scrubbedLink struct {
	Input string        // the link URL, scrubbed: what the replay resolves
	Files []draftFile   // in request order
	Hops  []liveOutcome // the hop outcomes, scrubbed: what the replay must store
}

var uaProduct = regexp.MustCompile(`^linkclean/[A-Za-z0-9.+-]+`)

// scrubCapture turns c into drafts, one per request. The last request of a
// hop carries the hop's outcome in meta.expect (a hop with no stored outcome
// was transient); an earlier one, the first attempt of a hop with a
// fallback, carries its own request's outcome and says so in its header.
// Status follows the hop rule: 0 when the request read the body. Any refusal
// in any draft refuses the whole link.
func scrubCapture(c liveCapture, registry map[string]bool, keepDest bool, deny []string) (scrubbedLink, error) {
	s := newScrubber(registry, keepDest)
	s.addURL(c.Input)
	for _, x := range c.Exchanges {
		if x.Status == 0 {
			return scrubbedLink{}, errors.New("a request had no response; a transport failure cannot be replayed")
		}
		s.addURL(x.Request)
		if x.Location != "" {
			s.addURL(x.Location)
			if base, err := url.Parse(x.Request); err == nil {
				if l, err := base.Parse(x.Location); err == nil {
					s.addURL(l.String())
				}
			}
		}
	}
	for _, h := range c.Hops {
		if h.Next != "" {
			s.addURL(h.Next)
		}
	}
	ua := uaProduct.FindString(c.UA)
	if ua == "" {
		ua = "linkclean/unknown"
	}
	out := scrubbedLink{Input: s.text(c.Input)}
	for _, h := range c.Hops {
		h.Next = s.text(h.Next)
		out.Hops = append(out.Hops, h)
	}
	used := map[string]int{}
	var drafts []*draft
	for i, x := range c.Exchanges {
		u, err := url.Parse(x.Request)
		if err != nil || u.Host == "" {
			return scrubbedLink{}, fmt.Errorf("request %d does not parse", i+1)
		}
		host := strings.ToLower(u.Hostname())
		if !registry[host] {
			return scrubbedLink{}, fmt.Errorf("request %d went to a host outside the registry", i+1)
		}
		last := i == len(c.Exchanges)-1 || c.Exchanges[i+1].Hop != x.Hop
		nth, of := 1, 0
		for _, y := range c.Exchanges {
			if y.Hop == x.Hop {
				of++
			}
		}
		for _, y := range c.Exchanges[:i] {
			if y.Hop == x.Hop {
				nth++
			}
		}
		status := x.Status
		if x.BodyRead {
			status = 0
		}
		exp := draftExpect{Failure: x.Failure, Status: status}
		note := fmt.Sprintf("request %d of %d of hop %d; meta.expect is this request's own outcome, not the hop's", nth, of, x.Hop)
		rawNext := ""
		switch {
		case last && x.Hop < len(c.Hops):
			h := c.Hops[x.Hop]
			rawNext = h.Next
			exp = draftExpect{Next: s.text(h.Next), Failure: h.Failure, Status: h.Status}
			note = fmt.Sprintf("request %d of %d of hop %d; meta.expect is the hop's outcome", nth, of, x.Hop)
		case last:
			exp.Failure = "transient"
			note = fmt.Sprintf("request %d of %d of hop %d, which was transient (never cached)", nth, of, x.Hop)
		}
		used[host]++
		name := c.Case
		if used[host] > 1 {
			name += "-" + strconv.Itoa(used[host])
		}
		d := &draft{
			Host: host, Case: name,
			Comment: fmt.Sprintf("%s %s. recorded %s livenet %s; scrubbed (%s)\ndraft over %s: %s. Review by hand before committing it under testdata/responses/.",
				host, name, c.Date, ua, scrubRules, x.Proto, note),
			Meta: draftMeta{
				Entry: x.Entry, Strategy: x.Strategy, Request: s.text(x.Request), Expect: exp,
				SetCookie: x.SetCookie, Body: "none", BodyLen: x.BodyLen,
			},
			Status: x.Status, Location: s.text(x.Location), ContentType: x.ContentType,
		}
		if x.BodyRead && len(x.Body) > 0 {
			d.Meta.Body = "excerpt"
			d.Body = s.body(excerpt(x.Body, x.ContentType, rawNext), exp.Next)
		}
		drafts = append(drafts, d)
	}
	var problems []string
	residual := s.originals()
	for _, d := range drafts {
		b, err := d.marshal()
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		for _, r := range refusals(string(b), deny, residual) {
			problems = append(problems, d.path()+": "+r)
		}
		out.Files = append(out.Files, draftFile{Path: d.path(), Data: b})
	}
	if len(problems) > 0 {
		return scrubbedLink{}, errors.New("refused: " + strings.Join(problems, "; "))
	}
	return out, nil
}

// --- tests ---

var testRegistryHosts = map[string]bool{
	"t.co": true, "pin.it": true, "api.pinterest.com": true, "lnkd.in": true, "vm.tiktok.com": true,
	"www.tiktok.com": true, "www.reddit.com": true, "share.google": true, "www.google.com": true,
}

func TestScrubberURLs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		keepDest bool
		urls     []string
		in, want string
	}{
		{"short token", false, []string{"https://t.co/Ab3dEf9"}, "GET https://t.co/Ab3dEf9 x", "GET https://t.co/TOKEN1 x"},
		{"short segment kept", false, []string{"https://t.co/abc"}, "https://t.co/abc", "https://t.co/abc"},
		{
			"platform destination", false, []string{"https://open.spotify.com/track/4uLU6hMCjMI75M1A2tKUQC?si=a1b2c3d4e5&utm_source=copy_link"},
			"https://open.spotify.com/track/4uLU6hMCjMI75M1A2tKUQC?si=a1b2c3d4e5&utm_source=copy_link",
			"https://example.net/track/TOKEN1?si=TOKEN2&utm_source=copy_link",
		},
		{
			"own destination kept", true, []string{"https://github.com/Espresso-Aficionados/linkclean?utm_source=x&t=Zq7x9Kp2"},
			"https://github.com/Espresso-Aficionados/linkclean?utm_source=x&t=Zq7x9Kp2",
			"https://github.com/Espresso-Aficionados/linkclean?utm_source=x&t=TOKEN1",
		},
		{"homepage kept", false, []string{"https://www.amazon.com/"}, "https://www.amazon.com/", "https://www.amazon.com/"},
		{
			"tiktok user and video", false, []string{"https://www.tiktok.com/@someuser/video/7300000000000000001"},
			`{"cite":"https:\/\/www.tiktok.com\/@someuser\/video\/7300000000000000001","embed_product_id":"7300000000000000001","author_unique_id":"someuser"}`,
			`{"cite":"https:\/\/www.tiktok.com\/@TOKEN1\/video\/TOKEN2","embed_product_id":"TOKEN2","author_unique_id":"TOKEN1"}`,
		},
		{
			"query URL scanned", false, []string{"https://www.tiktok.com/oembed?url=https%3A%2F%2Fvm.tiktok.com%2FZMabcdEF%2F"},
			"https://www.tiktok.com/oembed?url=https%3A%2F%2Fvm.tiktok.com%2FZMabcdEF%2F",
			"https://www.tiktok.com/oembed?url=https%3A%2F%2Fvm.tiktok.com%2FTOKEN1%2F",
		},
		{
			"intermediate hop", false, []string{"https://api.pinterest.com/url_shortener/2bN9xQ/redirect/"},
			"https://api.pinterest.com/url_shortener/2bN9xQ/redirect/", "https://api.pinterest.com/url_shortener/TOKEN1/redirect/",
		},
		{
			"reddit sub and token", false, []string{"https://www.reddit.com/r/espresso/s/Xy12AbCdEf"},
			"https://www.reddit.com/r/espresso/s/Xy12AbCdEf", "https://www.reddit.com/r/TOKEN1/s/TOKEN2",
		},
		{
			"html-escaped and longest first", false, []string{"https://t.co/Ab3dEf9", "https://lnkd.in/Ab3dEf9xyz"},
			`<a href="https://t.co/Ab3dEf9xyz?a=1&amp;b=2">Ab3dEf9</a>`, `<a href="https://t.co/TOKEN2?a=1&amp;b=2">TOKEN1</a>`,
		},
		{
			"host boundary", false, []string{"https://open.spotify.com/track/4uLU6hMCjMI75M1A2tKUQC"},
			"//open.spotify.com/x //open.spotify.com.au/x", "//example.net/x //open.spotify.com.au/x",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newScrubber(testRegistryHosts, tc.keepDest)
			for _, u := range tc.urls {
				s.addURL(u)
			}
			if got := s.text(tc.in); got != tc.want {
				t.Errorf("text\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestApplyRules(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`<script nonce="aB3+/x==">`, `<script nonce="REDACTED">`},
		{`<div data-entity-id='12345' data-id="9">`, `<div data-entity-id="REDACTED" data-id="REDACTED">`},
		{`id 123e4567-e89b-12d3-a456-426614174000.`, `id 00000000-0000-0000-0000-000000000000.`},
		{`"urn:li:activity:7100000000000000000"`, `"urn:li:activity:REDACTED"`},
		{`x;trackingId=abc123==&y`, `x;trackingId=REDACTED&y`},
		{`t=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U&`, `t=REDACTED-JWT&`},
		{`<input type="hidden" name="lsd" value="AVqZx1">`, `<input type="hidden" name="lsd" value="REDACTED">`},
		{`<input type="text" name="q" value="espresso">`, `<input type="text" name="q" value="espresso">`},
		{`["LSD",[],{"token":"AVq9zz"},323]`, `["LSD",[],{"token":"REDACTED"},323]`},
		{`"fb_dtsg":"NAcN9x:1"`, `"fb_dtsg":"REDACTED"`},
		{`a?jazoest=2981&b`, `a?jazoest=REDACTED&b`},
		{`k 0123456789abcdef0123456789abcdef01 k`, `k REDACTED k`},
		{`aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`, `aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`},
	} {
		if got := applyRules(tc.in); got != tc.want {
			t.Errorf("applyRules(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

func TestScrubberBodyProtects(t *testing.T) {
	s := newScrubber(testRegistryHosts, true)
	next := "https://github.com/Espresso-Aficionados/linkclean/blob/0123456789abcdef0123456789abcdef01234567/README.md"
	s.addURL(next)
	body := `<a data-tracking-control-name="external_url_click" href="` + html.EscapeString(next) + `">` +
		` 0123456789abcdef0123456789abcdef01234567`
	got := string(s.body([]byte(body), next))
	if !strings.Contains(got, html.EscapeString(next)) {
		t.Errorf("protected URL not kept: %s", got)
	}
	if !strings.HasSuffix(got, " REDACTED") {
		t.Errorf("unprotected run kept: %s", got)
	}
}

func TestRefusals(t *testing.T) {
	deny := []string{"maintainer-name", "aff-20x"}
	for _, tc := range []struct {
		in   string
		want string // first refusal's rule; "" for none
	}{
		{"https://t.co/TOKEN1", ""},
		{"sid=abc; Path=/; HttpOnly", "cookie"},
		{"Set-Cookie: x", "Set-Cookie header"},
		{"authorization: x", "Authorization"},
		{"Bearer abcdefgh1234", "Authorization"},
		{"mail me@example.org now", "email"},
		{"a%40b.example.com", "email"},
		{`<img src="logo@2x.png">`, ""},
		{"from 10.0.0.1 ok", "IP literal"},
		{"at 8.8.8.8:443", "IP literal"},
		{"doc 192.0.2.7 and 2001:db8::1", ""},
		{"v6 2606:4700::1111", "IP literal"},
		{"mapped ::ffff:127.0.0.1", "IP literal"},
		{"a::before std::string ::", ""},
		{"jquery-1.12.4.min.js go1.26.0", ""},
		{"by Maintainer-Name!", "deny-list entry"},
		{"tag=aff%2D20x", "deny-list entry (percent-decoded)"},
		{"q=maintainer&#45;name", "deny-list entry (HTML-unescaped)"},
		{"loopback ::1", "IP literal"},
		{"left Ab3dEf9%41", "residual token"},
		{"left Ab3d&#69;f9", "residual token (HTML-unescaped)"},
		{"left Ab3dEf9 behind", "residual token"},
		{"TOKEN12 example.net", ""},
	} {
		got := refusals(tc.in, deny, []string{"Ab3dEf9", "example", "TOKEN1"})
		first := ""
		if len(got) > 0 {
			first, _, _ = strings.Cut(got[0], " at byte ")
		}
		if first != tc.want {
			t.Errorf("refusals(%q) = %q, want first rule %q", tc.in, got, tc.want)
		}
		for _, r := range got {
			if strings.Contains(r, tc.in) && tc.in != "" {
				t.Errorf("refusal %q quotes the text", r)
			}
		}
	}
}

func TestExcerpt(t *testing.T) {
	pad := strings.Repeat("x", 3000)
	next := "https://example.net/p"
	body := []byte("<p>" + pad[:600] + `<a class="c" href="` + next + `">` + strings.Repeat("é", 1000) + "</a>" + pad)
	ex := string(excerpt(body, "text/html", next))
	if !strings.HasPrefix(ex, "<p>") || !strings.Contains(ex, `<a class="c" href="`+next) || !utf8.ValidString(ex) {
		t.Errorf("excerpt does not start at a tag, lost the element or cut a rune: %.60q…", ex)
	}
	if len(ex) > 512+len(next)+512+2048+64 {
		t.Errorf("excerpt is %d bytes", len(ex))
	}
	if got := excerpt([]byte(`{"a":1}`), "application/json", ""); string(got) != `{"a":1}` {
		t.Errorf("JSON excerpt = %q", got)
	}
	if got := excerpt(body, "text/html", "absent"); len(got) < 1024 || len(got) > 1024+2048 {
		t.Errorf("head excerpt is %d bytes", len(got))
	}
}

func TestDraftRoundTrip(t *testing.T) {
	d := &draft{
		Host: "t.co", Case: "good", Comment: "t.co good. recorded 2026-10-18 livenet linkclean/devel; scrubbed (rules v1)\nsecond line",
		Meta: draftMeta{
			Entry: "t.co", Strategy: "Redirect", Request: "https://t.co/TOKEN1",
			Expect:    draftExpect{Next: "https://example.net/p?a=1&b=2", Failure: "none", Status: 301},
			SetCookie: true, Body: "excerpt", BodyLen: 51234,
		},
		Status: 301, Location: "https://example.net/p?a=1&b=2", ContentType: "text/html; charset=utf-8",
		Body: []byte("<html>\n-- not a marker\n"),
	}
	b, err := d.marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"next":"https://example.net/p?a=1&b=2"`)) {
		t.Errorf("meta.json escapes HTML characters:\n%s", b)
	}
	got, err := parseDraft(b)
	if err != nil {
		t.Fatal(err)
	}
	got.Host, got.Case = d.Host, d.Case
	if gb, _ := got.marshal(); !bytes.Equal(gb, b) {
		t.Errorf("round trip differs:\n%s\n---\n%s", gb, b)
	}
	d.Body = []byte("a\n-- meta.json --\n")
	if _, err := d.marshal(); err == nil {
		t.Error("a body holding a section marker was written")
	}
	d.Body, d.Location = nil, "x\r\nSet-Cookie: y"
	if _, err := d.marshal(); err == nil {
		t.Error("a header holding a line break was written")
	}
}

func TestScrubCapture(t *testing.T) {
	const next = "https://www.linkedin.com/posts/someone_activity-7100000000000000000-AbCd"
	body := []byte(`<!doctype html><meta name="x" content="y"><script nonce="n0nce99">var t="0123456789abcdef0123456789abcdef";</script>` +
		`<a data-tracking-control-name="external_url_click" data-tracking-will-navigate href="` + html.EscapeString(next) + `">go</a>`)
	c := liveCapture{
		Case: "good", Date: "2026-10-18", UA: "linkclean/devel (+https://github.com/Espresso-Aficionados/linkclean)",
		Input: "https://lnkd.in/gAb3dEf?utm_source=x",
		Exchanges: []liveExchange{{
			Entry: "lnkd.in", Strategy: "BodyRegexp", Failure: "none", Request: "https://lnkd.in/gAb3dEf", Proto: "HTTP/2.0",
			Status: 200, ContentType: "text/html", SetCookie: true, Body: body, BodyRead: true, BodyLen: 40000,
		}},
		Hops: []liveOutcome{{Next: next, Strategy: "BodyRegexp", Status: 0, Failure: "none"}},
	}
	sl, err := scrubCapture(c, map[string]bool{"lnkd.in": true}, false, []string{"someone"})
	if err != nil {
		t.Fatal(err)
	}
	if sl.Input != "https://lnkd.in/TOKEN1?utm_source=x" {
		t.Errorf("input = %s", sl.Input)
	}
	wantNext := "https://example.net/posts/TOKEN2"
	if len(sl.Hops) != 1 || sl.Hops[0].Next != wantNext {
		t.Fatalf("hops = %+v, want next %s", sl.Hops, wantNext)
	}
	if len(sl.Files) != 1 || sl.Files[0].Path != "lnkd.in/good.txtar" {
		t.Fatalf("files = %v", sl.Files)
	}
	d, err := parseDraft(sl.Files[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	if d.Meta.Expect != (draftExpect{Next: wantNext, Failure: "none", Status: 0}) || d.Meta.Request != "https://lnkd.in/TOKEN1" || d.Meta.Body != "excerpt" {
		t.Errorf("meta = %+v", d.Meta)
	}
	for _, s := range []string{"gAb3dEf", "someone", "7100000000000000000", "n0nce99", "0123456789abcdef"} {
		if bytes.Contains(sl.Files[0].Data, []byte(s)) {
			t.Errorf("draft still holds %q:\n%s", s, sl.Files[0].Data)
		}
	}
	if !bytes.Contains(d.Body, []byte(`href="`+html.EscapeString(wantNext)+`"`)) {
		t.Errorf("the extracted element is not the scrubbed next:\n%s", d.Body)
	}
	if !strings.HasPrefix(d.Comment, "lnkd.in good. recorded 2026-10-18 livenet linkclean/devel; scrubbed (rules v1)\n") {
		t.Errorf("comment = %q", d.Comment)
	}

	// A refusal writes nothing for the link.
	c.Exchanges[0].Body = append(slices.Clip(body), " contact 10.1.2.3"...)
	if sl, err := scrubCapture(c, map[string]bool{"lnkd.in": true}, false, nil); err == nil || len(sl.Files) != 0 ||
		!strings.Contains(err.Error(), "lnkd.in/good.txtar: IP literal at byte") || strings.Contains(err.Error(), "10.1.2.3") {
		t.Errorf("scrubCapture = %v, %v; want an IP refusal naming no address", sl.Files, err)
	}
	c.Exchanges[0].Status = 0
	if _, err := scrubCapture(c, map[string]bool{"lnkd.in": true}, false, nil); err == nil {
		t.Error("a request without a response was drafted")
	}
}

func TestScrubCaptureTwoRequests(t *testing.T) {
	c := liveCapture{
		Case: "good", Date: "2026-10-18", UA: "linkclean/v0.3.0 (+x)", Input: "https://vm.tiktok.com/ZMab12Cd/",
		Exchanges: []liveExchange{
			{
				Entry: "tiktok", Strategy: "TikTokOEmbed", Failure: "none", Hop: 0, Proto: "HTTP/2.0",
				Request: "https://www.tiktok.com/oembed?url=" + url.QueryEscape("https://vm.tiktok.com/ZMab12Cd/"),
				Status:  400, ContentType: "application/json", Body: []byte(`{"code":400}`), BodyRead: true, BodyLen: 12,
			},
			{
				Entry: "tiktok", Strategy: "Redirect", Failure: "none", Hop: 0, Proto: "HTTP/2.0", Request: "https://vm.tiktok.com/ZMab12Cd/",
				Status: 301, Location: "https://www.tiktok.com/@someuser/video/7300000000000000001?_r=1&_t=ZT-8xYz123",
			},
		},
		Hops: []liveOutcome{{Next: "https://www.tiktok.com/@someuser/video/7300000000000000001?_r=1&_t=ZT-8xYz123", Strategy: "Redirect", Status: 301, Failure: "none"}},
	}
	sl, err := scrubCapture(c, testRegistryHosts, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range sl.Files {
		paths = append(paths, f.Path)
	}
	if !slices.Equal(paths, []string{"www.tiktok.com/good.txtar", "vm.tiktok.com/good.txtar"}) {
		t.Fatalf("paths = %q", paths)
	}
	first, _ := parseDraft(sl.Files[0].Data)
	second, _ := parseDraft(sl.Files[1].Data)
	if first.Meta.Request != "https://www.tiktok.com/oembed?url=https%3A%2F%2Fvm.tiktok.com%2FTOKEN1%2F" ||
		first.Meta.Expect != (draftExpect{Failure: "none", Status: 0}) || !strings.Contains(first.Comment, "this request's own outcome") {
		t.Errorf("first draft: %+v %q", first.Meta, first.Comment)
	}
	want := "https://www.tiktok.com/@TOKEN2/video/TOKEN3?_r=1&_t=TOKEN4"
	if second.Location != want || second.Meta.Expect != (draftExpect{Next: want, Failure: "none", Status: 301}) {
		t.Errorf("second draft: %q %+v", second.Location, second.Meta.Expect)
	}

	// A hop that stored nothing was transient.
	c.Hops = nil
	sl, err = scrubCapture(c, testRegistryHosts, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := parseDraft(sl.Files[1].Data); d.Meta.Expect.Failure != "transient" {
		t.Errorf("transient hop's expect = %+v", d.Meta.Expect)
	}
}
