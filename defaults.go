package linkclean

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strconv"
	"sync"

	"github.com/Espresso-Aficionados/linkclean/internal/brave"
	"github.com/Espresso-Aficionados/linkclean/internal/rule"
)

// rulesFS holds exactly the data that ships. TestRules validates this FS, so CI
// checks what is embedded.
//
//go:embed rules/brave/*.json rules/overlay/*.json rules.lock.json
var rulesFS embed.FS

const (
	lockPath    = "rules.lock.json"
	classesPath = "rules/overlay/debounce-classes.json"
	overlayPath = "rules/overlay/overlay.json"
	overlaySet  = "linkclean/overlay"
	braveSPDX   = "MPL-2.0"
)

// braveFiles are the vendored upstream files, keyed as in the lock, with the
// rule set each compiles to, in priority order.
var braveFiles = []struct{ set, upstream string }{
	{"brave/clean-urls", "brave-lists/clean-urls.json"},
	{"brave/query-filter", "brave-lists/query-filter.json"},
	{brave.DebounceSet, "brave-lists/debounce.json"},
}

// lockFile is rules.lock.json (schema 4, design §11.2). It is written only by
// `rulegen sync`.
type lockFile struct {
	Schema   int `json:"schema"`
	Upstream struct {
		Repo       string                `json:"repo"`
		Submodule  string                `json:"submodule"`
		Branch     string                `json:"branch"`
		Commit     string                `json:"commit"`
		CommitDate string                `json:"commit_date"`
		Files      map[string]lockedFile `json:"files"` // keyed by upstream path, e.g. "brave-lists/clean-urls.json"
	} `json:"upstream"`
}

type lockedFile struct {
	Vendored    string         `json:"vendored"` // repository path, e.g. "rules/brave/clean-urls.json"
	GitBlobSHA1 string         `json:"git_blob_sha1"`
	SHA256      string         `json:"sha256"`
	Counts      map[string]int `json:"counts,omitempty"`
	SPDX        string         `json:"spdx,omitempty"`
}

// DefaultSources returns fresh deep copies of the built-in rule sets, in
// priority order: "brave/clean-urls", "brave/query-filter", "brave/debounce",
// "linkclean/overlay". "brave/debounce" holds the Unwrap rules compiled from
// debounce.json through the overlay's classification table.
func DefaultSources() []RuleSet {
	sets, err := decodeDefaults()
	if err != nil {
		panic("linkclean: embedded rules: " + err.Error())
	}
	out := make([]RuleSet, len(sets))
	for i := range sets {
		out[i] = cloneSet(&sets[i])
	}
	return out
}

// decodeDefaults returns the built-in rule sets, decoded from rulesFS once per
// process. The result is shared: callers must not modify it.
var decodeDefaults = sync.OnceValues(func() ([]RuleSet, error) {
	return readDefaults(rulesFS)
})

// readDefaults decodes the built-in rule sets from fsys: each locked Brave
// file, after checking its sha256 against the lock, through internal/brave
// (debounce.json with the classification table), then overlay.json. Set
// metadata and notices come from the lock. All problems are returned together.
func readDefaults(fsys fs.FS) ([]RuleSet, error) {
	lock, err := readLock(fsys)
	if err != nil {
		return nil, err
	}
	if lic := lock.Upstream.Files["LICENSE"].SPDX; lic != braveSPDX {
		return nil, fmt.Errorf("%s: upstream LICENSE spdx is %q, want %q", lockPath, lic, braveSPDX)
	}
	var errs []error
	sets := make([]RuleSet, 0, len(braveFiles)+1)
	for _, f := range braveFiles {
		lf, ok := lock.Upstream.Files[f.upstream]
		if !ok {
			errs = append(errs, fmt.Errorf("%s: %s is not locked", lockPath, f.upstream))
			continue
		}
		data, err := readLocked(fsys, lf)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var rs []rule.Rule
		notice := braveNotice(&lock, f.upstream, lf)
		if f.set == brave.DebounceSet {
			var classes []byte
			if classes, err = fs.ReadFile(fsys, classesPath); err == nil {
				rs, _, err = brave.CompileDebounce(data, classes)
			}
			notice += "\n\n" + classesNotice
		} else {
			rs, err = brave.CompileStrip(f.set, data)
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s := rule.Set{
			Name:     f.set,
			License:  braveSPDX,
			Upstream: lock.Upstream.Repo + " " + f.upstream,
			Commit:   lock.Upstream.Commit,
			SHA256:   lf.SHA256,
			Notice:   notice,
			Rules:    rs,
		}
		sets = append(sets, setFromInternal(&s))
	}

	if data, err := fs.ReadFile(fsys, overlayPath); err != nil {
		errs = append(errs, err)
	} else if ov, err := decodeOverlay(data); err != nil {
		errs = append(errs, err)
	} else {
		sets = append(sets, RuleSet{Name: overlaySet, License: "MIT", Notice: overlayNotice, Rules: ov})
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return sets, nil
}

// readLock decodes rules.lock.json strictly and checks its schema version.
func readLock(fsys fs.FS) (lockFile, error) {
	var lock lockFile
	data, err := fs.ReadFile(fsys, lockPath)
	if err != nil {
		return lock, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&lock); err != nil {
		return lock, fmt.Errorf("%s: %w", lockPath, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return lock, fmt.Errorf("%s: trailing data after the JSON value", lockPath)
	}
	if lock.Schema != 4 {
		return lock, fmt.Errorf("%s: schema %d, want 4", lockPath, lock.Schema)
	}
	return lock, nil
}

// readLocked reads a vendored file and checks it against its locked sha256,
// so the SHA256 a RuleSet reports is that of the bytes it was compiled from.
func readLocked(fsys fs.FS, lf lockedFile) ([]byte, error) {
	data, err := fs.ReadFile(fsys, lf.Vendored)
	if err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != lf.SHA256 {
		return nil, fmt.Errorf("%s: sha256 %x does not match %s (%s)", lf.Vendored, sum, lockPath, lf.SHA256)
	}
	return data, nil
}

// decodeOverlay decodes overlay.json strictly (§4.7, §11.3 rules-schema): a
// JSON array of Rule objects without unknown or duplicate keys, each with a
// non-empty Ref. The rules themselves are validated by New.
func decodeOverlay(data []byte) ([]Rule, error) {
	if err := brave.CheckDuplicateKeys(data); err != nil {
		return nil, &RuleError{Set: overlaySet, Field: overlayPath, Err: err}
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, &RuleError{Set: overlaySet, Field: overlayPath, Err: err}
	}
	if raw == nil {
		return nil, &RuleError{Set: overlaySet, Field: overlayPath, Err: errors.New("want array of rules, have null")}
	}
	var errs []error
	rules := make([]Rule, 0, len(raw))
	for i, m := range raw {
		id := "#" + strconv.Itoa(i)
		var r Rule
		dec := json.NewDecoder(bytes.NewReader(m))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
			errs = append(errs, &RuleError{Set: overlaySet, RuleID: id, Err: err})
			continue
		}
		if r.ID != "" {
			id = r.ID
		}
		if r.Ref == "" {
			errs = append(errs, &RuleError{Set: overlaySet, RuleID: id, Field: "ref", Err: errors.New("missing: every overlay rule cites its evidence")})
		}
		rules = append(rules, r)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return rules, nil
}

// braveNotice is the MPL-2.0 §3.2 notice of one vendored file: what it is,
// where it came from, and where its source is.
func braveNotice(lock *lockFile, upstream string, lf lockedFile) string {
	u := &lock.Upstream
	return fmt.Sprintf("%s is %s from https://%s at commit %s (sha256 %s), unmodified.\n"+
		"It is licensed under the Mozilla Public License 2.0 (MPL-2.0): https://mozilla.org/MPL/2.0/\n"+
		"Source: https://%s/tree/%s/%s",
		lf.Vendored, upstream, u.Repo, u.Commit, lf.SHA256,
		u.Repo, u.Commit, path.Dir(upstream))
}

const classesNotice = classesPath + " classifies every debounce.json pattern.\n" +
	"It is part of the module github.com/Espresso-Aficionados/linkclean and is licensed under the\n" +
	"Mozilla Public License 2.0 (MPL-2.0): https://mozilla.org/MPL/2.0/\n" +
	"Source: https://github.com/Espresso-Aficionados/linkclean (" + classesPath + " at the module version in use)"

const overlayNotice = overlayPath + " is part of the module github.com/Espresso-Aficionados/linkclean.\n" +
	"Copyright (c) 2026 James McGuire. Licensed under the MIT License."
