package data

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Lock is rules.lock.json, schema 4. It is written only by rulegen sync and
// holds provenance only. The JSON tags allow a lenient json.Unmarshal; use
// ParseLock for the strict reading.
type Lock struct {
	Schema   int          `json:"schema"`
	Upstream LockUpstream `json:"upstream"`
}

type LockUpstream struct {
	Repo       string              `json:"repo"`
	Submodule  string              `json:"submodule"`
	Branch     string              `json:"branch"`
	Commit     string              `json:"commit"`      // the commit the vendored bytes were last synced from
	CommitDate string              `json:"commit_date"` // its committer date, RFC 3339
	Files      map[string]LockFile `json:"files"`       // keyed by upstream path
}

type LockFile struct {
	Vendored    string `json:"vendored"`
	GitBlobSHA1 string `json:"git_blob_sha1"`
	SHA256      string `json:"sha256"`
	SPDX        string `json:"spdx,omitempty"`  // LICENSE only
	Counts      Counts `json:"counts,omitzero"` // JSON files only
}

// Counts are the lock's per-file counts. Rules is the number of array
// elements; Params the sum of len(params); GlobalParams the same sum over
// the elements with an include whose host is "*" (not a unique count);
// Includes the sum of len(include). Strip files carry Rules, Params and
// GlobalParams; debounce.json carries Rules and Includes.
type Counts struct {
	Rules        int `json:"rules"`
	Params       int `json:"params,omitempty"`
	GlobalParams int `json:"global_params,omitempty"`
	Includes     int `json:"includes,omitempty"`
}

// lockJSON mirrors Lock for the strict decode, with presence tracked so
// that a field another kind owns is an error rather than silently zero.
type lockJSON struct {
	Schema   *int `json:"schema"`
	Upstream *struct {
		Repo       string                  `json:"repo"`
		Submodule  string                  `json:"submodule"`
		Branch     string                  `json:"branch"`
		Commit     string                  `json:"commit"`
		CommitDate string                  `json:"commit_date"`
		Files      map[string]lockFileJSON `json:"files"`
	} `json:"upstream"`
}

type lockFileJSON struct {
	Vendored    string  `json:"vendored"`
	GitBlobSHA1 string  `json:"git_blob_sha1"`
	SHA256      string  `json:"sha256"`
	SPDX        *string `json:"spdx"`
	Counts      *struct {
		Rules        *int `json:"rules"`
		Params       *int `json:"params"`
		GlobalParams *int `json:"global_params"`
		Includes     *int `json:"includes"`
	} `json:"counts"`
}

// ParseLock decodes a lock strictly: no unknown or duplicate keys, no
// trailing data, schema 4, the fixed upstream coordinates, exactly the four
// vendored files at their paths, well-formed ids and dates, and the count
// fields of each file's kind. All problems are returned together.
func ParseLock(b []byte) (Lock, error) {
	if err := checkDuplicateKeys(b); err != nil {
		return Lock{}, fmt.Errorf("%s: %w", LockPath, err)
	}
	var j lockJSON
	if err := decodeStrict(b, &j); err != nil {
		return Lock{}, fmt.Errorf("%s: %w", LockPath, err)
	}
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: "+format, append([]any{LockPath}, args...)...))
	}
	var l Lock
	switch {
	case j.Schema == nil:
		bad("missing schema")
	case *j.Schema != 4:
		bad("schema %d, want 4", *j.Schema)
	default:
		l.Schema = 4
	}
	if j.Upstream == nil {
		bad("missing upstream")
		return l, errors.Join(errs...)
	}
	u := j.Upstream
	l.Upstream = LockUpstream{Repo: u.Repo, Submodule: u.Submodule, Branch: u.Branch, Commit: u.Commit, CommitDate: u.CommitDate, Files: map[string]LockFile{}}
	for _, f := range []struct{ name, got, want string }{
		{"repo", u.Repo, RepoPath}, {"submodule", u.Submodule, Submodule}, {"branch", u.Branch, Branch},
	} {
		if f.got != f.want {
			bad("upstream.%s %q, want %q", f.name, f.got, f.want)
		}
	}
	if !isLowerHex(u.Commit, 40) {
		bad("upstream.commit %q is not 40 lowercase hex digits", u.Commit)
	}
	if _, err := time.Parse(time.RFC3339, u.CommitDate); err != nil {
		bad("upstream.commit_date %q is not RFC 3339", u.CommitDate)
	}
	for _, k := range sortedKeys(u.Files) {
		if _, ok := SpecOf(k); !ok {
			bad("upstream.files: unexpected file %q", k)
		}
	}
	for _, s := range Files {
		f, ok := u.Files[s.Upstream]
		if !ok {
			bad("upstream.files: missing %q", s.Upstream)
			continue
		}
		at := "upstream.files[" + strconv.Quote(s.Upstream) + "]"
		lf := LockFile{Vendored: f.Vendored, GitBlobSHA1: f.GitBlobSHA1, SHA256: f.SHA256}
		if f.Vendored != s.Vendored {
			bad("%s.vendored %q, want %q", at, f.Vendored, s.Vendored)
		}
		if !isLowerHex(f.GitBlobSHA1, 40) {
			bad("%s.git_blob_sha1 %q is not 40 lowercase hex digits", at, f.GitBlobSHA1)
		}
		if !isLowerHex(f.SHA256, 64) {
			bad("%s.sha256 %q is not 64 lowercase hex digits", at, f.SHA256)
		}
		if s.Kind == KindLicense {
			if f.Counts != nil {
				bad("%s: counts on LICENSE", at)
			}
			if f.SPDX == nil || *f.SPDX == "" {
				bad("%s: missing spdx", at)
			} else {
				lf.SPDX = *f.SPDX
			}
			l.Upstream.Files[s.Upstream] = lf
			continue
		}
		if f.SPDX != nil {
			bad("%s: spdx on a data file", at)
		}
		if f.Counts == nil {
			bad("%s: missing counts", at)
			l.Upstream.Files[s.Upstream] = lf
			continue
		}
		c := f.Counts
		type field struct {
			name string
			v    *int
			dst  *int
			want bool
		}
		strip := s.Kind == KindStrip
		for _, fl := range []field{
			{"rules", c.Rules, &lf.Counts.Rules, true},
			{"params", c.Params, &lf.Counts.Params, strip},
			{"global_params", c.GlobalParams, &lf.Counts.GlobalParams, strip},
			{"includes", c.Includes, &lf.Counts.Includes, !strip},
		} {
			switch {
			case fl.want && fl.v == nil:
				bad("%s.counts: missing %s", at, fl.name)
			case !fl.want && fl.v != nil:
				bad("%s.counts: unexpected %s", at, fl.name)
			case fl.v != nil && *fl.v < 0:
				bad("%s.counts.%s is negative", at, fl.name)
			case fl.v != nil:
				*fl.dst = *fl.v
			}
		}
		l.Upstream.Files[s.Upstream] = lf
	}
	return l, errors.Join(errs...)
}

// Encode renders the lock in the committed layout: two-space indent, files
// in lock order, counts on one line, a trailing newline. Files outside the
// vendored set (which ParseLock rejects) follow in key order.
func (l *Lock) Encode() []byte {
	var b bytes.Buffer
	b.WriteString("{\n")
	fmt.Fprintf(&b, "  \"schema\": %d,\n", l.Schema)
	b.WriteString("  \"upstream\": {\n")
	u := &l.Upstream
	fmt.Fprintf(&b, "    \"repo\": %s,\n", jsonString(u.Repo))
	fmt.Fprintf(&b, "    \"submodule\": %s,\n", jsonString(u.Submodule))
	fmt.Fprintf(&b, "    \"branch\": %s,\n", jsonString(u.Branch))
	fmt.Fprintf(&b, "    \"commit\": %s,\n", jsonString(u.Commit))
	fmt.Fprintf(&b, "    \"commit_date\": %s,\n", jsonString(u.CommitDate))
	b.WriteString("    \"files\": {\n")
	var keys []string
	for _, s := range Files {
		if _, ok := u.Files[s.Upstream]; ok {
			keys = append(keys, s.Upstream)
		}
	}
	for _, k := range sortedKeys(u.Files) {
		if _, ok := SpecOf(k); !ok {
			keys = append(keys, k)
		}
	}
	for i, k := range keys {
		f := u.Files[k]
		kind := KindStrip
		if s, ok := SpecOf(k); ok {
			kind = s.Kind
		}
		fmt.Fprintf(&b, "      %s: {\n", jsonString(k))
		fmt.Fprintf(&b, "        \"vendored\": %s,\n", jsonString(f.Vendored))
		fmt.Fprintf(&b, "        \"git_blob_sha1\": %s,\n", jsonString(f.GitBlobSHA1))
		fmt.Fprintf(&b, "        \"sha256\": %s,\n", jsonString(f.SHA256))
		switch kind {
		case KindLicense:
			fmt.Fprintf(&b, "        \"spdx\": %s\n", jsonString(f.SPDX))
		case KindDebounce:
			fmt.Fprintf(&b, "        \"counts\": {\"rules\": %d, \"includes\": %d}\n", f.Counts.Rules, f.Counts.Includes)
		default:
			fmt.Fprintf(&b, "        \"counts\": {\"rules\": %d, \"params\": %d, \"global_params\": %d}\n", f.Counts.Rules, f.Counts.Params, f.Counts.GlobalParams)
		}
		b.WriteString("      }")
		if i < len(keys)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("    }\n  }\n}\n")
	return b.Bytes()
}

// Notice renders rules/brave/NOTICE for the lock: the source repository,
// commit and date, and the four locked blobs in lock order.
func Notice(l *Lock) []byte {
	var b strings.Builder
	b.WriteString("The files in this directory are copied verbatim from Brave's adblock-lists\n" +
		"repository. They are licensed under the Mozilla Public License 2.0 (MPL-2.0);\n" +
		"the upstream license text at the commit below is in LICENSE.\n\n")
	fmt.Fprintf(&b, "Repository: %s\n", CanonicalURL)
	fmt.Fprintf(&b, "Commit:     %s\n", l.Upstream.Commit)
	fmt.Fprintf(&b, "Date:       %s\n", l.Upstream.CommitDate)
	fmt.Fprintf(&b, "Source:     %s/tree/%s/brave-lists\n\n", CanonicalURL, l.Upstream.Commit)
	b.WriteString("Files (upstream path, git blob sha1, sha256):\n")
	for _, s := range Files {
		if f, ok := l.Upstream.Files[s.Upstream]; ok {
			fmt.Fprintf(&b, "%s %s %s\n", s.Upstream, f.GitBlobSHA1, f.SHA256)
		}
	}
	b.WriteString("\nIts hashes must equal those in " + LockPath + "; TestRules/Provenance checks them.\n")
	return []byte(b.String())
}

// CountsOf counts a vendored file leniently at the raw-JSON level, so a
// schema-broken file still counts: a top level that is not an array counts
// zero, an element that is not an object counts as a rule with nothing in
// it, and a non-array include or params counts zero. KindLicense is always
// zero.
func CountsOf(k Kind, b []byte) Counts {
	var c Counts
	if k == KindLicense {
		return c
	}
	var elems []json.RawMessage
	if json.Unmarshal(b, &elems) != nil {
		return c
	}
	c.Rules = len(elems)
	for _, e := range elems {
		var obj map[string]json.RawMessage
		if json.Unmarshal(e, &obj) != nil {
			continue
		}
		var include, params []json.RawMessage
		_ = json.Unmarshal(obj["include"], &include)
		_ = json.Unmarshal(obj["params"], &params)
		if k == KindDebounce {
			c.Includes += len(include)
			continue
		}
		c.Params += len(params)
		for _, raw := range include {
			var p string
			if json.Unmarshal(raw, &p) == nil && PatternHost(p) == "*" {
				c.GlobalParams += len(params)
				break
			}
		}
	}
	return c
}

// PatternHost returns the host part of a match pattern
// ("<scheme>://<host><path>"), or "" when p has no "://".
func PatternHost(p string) string {
	_, rest, ok := strings.Cut(p, "://")
	if !ok {
		return ""
	}
	host, _, _ := strings.Cut(rest, "/")
	return host
}

func jsonString(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

// decodeStrict decodes one JSON value into v with unknown fields rejected
// and nothing after it.
func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return truncated(err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}

// checkDuplicateKeys reports the first object key that appears twice in one
// object, which encoding/json would silently resolve to the last value.
func checkDuplicateKeys(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'):
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				k := kt.(string)
				if seen[k] {
					return fmt.Errorf("duplicate key %q at byte %d", k, dec.InputOffset())
				}
				seen[k] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		case json.Delim('['):
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		}
		return nil
	}
	return truncated(walk())
}

// truncated reports truncated input the same way on every Go version:
// encoding/json returns io.EOF or io.ErrUnexpectedEOF depending on the
// version and on where the input ends.
func truncated(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errors.New("unexpected end of JSON input")
	}
	return err
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
