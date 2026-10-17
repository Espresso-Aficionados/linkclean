package linkclean

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Espresso-Aficionados/linkclean/internal/brave"
)

// Gate messages. This block is a copy of the shared strings of
// tools/rulegen/internal/data/messages.go; a tools test compares the two, so
// change both together.
const (
	msgHeld              = "held: %s %s (%s, expires %s)"                         // file, sha256[:12], reason, expires
	msgGitlinkBackwards  = "gitlink moved backwards: %s is not an ancestor of %s" // B, G
	msgNotSynced         = "vendored bytes ≠ upstream@%s; run `rulegen sync`"     // G
	msgUnreachable       = "upstream unreachable; re-run the job"
	msgInputsUnchanged   = "provenance inputs unchanged from base"
	msgGitlinkMissing    = "upstream does not serve the gitlink %s (gitlink-missing)"       // G
	msgNotOnMaster       = "gitlink %s is not an ancestor of upstream master"               // G
	msgLockNotAncestor   = "lock commit %s is neither the gitlink %s nor an ancestor of it" // X, G
	msgLockBlobs         = "%s: blob %s at lock commit %s, lock %s"                         // upstream path, blob, X, locked blob
	msgNotGitlink        = "%s is not a gitlink (mode 160000)"                              // submodule path
	msgGitmodulesChanged = ".gitmodules differs from base's"
	msgGitmodulesMapping = ".gitmodules does not map %s to %s with branch = master: %v" // path, URL, problem
	msgHistoryRewritten  = "upstream history rewritten: %s is no longer on master"      // B
	msgAckLapsed         = "ack lapsed (bound to %s)"                                   // the ack's vendored_sha256
	msgAckExpired        = "ack expired (%s)"                                           // expires
	msgAcksExpired       = "%s: %s expired on %s"                                       // acks path, entry, expires
	msgAcksMalformed     = "%s: malformed entry: %v"                                    // acks path, problem
	msgSkipNoBase        = "skipped (no LINKCLEAN_BASE_DIR): %s"                        // part
	msgSkipNoUpstream    = "skipped (no LINKCLEAN_UPSTREAM_DIR): %s"                    // part

	statusFile              = "STATUS"
	statusOK                = "ok"
	statusGitlinkMissing    = "gitlink-missing"
	statusUnreachablePrefix = "unreachable: "

	submodulePath = "tools/upstream/adblock-lists"
	canonicalURL  = "https://github.com/brave/adblock-lists"
	upstreamRef   = "refs/remotes/origin/master"
	acksPath      = "rules/acks.json"
)

// The base-relative parts, as named in their skip lines.
const (
	partCountsRelative = "Counts relative floor"
	partDebounceDrop   = "Counts debounce include-drop limit"
	partRisky          = "Risky"
	partGoldenC        = "Golden (c)"
	partAcksChanged    = "Provenance acks-changed check"
	partHolds          = "Provenance hold match"
	partUpstream       = "Provenance Upstream checks"
)

// acksDoc is rules/acks.json (schema 1, §11.2): human-owned acks and holds.
type acksDoc struct {
	Schema int         `json:"schema"`
	Acks   []ackEntry  `json:"acks"`
	Holds  []holdEntry `json:"holds"`
}

// ackEntry lets one gate pass on data bound by sha256. Vocabulary:
//   - check "counts": metric <clean-urls|query-filter>.{rules,params,
//     global_params,scoped_params}, union.{params,global_params,scoped_params}
//     or debounce.includes, with min, the lowest value it accepts;
//   - check "golden": metric corpus.exact or corpus.recall_removed, with min;
//   - check "risky": scope ("global" or an include pattern) and param.
//
// vendored_sha256 is that of the file a per-file metric names
// (debounce.includes: debounce.json), and otherwise that of any Brave file
// the PR changes relative to base's lock.
type ackEntry struct {
	Check          string `json:"check"`
	Metric         string `json:"metric,omitempty"`
	Min            *int   `json:"min,omitempty"`
	Scope          string `json:"scope,omitempty"`
	Param          string `json:"param,omitempty"`
	VendoredSHA256 string `json:"vendored_sha256"`
	Expires        string `json:"expires"`
	Reason         string `json:"reason"`
	By             string `json:"by"`
}

// holdEntry keeps the bytes with sha256 of a locked Brave file off main.
type holdEntry struct {
	File    string `json:"file"`
	SHA256  string `json:"sha256"`
	Expires string `json:"expires"`
	Reason  string `json:"reason"`
	By      string `json:"by"`
}

// ackMetrics are the counts and golden metrics an ack may name.
var ackMetrics = map[string][]string{
	"counts": {
		"clean-urls.rules", "clean-urls.params", "clean-urls.global_params", "clean-urls.scoped_params",
		"query-filter.rules", "query-filter.params", "query-filter.global_params", "query-filter.scoped_params",
		"union.params", "union.global_params", "union.scoped_params", "debounce.includes",
	},
	"golden": {"corpus.exact", "corpus.recall_removed"},
}

var (
	hex64Re = regexp.MustCompile(`^[0-9a-f]{64}$`)
	dateRe  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// parseAcks decodes rules/acks.json strictly and validates every entry. On a
// decode error the doc is empty; otherwise malformed entries are reported and
// kept, and acksUsable drops them.
func parseAcks(data []byte) (acksDoc, []error) {
	var doc acksDoc
	if err := brave.CheckDuplicateKeys(data); err != nil {
		return acksDoc{}, []error{err}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return acksDoc{}, []error{err}
	}
	if _, err := dec.Token(); err != io.EOF {
		return acksDoc{}, []error{errors.New("trailing data after the JSON value")}
	}
	var errs []error
	if doc.Schema != 1 {
		errs = append(errs, fmt.Errorf("schema %d, want 1", doc.Schema))
	}
	if doc.Acks == nil || doc.Holds == nil {
		errs = append(errs, errors.New(`"acks" and "holds" must both be arrays`))
	}
	for i := range doc.Acks {
		if err := doc.Acks[i].validate(); err != nil {
			errs = append(errs, fmt.Errorf("acks[%d]: %w", i, err))
		}
	}
	for i := range doc.Holds {
		if err := doc.Holds[i].validate(); err != nil {
			errs = append(errs, fmt.Errorf("holds[%d]: %w", i, err))
		}
	}
	return doc, errs
}

func validateDate(s string) error {
	if !dateRe.MatchString(s) {
		return fmt.Errorf("expires %q is not YYYY-MM-DD", s)
	}
	if _, err := time.Parse(time.DateOnly, s); err != nil {
		return fmt.Errorf("expires %q: %v", s, err)
	}
	return nil
}

func (a *ackEntry) validate() error {
	var errs []error
	switch a.Check {
	case "counts", "golden":
		if !slices.Contains(ackMetrics[a.Check], a.Metric) {
			errs = append(errs, fmt.Errorf("check %s: unknown metric %q", a.Check, a.Metric))
		}
		if a.Min == nil || *a.Min < 0 {
			errs = append(errs, fmt.Errorf("check %s: min must be a non-negative integer", a.Check))
		}
		if a.Scope != "" || a.Param != "" {
			errs = append(errs, fmt.Errorf("check %s: scope and param are for risky acks", a.Check))
		}
	case "risky":
		if a.Scope == "" || a.Param == "" {
			errs = append(errs, errors.New("check risky: scope and param are required"))
		}
		if a.Metric != "" || a.Min != nil {
			errs = append(errs, errors.New("check risky: metric and min are for counts and golden acks"))
		}
	default:
		errs = append(errs, fmt.Errorf("unknown check %q (want counts, golden or risky)", a.Check))
	}
	if !hex64Re.MatchString(a.VendoredSHA256) {
		errs = append(errs, fmt.Errorf("vendored_sha256 %q is not 64-hex", a.VendoredSHA256))
	}
	if err := validateDate(a.Expires); err != nil {
		errs = append(errs, err)
	}
	if strings.TrimSpace(a.Reason) == "" || strings.TrimSpace(a.By) == "" {
		errs = append(errs, errors.New("reason and by are required"))
	}
	return errors.Join(errs...)
}

func (h *holdEntry) validate() error {
	var errs []error
	if !slices.ContainsFunc(lockedPaths[:3], func(p struct{ upstream, vendored string }) bool { return p.upstream == h.File }) {
		errs = append(errs, fmt.Errorf("file %q is not a locked Brave path", h.File))
	}
	if !hex64Re.MatchString(h.SHA256) {
		errs = append(errs, fmt.Errorf("sha256 %q is not 64-hex", h.SHA256))
	}
	if err := validateDate(h.Expires); err != nil {
		errs = append(errs, err)
	}
	if strings.TrimSpace(h.Reason) == "" || strings.TrimSpace(h.By) == "" {
		errs = append(errs, errors.New("reason and by are required"))
	}
	return errors.Join(errs...)
}

// expired reports whether now is after the end of the UTC date expires
// (expires is inclusive). A malformed date counts as expired.
func expired(expires string, now time.Time) bool {
	d, err := time.Parse(time.DateOnly, expires)
	return err != nil || !now.Before(d.Add(24*time.Hour))
}

// acks returns the well-formed entries of the working tree's rules/acks.json.
// A file that does not decode yields none: rules-schema fails it.
func (e *gateEnv) acks() acksDoc {
	data, err := fs.ReadFile(e.tree, acksPath)
	if err != nil {
		return acksDoc{}
	}
	doc, _ := parseAcks(data)
	var out acksDoc
	for _, a := range doc.Acks {
		if a.validate() == nil {
			out.Acks = append(out.Acks, a)
		}
	}
	for _, h := range doc.Holds {
		if h.validate() == nil {
			out.Holds = append(out.Holds, h)
		}
	}
	return out
}

// changedBraveSHAs returns the head sha256 of every Brave file whose bytes
// differ from base's lock. e.base must be set.
func (e *gateEnv) changedBraveSHAs() map[string]bool {
	out := map[string]bool{}
	baseLock, _ := readLock(e.base)
	for _, p := range lockedPaths[:3] {
		data, err := fs.ReadFile(e.data, p.vendored)
		if err != nil {
			continue
		}
		if sum := sha256Hex(data); sum != baseLock.Upstream.Files[p.upstream].SHA256 {
			out[sum] = true
		}
	}
	return out
}

// headSHA is the sha256 of a head vendored file, or "".
func (e *gateEnv) headSHA(vendored string) string {
	data, err := fs.ReadFile(e.data, vendored)
	if err != nil {
		return ""
	}
	return sha256Hex(data)
}

// ackFor finds an ack satisfying match whose binding holds: bound to the
// sha256 of bindFile (a vendored path), or with bindFile "", to that of any
// Brave file the PR changes. It logs every lapsed or expired candidate, which
// never satisfies a gate (§11.2).
func (e *gateEnv) ackFor(t testing.TB, match func(*ackEntry) bool, bindFile string) (*ackEntry, bool) {
	t.Helper()
	bound := map[string]bool{}
	if bindFile != "" {
		bound[e.headSHA(bindFile)] = true
	} else {
		bound = e.changedBraveSHAs()
	}
	doc := e.acks()
	for i := range doc.Acks {
		a := &doc.Acks[i]
		if !match(a) {
			continue
		}
		switch {
		case !bound[a.VendoredSHA256]:
			t.Logf("note: "+msgAckLapsed, a.VendoredSHA256)
		case expired(a.Expires, e.now):
			t.Logf("note: "+msgAckExpired, a.Expires)
		default:
			return a, true
		}
	}
	return nil, false
}

// checkAcksChanged fails when the PR changes rules/acks.json and leaves an
// expired or malformed entry in it (§11.2). An expired entry in an unchanged
// file is a note.
func checkAcksChanged(t testing.TB, e *gateEnv) {
	t.Helper()
	if skipNoBase(t, e, partAcksChanged) {
		return
	}
	head, err := fs.ReadFile(e.tree, acksPath)
	if err != nil {
		return // rules-schema reports a missing file
	}
	report := func(format string, args ...any) { t.Logf("note: "+format, args...) }
	if base, err := fs.ReadFile(e.base, acksPath); err != nil || !bytes.Equal(head, base) {
		report = t.Errorf
	}
	doc, errs := parseAcks(head)
	for _, err := range errs {
		report(msgAcksMalformed, acksPath, err)
	}
	for i, a := range doc.Acks {
		if expired(a.Expires, e.now) {
			report(msgAcksExpired, acksPath, fmt.Sprintf("acks[%d] (%s %s%s%s)", i, a.Check, a.Metric, a.Scope, a.Param), a.Expires)
		}
	}
	for i, h := range doc.Holds {
		if expired(h.Expires, e.now) {
			report(msgAcksExpired, acksPath, fmt.Sprintf("holds[%d] (%s)", i, h.File), h.Expires)
		}
	}
}

// checkHolds fails on every matching hold: unexpired, the PR's vendored copy
// of its file has exactly its sha256, and that differs from base's lock
// (§11.2). One held file keeps the whole update off main.
func checkHolds(t testing.TB, e *gateEnv, lock *lockFile) {
	t.Helper()
	if skipNoBase(t, e, partHolds) {
		return
	}
	baseLock, err := readLock(e.base)
	if err != nil {
		t.Errorf("base: %v", err)
		return
	}
	for _, h := range e.acks().Holds {
		lf, ok := lock.Upstream.Files[h.File]
		if !ok || expired(h.Expires, e.now) {
			continue
		}
		if e.headSHA(lf.Vendored) == h.SHA256 && baseLock.Upstream.Files[h.File].SHA256 != h.SHA256 {
			t.Errorf(msgHeld, h.File, h.SHA256[:12], h.Reason, h.Expires)
		}
	}
}

// provenanceInputsChanged reports whether the tree under test differs from
// base in the gitlink, .gitmodules, rules/brave/* or rules.lock.json.
func provenanceInputsChanged(e *gateEnv) bool {
	read := func(fsys fs.FS, p string) []byte {
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil
		}
		return b
	}
	head := func(p string) []byte {
		if b := read(e.data, p); b != nil {
			return b
		}
		return read(e.tree, p)
	}
	baseGitlink := strings.TrimSpace(string(read(e.base, "GITLINK")))
	if baseGitlink != e.headGitlink || !bytes.Equal(read(e.base, ".gitmodules"), read(e.tree, ".gitmodules")) ||
		!bytes.Equal(read(e.base, lockPath), head(lockPath)) {
		return true
	}
	names := map[string]bool{}
	for _, fsys := range []fs.FS{e.data, e.tree, e.base} {
		m, _ := fs.Glob(fsys, "rules/brave/*")
		for _, p := range m {
			names[p] = true
		}
	}
	for p := range names {
		hb, bb := head(p), read(e.base, p)
		if hb == nil != (bb == nil) || !bytes.Equal(hb, bb) {
			return true
		}
	}
	return false
}

// checkUpstream is the Upstream part of rules-provenance (§11.3, D1). It runs
// only when a provenance input differs from base, so an upstream outage or
// history rewrite cannot turn unrelated PRs red (UM-R3-5).
func checkUpstream(t testing.TB, e *gateEnv, lock *lockFile) {
	t.Helper()
	if skipNoBase(t, e, partUpstream) {
		return
	}
	if !provenanceInputsChanged(e) {
		t.Logf("%s skipped: %s", partUpstream, msgInputsUnchanged)
		return
	}
	if e.upstream == "" {
		t.Logf(msgSkipNoUpstream, partUpstream)
		return
	}

	head, herr := fs.ReadFile(e.tree, ".gitmodules")
	if base, err := fs.ReadFile(e.base, ".gitmodules"); err == nil && !bytes.Equal(base, head) {
		t.Error(msgGitmodulesChanged)
	}
	if herr != nil {
		t.Errorf(msgGitmodulesMapping, submodulePath, canonicalURL, herr)
	} else if err := checkGitmodulesMapping(head); err != nil {
		t.Errorf(msgGitmodulesMapping, submodulePath, canonicalURL, err)
	}
	g := e.headGitlink
	if g == "" {
		t.Errorf(msgNotGitlink, submodulePath)
		return
	}

	status, err := os.ReadFile(filepath.Join(e.upstream, statusFile))
	if err != nil {
		t.Errorf("upstream clone: %v", err)
		return
	}
	switch line, _, _ := strings.Cut(string(status), "\n"); {
	case line == statusOK:
	case line == statusGitlinkMissing:
		t.Errorf(msgGitlinkMissing, g)
		return
	case strings.HasPrefix(line, statusUnreachablePrefix):
		t.Errorf("%s (%s)", msgUnreachable, strings.TrimPrefix(line, statusUnreachablePrefix))
		return
	default:
		t.Errorf("upstream clone: STATUS %q", line)
		return
	}

	up := upstreamClone{t: t, dir: e.upstream}
	if !up.hasCommit(g) {
		t.Errorf(msgGitlinkMissing, g)
		return
	}
	if !up.isAncestor(g, upstreamRef) {
		t.Errorf(msgNotOnMaster, g)
	}
	if b, err := fs.ReadFile(e.base, "GITLINK"); err == nil {
		if b := strings.TrimSpace(string(b)); b != g && !(up.hasCommit(b) && up.isAncestor(b, g)) {
			if up.hasCommit(b) && up.isAncestor(b, upstreamRef) {
				t.Errorf(msgGitlinkBackwards, b, g)
			} else {
				// A history rewrite: summary reports it (§11.6).
				t.Logf("note: "+msgHistoryRewritten, b)
			}
		}
	}
	unsynced := false
	for _, p := range lockedPaths {
		blob, ok := up.blob(g, p.upstream)
		switch {
		case !ok:
			t.Errorf("%s is missing at upstream@%s", p.upstream, g)
		case blob != lock.Upstream.Files[p.upstream].GitBlobSHA1:
			unsynced = true
			t.Logf("%s: blob %s at upstream@%s, lock %s", p.upstream, blob, g, lock.Upstream.Files[p.upstream].GitBlobSHA1)
		}
	}
	if unsynced {
		t.Errorf(msgNotSynced, g)
	}
	if x := lock.Upstream.Commit; x != g {
		if !up.hasCommit(x) || !up.isAncestor(x, g) {
			t.Errorf(msgLockNotAncestor, x, g)
			return
		}
		for _, p := range lockedPaths {
			if blob, _ := up.blob(x, p.upstream); blob != lock.Upstream.Files[p.upstream].GitBlobSHA1 {
				t.Errorf(msgLockBlobs, p.upstream, blob, x, lock.Upstream.Files[p.upstream].GitBlobSHA1)
			}
		}
	}
}

// upstreamClone runs git in the clone `rulegen upstream` made: commits and
// trees only. Lazy fetches are off, so a missing object is an answer, not a
// network request.
type upstreamClone struct {
	t   testing.TB
	dir string
}

// git returns git's stdout and exit code; any failure other than a non-zero
// exit fails the test.
func (u upstreamClone) git(args ...string) (string, int) {
	cmd := exec.Command("git", append([]string{"-C", u.dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1", "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		return "", ee.ExitCode()
	case err != nil:
		u.t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out)), 0
}

func (u upstreamClone) hasCommit(sha string) bool {
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(sha) {
		return false
	}
	_, code := u.git("cat-file", "-e", sha+"^{commit}")
	return code == 0
}

// isAncestor is merge-base --is-ancestor; a is an ancestor of itself.
func (u upstreamClone) isAncestor(a, b string) bool {
	_, code := u.git("merge-base", "--is-ancestor", a, b)
	if code > 1 {
		u.t.Errorf("git merge-base --is-ancestor %s %s: exit %d", a, b, code)
	}
	return code == 0
}

// blob is the blob ID at rev:path, read from the tree (no blob is fetched).
func (u upstreamClone) blob(rev, path string) (string, bool) {
	out, code := u.git("rev-parse", "--verify", "--quiet", rev+":"+path)
	return out, code == 0
}

// checkGitmodulesMapping: .gitmodules has exactly one entry for the
// submodule path, with the canonical URL (with or without .git) and branch
// master.
func checkGitmodulesMapping(data []byte) error {
	subs, err := parseGitmodules(data)
	if err != nil {
		return err
	}
	var found []map[string]string
	for _, s := range subs {
		if s["path"] == submodulePath {
			found = append(found, s)
		}
	}
	switch {
	case len(found) == 0:
		return fmt.Errorf("no submodule with path %s", submodulePath)
	case len(found) > 1:
		return fmt.Errorf("%d submodules with path %s", len(found), submodulePath)
	case found[0]["url"] != canonicalURL && found[0]["url"] != canonicalURL+".git":
		return fmt.Errorf("url %q", found[0]["url"])
	case found[0]["branch"] != "master":
		return fmt.Errorf("branch %q", found[0]["branch"])
	}
	return nil
}

// parseGitmodules reads the [submodule "name"] sections of a .gitmodules
// file (git-config syntax: comments, case-insensitive keys, quoted values).
func parseGitmodules(data []byte) ([]map[string]string, error) {
	var out []map[string]string
	var cur map[string]string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			name, ok := strings.CutPrefix(line, `[submodule "`)
			if !ok || !strings.HasSuffix(name, `"]`) {
				return nil, fmt.Errorf(".gitmodules:%d: unexpected section %q", n, line)
			}
			cur = map[string]string{"name": strings.TrimSuffix(name, `"]`)}
			out = append(out, cur)
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || cur == nil {
			return nil, fmt.Errorf(".gitmodules:%d: unexpected line %q", n, line)
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			v = v[1 : len(v)-1]
		}
		if _, dup := cur[k]; dup {
			return nil, fmt.Errorf(".gitmodules:%d: %s set twice", n, k)
		}
		cur[k] = v
	}
	return out, sc.Err()
}

// upstreamRepo is a local stand-in for the clone `rulegen upstream` makes: a
// repository with refs/remotes/origin/master and a STATUS file.
type upstreamRepo struct {
	t   *testing.T
	dir string
	env []string
}

func newUpstreamRepo(t *testing.T) *upstreamRepo {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	home := t.TempDir()
	u := &upstreamRepo{t: t, dir: t.TempDir(), env: append(os.Environ(),
		"HOME="+home, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C",
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.com",
		"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.com",
		"GIT_AUTHOR_DATE=2026-10-14T19:19:33Z", "GIT_COMMITTER_DATE=2026-10-14T19:19:33Z",
		"GIT_INDEX_FILE="+filepath.Join(home, "index"))}
	u.git(nil, "init", "-q")
	return u
}

func (u *upstreamRepo) git(stdin []byte, args ...string) string {
	u.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", u.dir}, args...)...)
	cmd.Env = u.env
	cmd.Stdin = bytes.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		u.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes a commit with exactly files, on parent ("" for a root).
func (u *upstreamRepo) commit(parent string, files map[string][]byte, msg string) string {
	u.t.Helper()
	u.git(nil, "read-tree", "--empty")
	for _, p := range slices.Sorted(maps.Keys(files)) {
		blob := u.git(files[p], "hash-object", "-w", "--stdin")
		u.git(nil, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+p)
	}
	args := []string{"commit-tree", u.git(nil, "write-tree"), "-m", msg}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	return u.git(nil, args...)
}

func (u *upstreamRepo) status(s string) {
	u.t.Helper()
	if err := os.WriteFile(filepath.Join(u.dir, statusFile), []byte(s+"\n"), 0o644); err != nil {
		u.t.Fatal(err)
	}
}

const testGitmodules = "[submodule \"tools/upstream/adblock-lists\"]\n\tpath = tools/upstream/adblock-lists\n\turl = https://github.com/brave/adblock-lists\n\tbranch = master\n"

// TestRulesUpstream runs the Upstream part of Provenance against local git
// repositories: the gate side of the sync_*, gitlink_*, gitmodules_changed
// and provenance_* rows of §11.8 (their end-to-end halves run in
// tools/rulegen through `go test`).
func TestRulesUpstream(t *testing.T) {
	u := newUpstreamRepo(t)
	real, err := realInputs()
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, p := range lockedPaths {
		files[p.upstream] = real[p.vendored]
	}
	x0 := u.commit("", files, "x0: the locked bytes")
	files["brave-lists/other.txt"] = []byte("another list\n")
	x1 := u.commit(x0, files, "x1: another list only")
	changed := append(slices.Clone(real[lockedPaths[0].vendored]), '\n')
	files[lockedPaths[0].upstream] = changed
	x2 := u.commit(x1, files, "x2: clean-urls changed")
	delete(files, "brave-lists/other.txt")
	files[lockedPaths[0].upstream] = real[lockedPaths[0].vendored]
	files["README"] = []byte("unmerged\n")
	pr := u.commit(x0, files, "pr: unmerged head, same blobs")
	u.git(nil, "update-ref", upstreamRef, x2)
	u.git(nil, "update-ref", "refs/pull/1/head", pr)
	const gone = "ffffffffffffffffffffffffffffffffffffffff"

	baseLock, err := readLock(mapFS(real, embedded))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		g, b, x       string // head gitlink, base GITLINK ("" none), lock commit
		synced        bool   // the lock holds x2's clean-urls blob
		status        string // "" = ok
		headGM        string // "" = testGitmodules
		baseNoGM      bool
		noUpstreamDir bool
		noBase        bool
		fail, log     []string
	}{
		{name: "sync_no_change", g: x1, b: x0, x: x0},
		{name: "provenance_unsynced", g: x2, b: x1, x: x0, fail: []string{fmt.Sprintf(msgNotSynced, x2)}},
		{name: "provenance_synced", g: x2, b: x1, x: x2, synced: true},
		{name: "sync_gitlink_not_ancestor", g: pr, b: x0, x: x0, fail: []string{fmt.Sprintf(msgNotOnMaster, pr)}},
		{name: "gitlink_moved_backwards", g: x0, b: x1, x: x0, fail: []string{fmt.Sprintf(msgGitlinkBackwards, x1, x0)}},
		{name: "sync_after_upstream_rewrite", g: x1, b: gone, x: x0, log: []string{fmt.Sprintf(msgHistoryRewritten, gone)}},
		{name: "base_gitlink_off_master", g: x1, b: pr, x: x0, log: []string{fmt.Sprintf(msgHistoryRewritten, pr)}},
		{name: "provenance_lock_commit_not_ancestor", g: x1, b: x0, x: pr, fail: []string{fmt.Sprintf(msgLockNotAncestor, pr, x1)}},
		{name: "lock_commit_gone", g: x1, b: x0, x: gone, fail: []string{fmt.Sprintf(msgLockNotAncestor, gone, x1)}},
		{name: "lock_commit_blobs", g: x2, b: x1, x: x1, synced: true,
			fail: []string{fmt.Sprintf(msgLockBlobs, lockedPaths[0].upstream, gitBlobSHA1(real[lockedPaths[0].vendored]), x1, gitBlobSHA1(changed))}},
		{name: "sync_fetch_fails", g: x1, b: x0, x: x0, status: "unreachable: fatal: unable to access", fail: []string{msgUnreachable}},
		{name: "gitlink_missing", g: x1, b: x0, x: x0, status: statusGitlinkMissing, fail: []string{fmt.Sprintf(msgGitlinkMissing, x1)}},
		{name: "gitlink_not_in_clone", g: gone, b: x0, x: x0, fail: []string{fmt.Sprintf(msgGitlinkMissing, gone)}},
		{name: "gitmodules_fork", g: x1, b: x0, x: x0, headGM: strings.Replace(testGitmodules, "brave/", "fork/", 1),
			fail: []string{msgGitmodulesChanged, `url "https://github.com/fork/adblock-lists"`}},
		{name: "gitmodules_branch_main", g: x1, b: x0, x: x0, headGM: strings.Replace(testGitmodules, "= master", "= main", 1),
			fail: []string{msgGitmodulesChanged, `branch "main"`}},
		{name: "gitmodules_added", g: x1, b: x0, x: x0, baseNoGM: true},
		{name: "not_a_gitlink", g: "", b: x0, x: x0, fail: []string{fmt.Sprintf(msgNotGitlink, submodulePath)}},
		{name: "provenance_unrelated_pr_upstream_down", g: x0, b: x0, x: x0, status: "unreachable: fatal: unable to access",
			log: []string{msgInputsUnchanged}},
		{name: "provenance_no_upstream_dir", g: x1, b: x0, x: x0, noUpstreamDir: true,
			log: []string{fmt.Sprintf(msgSkipNoUpstream, partUpstream)}},
		{name: "gates_without_base_dir", g: x1, x: x0, noBase: true, log: []string{fmt.Sprintf(msgSkipNoBase, partUpstream)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lock := baseLock
			lock.Upstream.Files = maps.Clone(baseLock.Upstream.Files)
			lock.Upstream.Commit = tc.x
			if tc.synced {
				lf := lock.Upstream.Files[lockedPaths[0].upstream]
				lf.GitBlobSHA1 = gitBlobSHA1(changed)
				lock.Upstream.Files[lockedPaths[0].upstream] = lf
			}
			lockJSON, err := json.Marshal(lock)
			if err != nil {
				t.Fatal(err)
			}
			head := cloneFiles(real, nil)
			head[lockPath] = lockJSON
			head[".gitmodules"] = []byte(cmp.Or(tc.headGM, testGitmodules))
			base := cloneFiles(head, inBase)
			base[".gitmodules"] = []byte(testGitmodules)
			if tc.baseNoGM {
				delete(base, ".gitmodules")
			}
			if tc.b != "" {
				base["GITLINK"] = []byte(tc.b + "\n")
			}
			u.status(cmp.Or(tc.status, statusOK))
			e := &gateEnv{data: mapFS(head, embedded), tree: mapFS(head, nil), base: mapFS(base, nil),
				headGitlink: tc.g, upstream: u.dir, now: time.Now(), key: "upstream"}
			if tc.noUpstreamDir {
				e.upstream = ""
			}
			if tc.noBase {
				e.base = nil
			}
			r := recordFunc(t, tc.name, func(r testing.TB) { checkUpstream(r, e, &lock) })
			for _, want := range tc.fail {
				if !slices.ContainsFunc(r.errs, func(s string) bool { return strings.Contains(s, want) }) {
					t.Errorf("want error %q; errors %q", want, r.errs)
				}
			}
			if len(tc.fail) == 0 && len(r.errs) > 0 {
				t.Errorf("want pass; errors %q", r.errs)
			}
			for _, want := range tc.log {
				if !slices.ContainsFunc(r.logs, func(s string) bool { return strings.Contains(s, want) }) {
					t.Errorf("want log %q; logs %q", want, r.logs)
				}
			}
		})
	}
}

func TestGitmodulesMapping(t *testing.T) {
	for _, tc := range []struct {
		in, err string
	}{
		{testGitmodules, ""},
		{strings.Replace(testGitmodules, "adblock-lists\n\tbranch", "adblock-lists.git\n\tbranch", 1), ""},
		{"# comment\n[submodule \"x\"]\n\tPath = \"tools/upstream/adblock-lists\"\n\tURL = https://github.com/brave/adblock-lists\n\tbranch = master\n", ""},
		{strings.Replace(testGitmodules, "https://github.com/brave/adblock-lists", "https://github.com/brave/adblock-lists-fork", 1), "url"},
		{strings.Replace(testGitmodules, "\tbranch = master\n", "", 1), "branch"},
		{testGitmodules + strings.Replace(testGitmodules, "\"tools", "\"other", 1), "2 submodules"},
		{strings.Replace(testGitmodules, "tools/upstream/adblock-lists\n", "tools/upstream/other\n", 1), "no submodule"},
		{testGitmodules + "\turl = https://example.com/x\n", "set twice"},
		{"[core]\n\tbare = false\n", "unexpected section"},
	} {
		err := checkGitmodulesMapping([]byte(tc.in))
		if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("%q: err %v, want %q", tc.in, err, tc.err)
		}
	}
}

func TestAcksExpiry(t *testing.T) {
	for _, tc := range []struct {
		now  string
		want bool
	}{
		{"2026-12-01T00:00:00Z", false},
		{"2026-12-01T23:59:59Z", false},
		{"2026-12-02T00:00:00Z", true},
		{"2026-12-01T20:00:00-05:00", true}, // 01:00 UTC on Dec 2
	} {
		now, _ := time.Parse(time.RFC3339, tc.now)
		if got := expired("2026-12-01", now); got != tc.want {
			t.Errorf("expired(2026-12-01, %s) = %v, want %v", tc.now, got, tc.want)
		}
	}
	if !expired("2026-02-30", time.Now()) {
		t.Error("an invalid date does not count as expired")
	}
}
