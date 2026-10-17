// Package data holds the rule-data contract shared by every rulegen
// command: the upstream coordinates, the vendored file set, the lock and
// NOTICE formats (reproduced byte for byte), rules/acks.json, .gitmodules
// parsing and the message strings the gates and the routine match on.
package data

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
)

const (
	RepoPath     = "github.com/brave/adblock-lists"
	CanonicalURL = "https://github.com/brave/adblock-lists"
	Submodule    = "tools/upstream/adblock-lists" // gitlink path in this repository
	Branch       = "master"

	LockPath    = "rules.lock.json"
	NoticePath  = "rules/brave/NOTICE"
	AcksPath    = "rules/acks.json"
	ClassesPath = "rules/overlay/debounce-classes.json"
	OverlayPath = "rules/overlay/overlay.json"
	WatchPath   = "tools/rulegen/watch.json"

	GitmodulesPath = ".gitmodules"
	GitlinkFile    = "GITLINK" // basedir output: base's gitlink id

	TrailerSync  = "Rulegen-Sync"
	TrailerMerge = "Rulegen-Merge"

	// The upstream clone (rulegen upstream): a non-bare --no-checkout clone
	// whose origin is CloneURL, with MasterRef, and a one-line StatusFile.
	MasterRef               = "refs/remotes/origin/master"
	StatusFile              = "STATUS"
	StatusOK                = "ok"
	StatusGitlinkMissing    = "gitlink-missing"
	StatusUnreachablePrefix = "unreachable: "

	SummaryMarker = "<!-- rulegen:summary -->"
)

// CloneURL is the URL rulegen clones upstream from. It is a variable only so
// tests can point it at a file:// fixture; .gitmodules is always checked
// against CanonicalURL.
var CloneURL = CanonicalURL

// Kind is how a vendored file is counted.
type Kind int

const (
	KindStrip    Kind = iota // clean-urls.json, query-filter.json: [{include, exclude, params}]
	KindDebounce             // debounce.json: [{include, exclude, action, param, …}]
	KindLicense              // LICENSE: no counts, an SPDX id
)

// Spec is one vendored file: its upstream path (the lock's key) and its
// repository path.
type Spec struct {
	Upstream, Vendored string
	Kind               Kind
}

// Files is the vendored set, in lock order. All four come from one upstream
// commit.
var Files = [4]Spec{
	{"brave-lists/clean-urls.json", "rules/brave/clean-urls.json", KindStrip},
	{"brave-lists/query-filter.json", "rules/brave/query-filter.json", KindStrip},
	{"brave-lists/debounce.json", "rules/brave/debounce.json", KindDebounce},
	{"LICENSE", "rules/brave/LICENSE", KindLicense},
}

// SpecOf returns the Spec whose upstream path is up.
func SpecOf(up string) (Spec, bool) {
	for _, s := range Files {
		if s.Upstream == up {
			return s, true
		}
	}
	return Spec{}, false
}

// InWriteSet reports whether p (slash-separated, relative to the repository
// root) is in the sync write set: rules.lock.json or a file directly in
// rules/brave/.
func InWriteSet(p string) bool {
	if p == LockPath {
		return true
	}
	name, ok := strings.CutPrefix(p, "rules/brave/")
	return ok && name != "" && name != "." && name != ".." && !strings.Contains(name, "/") && path.Clean(p) == p
}

// SHA256Hex is the lowercase hex sha256 of b.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SPDXOf identifies the upstream LICENSE text: "MPL-2.0" when its first line
// is the MPL 2.0 title, "NOASSERTION" otherwise.
func SPDXOf(license []byte) string {
	first, _, _ := strings.Cut(string(license), "\n")
	if strings.TrimRight(first, " \t\r") == "Mozilla Public License Version 2.0" {
		return "MPL-2.0"
	}
	return "NOASSERTION"
}

// Canon returns canon(name): "%XX" decoded once (invalid escapes stay
// literal, '+' is untouched), then ASCII-lowercased. It is a copy of the
// root module's internal/query.Canon, which tools cannot import; a parity
// test runs that package's own table against it.
func Canon(name string) string {
	var b []byte
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '%' && i+2 < len(name) && isHex(name[i+1]) && isHex(name[i+2]) {
			c = unhex(name[i+1])<<4 | unhex(name[i+2])
			i += 2
		}
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		b = append(b, c)
	}
	return string(b)
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c >= 'a':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

// isLowerHex reports whether s is exactly n lowercase hex digits.
func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}
