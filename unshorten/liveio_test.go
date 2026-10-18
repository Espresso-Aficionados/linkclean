package unshorten_test

// The livenet suite's inputs and outputs: the links file (schema
// testdata/livenet/links.schema.json), the deny list, the checks on the
// files and directories it touches, the expectation check, the gating table
// and the results file (template testdata/livenet/results.md.tmpl). All of
// it is pure, so it has no build tag and CI tests it, the committed example
// links file and the template included. The suite that drives it is
// livenet_test.go, which never runs in CI.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"text/template"

	"github.com/Espresso-Aficionados/linkclean/unshorten"
)

// liveEgress is the results file's egress field. Record and check mode both
// run from the maintainer's workstation, whose egress is the bot's: the
// server runs from the same house.
const liveEgress = "workstation"

// Paths of the committed livenet files, relative to the package directory.
const (
	liveExamplePath  = "testdata/livenet/links.example.json"
	liveSchemaPath   = "testdata/livenet/links.schema.json"
	liveTemplatePath = "testdata/livenet/results.md.tmpl"
)

// linksFile is the -livenet.links file. A field without omitempty is
// required (TestLinksSchema holds the schema to that).
type linksFile struct {
	Comment string     `json:"comment,omitempty"`
	Version int        `json:"version"`
	Links   []liveLink `json:"links"`
}

type liveLink struct {
	Case   string `json:"case"`   // the draft's name: <host>/<case>.txtar
	URL    string `json:"url"`    // the link, as a user would post it
	Kind   string `json:"kind"`   // "good" (resolves) or "invalid" (an unknown code)
	Origin string `json:"origin"` // "created" (the maintainer's), "public" (someone else's: check only) or "synthetic" (an invented code)
	// Generic is the GenericPolicy: "PreviewOnly" (the default), "Follow" or
	// "Skip".
	Generic string `json:"generic,omitempty"`
	// KeepDestination keeps a destination's host and path in the drafts:
	// set it only when the destination is the maintainer's own target.
	KeepDestination bool       `json:"keep_destination,omitempty"`
	Note            string     `json:"note,omitempty"` // what the case confirms; never written to the results
	Expect          liveExpect `json:"expect"`
}

type liveExpect struct {
	Entry    *string   `json:"entry"`    // Match's entry; "" when the link must not match (and makes no request)
	Failure  *string   `json:"failure"`  // the outcome: a FailureKind name but "transient", or "not_short_link"
	Strategy *string   `json:"strategy"` // the strategy of the last request; "" when none is made
	Status   *int      `json:"status"`   // the HTTP status of the last request; 0 when none or no response
	Host     string    `json:"host,omitempty"`
	Path     string    `json:"path,omitempty"` // a regexp the final URL's escaped path must match
	Hops     []liveHop `json:"hops,omitempty"` // Result.Hops, when given; only with failure "none"
}

type liveHop struct {
	Entry    string `json:"entry"`
	Strategy string `json:"strategy"`
	Status   int    `json:"status"`
}

var (
	caseName     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)
	placeholders = regexp.MustCompile(`^TOKEN[0-9]+$`)
)

// expectFailures are the outcomes a links file may expect.
func expectFailures() []string {
	var out []string
	for k := unshorten.FailNone; ; k++ {
		s := k.String()
		if strings.HasPrefix(s, "FailureKind(") {
			break
		}
		if k != unshorten.FailTransient {
			out = append(out, s)
		}
	}
	return append(out, "not_short_link")
}

func policyOf(s string) (unshorten.GenericPolicy, bool) {
	switch s {
	case "", "PreviewOnly":
		return unshorten.PreviewOnly, true
	case "Follow":
		return unshorten.Follow, true
	case "Skip":
		return unshorten.Skip, true
	}
	return 0, false
}

// parseLinks decodes and validates a links file. A live run refuses
// placeholder tokens (TOKENn, the committed example's); example parses the
// committed example, which must hold nothing else in its links' paths and
// query values.
func parseLinks(data []byte, example bool) (*linksFile, error) {
	var lf linksFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&lf); err != nil {
		return nil, fmt.Errorf("links file: %v", err)
	}
	if dec.More() {
		return nil, errors.New("links file: more than one JSON value")
	}
	if lf.Version != 1 {
		return nil, fmt.Errorf("links file: version %d, want 1", lf.Version)
	}
	if len(lf.Links) == 0 {
		return nil, errors.New("links file: no links")
	}
	var errs []error
	seen := map[string]bool{}
	for i, l := range lf.Links {
		where := "links[" + strconv.Itoa(i) + "]"
		bad := func(format string, a ...any) { errs = append(errs, errors.New(where+": "+fmt.Sprintf(format, a...))) }
		u, err := url.Parse(l.URL)
		switch {
		case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || len(l.URL) > 2048:
			bad("url is not an absolute http(s) URL without userinfo, of at most 2048 bytes")
			continue
		case !caseName.MatchString(l.Case):
			bad("case must match %s", caseName)
		case seen[u.Hostname()+"/"+l.Case]:
			bad("case %q repeats for its host", l.Case)
		}
		where = fmt.Sprintf("links[%d] (%s/%s)", i, u.Hostname(), l.Case)
		seen[u.Hostname()+"/"+l.Case] = true
		if example {
			for _, v := range linkValues(u) {
				if !placeholders.MatchString(v) && !structural[strings.ToLower(v)] {
					bad("the example may hold only TOKENn in a link's path and query values")
					break
				}
			}
		} else if strings.Contains(l.URL, "TOKEN") {
			bad("url still holds a placeholder TOKEN; put a real link there")
		}
		if l.Kind != "good" && l.Kind != "invalid" {
			bad("kind must be good or invalid")
		}
		switch l.Origin {
		case "created", "public":
		case "synthetic":
			if l.Kind != "invalid" {
				bad("a synthetic link is an invented code: kind must be invalid")
			}
		default:
			bad("origin must be created, public or synthetic")
		}
		if _, ok := policyOf(l.Generic); !ok {
			bad("generic must be PreviewOnly, Follow or Skip")
		}
		e := l.Expect
		if e.Entry == nil || e.Failure == nil || e.Strategy == nil || e.Status == nil {
			bad("expect needs entry, failure, strategy and status")
			continue
		}
		if !slices.Contains(expectFailures(), *e.Failure) {
			bad("expect.failure must be one of %s", strings.Join(expectFailures(), ", "))
		}
		if *e.Status < 0 || *e.Status > 999 {
			bad("expect.status out of range")
		}
		if e.Path != "" {
			if _, err := regexp.Compile(e.Path); err != nil {
				bad("expect.path does not compile: %v", err)
			}
		}
		switch {
		case *e.Entry == "":
			if *e.Failure != "none" || *e.Strategy != "" || *e.Status != 0 || len(e.Hops) > 0 {
				bad("a link no entry matches makes no request: failure none, strategy \"\", status 0, no hops")
			}
		case *e.Failure == "none":
			if e.Host == "" || e.Path == "" {
				bad("a resolving link needs expect.host and expect.path")
			}
		default:
			if len(e.Hops) > 0 {
				bad("a failed resolution reports no hops")
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &lf, nil
}

// linkValues returns the parts of u that can hold a token: its path
// segments and query values.
func linkValues(u *url.URL) []string {
	var out []string
	for seg := range strings.SplitSeq(strings.Trim(u.EscapedPath(), "/"), "/") {
		if seg != "" {
			out = append(out, strings.TrimPrefix(seg, "@"))
		}
	}
	for kv := range strings.SplitSeq(u.RawQuery, "&") {
		if _, v, _ := strings.Cut(kv, "="); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// parseDeny reads the deny list: one string per line, blank lines and lines
// starting with '#' ignored. An entry shorter than 3 bytes would refuse
// nearly everything and is an error.
func parseDeny(data []byte) ([]string, error) {
	var out []string
	for n, l := range strings.Split(string(data), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if len(l) < 3 {
			return nil, fmt.Errorf("deny list line %d: an entry needs at least 3 bytes", n+1)
		}
		out = append(out, l)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// gitTree reports the nearest ancestor of path (itself included) holding a
// .git entry, checking path as given and with symlinks resolved.
func gitTree(path string) (string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", true // cannot tell: treat as inside
	}
	paths := []string{abs}
	if r, err := filepath.EvalSymlinks(abs); err == nil && r != abs {
		paths = append(paths, r)
	}
	for _, p := range paths {
		for d := p; ; d = filepath.Dir(d) {
			if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
				return d, true
			}
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	return "", false
}

// readInput reads a regular file of at most 1 MiB that lies outside every
// git work tree: the links file and the deny list name the maintainer's
// accounts, so they are never committed. No error quotes the content.
func readInput(flagName, path string) ([]byte, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("-%s: %v", flagName, err)
	}
	if _, in := gitTree(abs); in {
		return nil, fmt.Errorf("-%s is inside a git work tree; keep it outside every repository", flagName)
	}
	fi, err := os.Stat(abs)
	switch {
	case err != nil:
		return nil, fmt.Errorf("-%s: %v", flagName, err)
	case !fi.Mode().IsRegular():
		return nil, fmt.Errorf("-%s is not a regular file", flagName)
	case fi.Size() > 1<<20:
		return nil, fmt.Errorf("-%s is larger than 1 MiB", flagName)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("-%s: %v", flagName, err)
	}
	return b, nil
}

// checkOutDir accepts only an existing directory, given as an absolute
// path, with mode exactly 0700, outside every git work tree: the recorder
// sees raw responses, and nothing it writes may land in a repository or be
// readable by others.
func checkOutDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return errors.New("-livenet.out must be an absolute path")
	}
	if _, in := gitTree(dir); in {
		return errors.New("-livenet.out is inside a git work tree; use a directory outside every repository")
	}
	fi, err := os.Stat(dir)
	switch {
	case err != nil:
		return fmt.Errorf("-livenet.out: %v (create it with mkdir -m 0700)", err)
	case !fi.IsDir():
		return errors.New("-livenet.out is not a directory")
	case fi.Mode().Perm() != 0o700:
		return fmt.Errorf("-livenet.out has mode %04o; it must be 0700", fi.Mode().Perm())
	}
	return nil
}

// writeNew creates path with mode 0600 and its missing parent directories
// with 0700; it never overwrites.
func writeNew(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

// failureOf names the outcome of a Resolve from its error, through the
// sentinels only, as a caller would: a FailureKind name, "not_short_link"
// (an Intermediate entry's stop, failing open) or "unknown".
func failureOf(err error) string {
	var se *unshorten.StatusError
	switch {
	case err == nil:
		return unshorten.FailNone.String()
	case errors.Is(err, unshorten.ErrTransient):
		return unshorten.FailTransient.String()
	case errors.Is(err, unshorten.ErrHopLimit):
		return unshorten.FailHopLimit.String()
	case errors.Is(err, unshorten.ErrLoop):
		return unshorten.FailLoop.String()
	case errors.Is(err, unshorten.ErrFallback):
		return unshorten.FailFallback.String()
	case errors.Is(err, unshorten.ErrNoRedirect):
		return unshorten.FailNoRedirect.String()
	case errors.Is(err, unshorten.ErrNoMatch):
		return unshorten.FailNoMatch.String()
	case errors.Is(err, unshorten.ErrBadTarget):
		return unshorten.FailBadTarget.String()
	case errors.Is(err, unshorten.ErrFetchDenied):
		return unshorten.FailFetchDenied.String()
	case errors.Is(err, unshorten.ErrBudget):
		return unshorten.FailBudget.String()
	case errors.As(err, &se):
		return unshorten.FailStatus.String()
	case errors.Is(err, unshorten.ErrNotShortLink):
		return "not_short_link"
	case errors.Is(err, unshorten.ErrUnresolved):
		return unshorten.FailTransport.String()
	}
	return "unknown"
}

// observedRequest is one RequestInfo.
type observedRequest struct {
	Entry, Strategy, Host string
	Status                int
}

// observed is what one link's resolution did.
type observed struct {
	Entry    string // Match's entry; "" when it did not match
	URL      string // Result.URL
	Clean    string // the Cleaner's Clean of the link
	Fixed    bool   // Clean(Result.URL) == Result.URL
	Failure  string // failureOf(err)
	Hops     []liveHop
	Requests []observedRequest
}

// evaluate checks o against l's expectations. Its problems name fields and
// the non-secret values (entries, strategies, statuses, outcomes, hosts),
// never a path, query or token.
func evaluate(l liveLink, o observed) []string {
	var p []string
	e := l.Expect
	if o.Entry != *e.Entry {
		p = append(p, fmt.Sprintf("Match entry %q, want %q", o.Entry, *e.Entry))
	}
	if o.Failure != *e.Failure {
		p = append(p, fmt.Sprintf("outcome %s, want %s", o.Failure, *e.Failure))
	}
	strategy, status := "", 0
	if n := len(o.Requests); n > 0 {
		strategy, status = o.Requests[n-1].Strategy, o.Requests[n-1].Status
	}
	if strategy != *e.Strategy {
		p = append(p, fmt.Sprintf("last strategy %q, want %q", strategy, *e.Strategy))
	}
	if status != *e.Status {
		p = append(p, fmt.Sprintf("last status %d, want %d", status, *e.Status))
	}
	if *e.Entry == "" && len(o.Requests) > 0 {
		p = append(p, fmt.Sprintf("%d requests for a link that must not match", len(o.Requests)))
	}
	host, path := "", ""
	if u, err := url.Parse(o.URL); err == nil {
		host, path = u.Hostname(), u.EscapedPath()
	}
	if e.Host != "" && !strings.EqualFold(host, e.Host) {
		p = append(p, fmt.Sprintf("destination host %s, want %s", host, e.Host))
	}
	if e.Path != "" {
		if re, err := regexp.Compile(e.Path); err != nil || !re.MatchString(path) {
			p = append(p, "destination path does not match expect.path")
		}
	}
	if len(e.Hops) > 0 && !slices.Equal(o.Hops, e.Hops) {
		p = append(p, fmt.Sprintf("hops %s, want %s", hopsText(o.Hops), hopsText(e.Hops)))
	}
	if (o.Failure != "none" || len(o.Hops) == 0) && o.URL != o.Clean {
		p = append(p, "URL is not Clean(input) although nothing resolved")
	}
	if !o.Fixed {
		p = append(p, "Clean(URL) != URL")
	}
	return p
}

func hopsText(hs []liveHop) string {
	var s []string
	for _, h := range hs {
		s = append(s, fmt.Sprintf("%s/%s/%d", h.Entry, h.Strategy, h.Status))
	}
	return "[" + strings.Join(s, " ") + "]"
}

// caseResult is one row of the results file.
type caseResult struct {
	Case, Kind, Origin, Policy, Entry, Outcome, Strategy, DestHost string
	Status                                                         int
	Pass                                                           bool
	Problems                                                       []string
	Drafts                                                         string   // record mode: what was written
	Entries, Hosts                                                 []string // the entries involved and the hosts that answered, for gate
}

// gateEntry is one registry entry for the gating table.
type gateEntry struct {
	Name         string
	Hosts        []string // Match hosts and FetchHosts
	Intermediate bool
}

func gateEntries(es []unshorten.Entry) ([]gateEntry, error) {
	var out []gateEntry
	for _, e := range es {
		g := gateEntry{Name: e.Name, Intermediate: e.Intermediate}
		for _, m := range e.Match {
			h, err := patternHost(m)
			if err != nil {
				return nil, err
			}
			g.Hosts = append(g.Hosts, h)
		}
		g.Hosts = append(g.Hosts, e.FetchHosts...)
		slices.Sort(g.Hosts)
		g.Hosts = slices.Compact(g.Hosts)
		out = append(out, g)
	}
	return out, nil
}

// patternHost returns the host of a registry pattern ("*://host/path").
func patternHost(p string) (string, error) {
	rest, ok := strings.CutPrefix(p, "*://")
	h, _, _ := strings.Cut(rest, "/")
	if !ok || h == "" {
		return "", fmt.Errorf("registry pattern %q is not *://host/…", p)
	}
	return h, nil
}

// gateRow is one row of the gating table.
type gateRow struct {
	Entry, Good, Invalid, Verdict string
}

// gate decides, per registry entry, whether the run proves it (the release gate):
// at least one good case, every good case passing, at least one invalid case
// (not required of Intermediate entries, which an invalid code may never
// reach), every invalid case passing, and every host of the entry answering
// in a passing case. Anything less is "unproven", with the reasons: the
// entry, or the patterns of its unproven hosts, are removed from
// DefaultRegistry or proven by another case.
func gate(entries []gateEntry, cases []caseResult) []gateRow {
	var out []gateRow
	for _, e := range entries {
		var good, goodPass, inv, invPass int
		answered := map[string]bool{}
		for _, c := range cases {
			if !slices.Contains(c.Entries, e.Name) {
				continue
			}
			switch c.Kind {
			case "good":
				good++
				if c.Pass {
					goodPass++
				}
			case "invalid":
				inv++
				if c.Pass {
					invPass++
				}
			}
			if c.Pass {
				for _, h := range c.Hosts {
					answered[h] = true
				}
			}
		}
		var why []string
		switch {
		case good == 0:
			why = append(why, "no good case")
		case goodPass < good:
			why = append(why, "a good case failed")
		}
		switch {
		case inv == 0 && !e.Intermediate:
			why = append(why, "no invalid case")
		case invPass < inv:
			why = append(why, "an invalid case failed")
		}
		var missing []string
		for _, h := range e.Hosts {
			if !answered[h] {
				missing = append(missing, h)
			}
		}
		if len(missing) > 0 {
			why = append(why, "no passing case reached "+strings.Join(missing, ", "))
		}
		row := gateRow{Entry: e.Name, Good: fmt.Sprintf("%d/%d", goodPass, good), Invalid: fmt.Sprintf("%d/%d", invPass, inv), Verdict: "proven"}
		if inv == 0 && e.Intermediate {
			row.Invalid = "n/a"
		}
		if len(why) > 0 {
			row.Verdict = "unproven: " + strings.Join(why, "; ")
		}
		out = append(out, row)
	}
	return out
}

// liveResults is the data of the results template.
type liveResults struct {
	Mode, Date, Egress, Commit, GoVersion, UserAgent string
	Marketing                                        bool
	Cases                                            []caseResult
	Gating                                           []gateRow
	Passed, Failed                                   int
}

// mdCell makes s one Markdown table cell.
func mdCell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "-"
	}
	return strings.ReplaceAll(s, "|", `\|`)
}

func renderResults(tmpl string, r liveResults) ([]byte, error) {
	t, err := template.New("results").Option("missingkey=error").
		Funcs(template.FuncMap{"cell": mdCell, "join": strings.Join}).Parse(tmpl)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, r); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// linkOriginals returns every value of the links' URLs that could be a
// token, for the residual check of the results file.
func linkOriginals(lf *linksFile) []string {
	var out []string
	for _, l := range lf.Links {
		if u, err := url.Parse(l.URL); err == nil {
			for _, v := range linkValues(u) {
				if len(v) >= minToken && !structural[strings.ToLower(v)] {
					out = append(out, v)
				}
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// --- tests ---

func TestLinksExample(t *testing.T) {
	b, err := os.ReadFile(liveExamplePath)
	if err != nil {
		t.Fatal(err)
	}
	lf, err := parseLinks(b, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseLinks(b, false); err == nil || !strings.Contains(err.Error(), "placeholder TOKEN") {
		t.Errorf("a live run accepts the example's placeholders: %v", err)
	}
	kinds := map[string]bool{}
	for _, l := range lf.Links {
		kinds[l.Kind+"/"+l.Origin] = true
	}
	for _, k := range []string{"good/created", "good/public", "invalid/synthetic"} {
		if !kinds[k] {
			t.Errorf("the example has no %s link", k)
		}
	}
}

// TestLinksSchema holds testdata/livenet/links.schema.json to the Go
// types: the same properties, and "required" exactly the fields without
// omitempty.
func TestLinksSchema(t *testing.T) {
	b, err := os.ReadFile(liveSchemaPath)
	if err != nil {
		t.Fatal(err)
	}
	type object struct {
		Properties           map[string]json.RawMessage `json:"properties"`
		Required             []string                   `json:"required"`
		AdditionalProperties *bool                      `json:"additionalProperties"`
	}
	var schema struct {
		object
		Defs map[string]object `json:"$defs"`
	}
	if err := json.Unmarshal(b, &schema); err != nil {
		t.Fatal(err)
	}
	for name, typ := range map[string]reflect.Type{
		"": reflect.TypeFor[linksFile](), "link": reflect.TypeFor[liveLink](),
		"expect": reflect.TypeFor[liveExpect](), "hop": reflect.TypeFor[liveHop](),
	} {
		o, ok := schema.Defs[name]
		if name == "" {
			o, ok = schema.object, true
		}
		if !ok {
			t.Errorf("schema has no $defs.%s", name)
			continue
		}
		var props, req []string
		for i := range typ.NumField() {
			tag, opt, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			props = append(props, tag)
			if opt != "omitempty" {
				req = append(req, tag)
			}
		}
		var got []string
		for p := range o.Properties {
			got = append(got, p)
		}
		slices.Sort(props)
		slices.Sort(req)
		slices.Sort(got)
		gotReq := slices.Sorted(slices.Values(o.Required))
		if !slices.Equal(got, props) || !slices.Equal(gotReq, req) || o.AdditionalProperties == nil || *o.AdditionalProperties {
			t.Errorf("schema %q: properties %q required %q additionalProperties %v; want properties %q required %q, additionalProperties false",
				name, got, gotReq, o.AdditionalProperties, props, req)
		}
	}
	var failure struct {
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(schema.Defs["expect"].Properties["failure"], &failure); err != nil || !slices.Equal(failure.Enum, expectFailures()) {
		t.Errorf("schema expect.failure enum %q (%v), want %q", failure.Enum, err, expectFailures())
	}
}

func TestParseLinksRejects(t *testing.T) {
	ok := `{"case":"good","url":"https://t.co/Ab3dEf9","kind":"good","origin":"created",` +
		`"expect":{"entry":"t.co","failure":"none","strategy":"Redirect","status":301,"host":"github.com","path":"^/x$"}}`
	file := func(links ...string) []byte {
		return []byte(`{"version":1,"links":[` + strings.Join(links, ",") + `]}`)
	}
	if _, err := parseLinks(file(ok), false); err != nil {
		t.Fatalf("valid file: %v", err)
	}
	for _, tc := range []struct{ name, from, to, want string }{
		{"unknown field", `"kind"`, `"color":"red","kind"`, "unknown field"},
		{"bad url", `https://t.co/Ab3dEf9`, `t.co/x`, "absolute http(s) URL"},
		{"userinfo", `https://t.co`, `https://u:p@t.co`, "absolute http(s) URL"},
		{"case", `"case":"good"`, `"case":"Good"`, "case must match"},
		{"placeholder", `Ab3dEf9`, `TOKEN1`, "placeholder TOKEN"},
		{"kind", `"kind":"good"`, `"kind":"ok"`, "kind must be"},
		{"origin", `"origin":"created"`, `"origin":"found"`, "origin must be"},
		{"synthetic good", `"origin":"created"`, `"origin":"synthetic"`, "kind must be invalid"},
		{"policy", `"kind"`, `"generic":"Always","kind"`, "generic must be"},
		{"missing status", `,"status":301`, ``, "expect needs"},
		{"failure", `"failure":"none"`, `"failure":"transient"`, "expect.failure must be"},
		{"path", `"^/x$"`, `"("`, "does not compile"},
		{"no host", `"host":"github.com",`, ``, "needs expect.host"},
		{"no-match with request", `"entry":"t.co"`, `"entry":""`, "makes no request"},
		{"failure with hops", `"failure":"none"`, `"failure":"fallback","hops":[{"entry":"t.co","strategy":"Redirect","status":301}]`, "reports no hops"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := strings.Replace(ok, tc.from, tc.to, 1)
			if bad == ok {
				t.Fatalf("replacement %q not applied", tc.from)
			}
			if _, err := parseLinks(file(bad), false); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("parseLinks = %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := parseLinks(file(ok, ok), false); err == nil || !strings.Contains(err.Error(), "repeats") {
		t.Errorf("a repeated case: %v", err)
	}
	if _, err := parseLinks([]byte(`{"version":2,"links":[`+ok+`]}`), false); err == nil {
		t.Error("version 2 accepted")
	}
	if _, err := parseLinks(append(file(ok), "{}"...), false); err == nil {
		t.Error("trailing value accepted")
	}
	if _, err := parseLinks(file(strings.Replace(ok, "Ab3dEf9", "real9Tok", 1)), true); err == nil {
		t.Error("the example check accepts a real-looking token")
	}
}

func TestParseDeny(t *testing.T) {
	got, err := parseDeny([]byte("# accounts\n  someuser \n\naff-20x\nsomeuser\n"))
	if err != nil || !slices.Equal(got, []string{"aff-20x", "someuser"}) {
		t.Errorf("parseDeny = %q, %v", got, err)
	}
	if _, err := parseDeny([]byte("ab\n")); err == nil {
		t.Error("a 2-byte entry accepted")
	}
}

func TestLiveFileChecks(t *testing.T) {
	if _, in := gitTree("."); !in {
		t.Skip("the package directory is not in a git work tree")
	}
	here, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkOutDir(here); err == nil || !strings.Contains(err.Error(), "git work tree") {
		t.Errorf("checkOutDir(package dir) = %v", err)
	}
	if _, err := readInput("livenet.links", liveExamplePath); err == nil || !strings.Contains(err.Error(), "git work tree") {
		t.Errorf("readInput(committed example) = %v", err)
	}
	dir := t.TempDir()
	if _, in := gitTree(dir); in {
		t.Skip("the temporary directory is inside a git work tree")
	}
	out := filepath.Join(dir, "out")
	if err := checkOutDir(out); err == nil {
		t.Error("a missing directory accepted")
	}
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkOutDir(out); err == nil || !strings.Contains(err.Error(), "0700") {
		t.Errorf("checkOutDir(0755) = %v", err)
	}
	if err := os.Chmod(out, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkOutDir(out); err != nil {
		t.Errorf("checkOutDir(0700) = %v", err)
	}
	if err := checkOutDir("relative/out"); err == nil {
		t.Error("a relative path accepted")
	}
	p := filepath.Join(out, "drafts", "t.co", "good.txtar")
	if err := writeNew(p, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writeNew(p, []byte("y")); err == nil {
		t.Error("writeNew overwrote a file")
	}
	for _, c := range []struct {
		path string
		perm os.FileMode
	}{{p, 0o600}, {filepath.Dir(p), 0o700}} {
		if fi, err := os.Stat(c.path); err != nil || fi.Mode().Perm() != c.perm {
			t.Errorf("%s: mode %v, %v; want %04o", c.path, fi.Mode().Perm(), err, c.perm)
		}
	}
	in := filepath.Join(dir, "links.json")
	if err := os.WriteFile(in, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := readInput("livenet.links", in); err != nil || string(b) != "{}" {
		t.Errorf("readInput = %q, %v", b, err)
	}
	if _, err := readInput("livenet.links", dir); err == nil {
		t.Error("readInput accepts a directory")
	}
}

func TestFailureOf(t *testing.T) {
	wrap := func(errs ...error) error { return errors.Join(append([]error{unshorten.ErrUnresolved}, errs...)...) }
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, "none"},
		{wrap(unshorten.ErrTransient, &unshorten.StatusError{Host: "t.co", Status: 503}), "transient"},
		{wrap(unshorten.ErrFallback), "fallback"},
		{wrap(unshorten.ErrNoRedirect), "no_redirect"},
		{wrap(&unshorten.StatusError{Host: "lnkd.in", Status: 999}), "status"},
		{wrap(unshorten.ErrNotShortLink), "not_short_link"},
		{wrap(), "transport"},
		{errors.New("x"), "unknown"},
	} {
		if got := failureOf(tc.err); got != tc.want {
			t.Errorf("failureOf(%v) = %s, want %s", tc.err, got, tc.want)
		}
	}
}

func TestEvaluate(t *testing.T) {
	str := func(s string) *string { return &s }
	num := func(n int) *int { return &n }
	l := liveLink{Expect: liveExpect{
		Entry: str("t.co"), Failure: str("none"), Strategy: str("Redirect"), Status: num(301),
		Host: "github.com", Path: "^/Espresso-Aficionados/linkclean$", Hops: []liveHop{{"t.co", "Redirect", 301}},
	}}
	o := observed{
		Entry: "t.co", URL: "https://github.com/Espresso-Aficionados/linkclean", Clean: "https://t.co/x", Fixed: true, Failure: "none",
		Hops: []liveHop{{"t.co", "Redirect", 301}}, Requests: []observedRequest{{"t.co", "Redirect", "t.co", 301}},
	}
	if p := evaluate(l, o); len(p) != 0 {
		t.Errorf("a matching observation: %q", p)
	}
	o.URL, o.Hops, o.Failure = "https://t.co/x", nil, "fallback"
	o.Requests[0].Status = 307
	p := evaluate(l, o)
	want := []string{"outcome fallback, want none", "last status 307, want 301", "destination host t.co, want github.com",
		"destination path does not match expect.path", "hops [], want [t.co/Redirect/301]"}
	if !slices.Equal(p, want) {
		t.Errorf("problems\n got %q\nwant %q", p, want)
	}
	o.URL = "https://t.co/other"
	o.Fixed = false
	if p := evaluate(l, o); !slices.Contains(p, "URL is not Clean(input) although nothing resolved") || !slices.Contains(p, "Clean(URL) != URL") {
		t.Errorf("invariants not checked: %q", p)
	}
	l.Expect = liveExpect{Entry: str(""), Failure: str("none"), Strategy: str(""), Status: num(0)}
	if p := evaluate(l, observed{URL: "u", Clean: "u", Fixed: true, Failure: "none", Requests: []observedRequest{{}}}); !slices.Contains(p, "1 requests for a link that must not match") {
		t.Errorf("a request for an unmatched link: %q", p)
	}
}

func TestGate(t *testing.T) {
	entries := []gateEntry{
		{Name: "amazon", Hosts: []string{"a.co", "amzn.to"}},
		{Name: "pinterest-api", Hosts: []string{"api.pinterest.com"}, Intermediate: true},
		{Name: "t.co", Hosts: []string{"t.co"}},
		{Name: "fb.me", Hosts: []string{"fb.me"}},
	}
	cases := []caseResult{
		{Kind: "good", Pass: true, Entries: []string{"amazon"}, Hosts: []string{"amzn.to"}},
		{Kind: "invalid", Pass: true, Entries: []string{"amazon"}, Hosts: []string{"amzn.to"}},
		{Kind: "good", Pass: true, Entries: []string{"pinterest", "pinterest-api"}, Hosts: []string{"pin.it", "api.pinterest.com"}},
		{Kind: "good", Pass: true, Entries: []string{"t.co"}, Hosts: []string{"t.co"}},
		{Kind: "invalid", Pass: true, Entries: []string{"t.co"}, Hosts: []string{"t.co"}},
		{Kind: "good", Pass: false, Entries: []string{"fb.me"}, Hosts: []string{"fb.me"}},
	}
	want := []gateRow{
		{"amazon", "1/1", "1/1", "unproven: no passing case reached a.co"},
		{"pinterest-api", "1/1", "n/a", "proven"},
		{"t.co", "1/1", "1/1", "proven"},
		{"fb.me", "0/1", "0/0", "unproven: a good case failed; no invalid case; no passing case reached fb.me"},
	}
	if got := gate(entries, cases); !slices.Equal(got, want) {
		t.Errorf("gate\n got %+v\nwant %+v", got, want)
	}
	if h, err := patternHost("*://www.google.com/share.google*"); err != nil || h != "www.google.com" {
		t.Errorf("patternHost = %q, %v", h, err)
	}
	if _, err := patternHost("https://t.co/*"); err == nil {
		t.Error("patternHost accepts a non-* scheme")
	}
}

func TestRenderResults(t *testing.T) {
	tmpl, err := os.ReadFile(liveTemplatePath)
	if err != nil {
		t.Fatal(err)
	}
	r := liveResults{
		Mode: "check", Date: "2026-10-18", Egress: liveEgress, Commit: "0123456789abcdef0123456789abcdef01234567",
		GoVersion: "go1.26.0", UserAgent: "linkclean/devel", Passed: 1, Failed: 1,
		Cases: []caseResult{
			{Case: "t.co/good", Kind: "good", Origin: "created", Policy: "PreviewOnly", Entry: "t.co", Outcome: "none", Strategy: "Redirect", Status: 301, DestHost: "github.com", Pass: true},
			{Case: "lnkd.in/invalid", Kind: "invalid", Origin: "synthetic", Policy: "PreviewOnly", Entry: "lnkd.in", Outcome: "status", Strategy: "BodyRegexp", Status: 999, DestHost: "lnkd.in", Problems: []string{"outcome status, want no_match", "a|b"}},
		},
		Gating: []gateRow{{"t.co", "1/1", "0/0", "unproven: no invalid case"}},
	}
	for _, mode := range []string{"check", "record"} {
		r.Mode = mode
		b, err := renderResults(string(tmpl), r)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		for _, want := range []string{
			"- egress: workstation\n", "- mode: " + mode + "\n",
			"| t.co/good | good | created | PreviewOnly | t.co | none | Redirect | 301 | github.com | pass |",
			`FAIL: outcome status, want no_match; a\|b |`,
			"| t.co | 1/1 | 0/0 | unproven: no invalid case |",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("%s results lack %q:\n%s", mode, want, s)
			}
		}
		if got := strings.Contains(s, "| drafts |"); got != (mode == "record") {
			t.Errorf("%s results: drafts column %v", mode, got)
		}
		if p := refusals(s, nil, nil); len(p) > 0 {
			t.Errorf("%s results refused: %q", mode, p)
		}
	}
	if _, err := renderResults("{{.Nope}}", r); err == nil {
		t.Error("a template naming a missing field rendered")
	}
}
