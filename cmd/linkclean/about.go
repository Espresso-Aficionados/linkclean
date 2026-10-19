package main

import (
	"bufio"
	_ "embed"
	"io"
	"runtime"
	"runtime/debug"

	"github.com/Espresso-Aficionados/linkclean"
)

// modulePath is linkclean's module, whose version -version reports.
const modulePath = "github.com/Espresso-Aficionados/linkclean"

// The license texts a copy of the binary must carry besides the rule data's
// Notices: byte copies of the root LICENSE and of golang.org/x/net's LICENSE
// (the same text as golang.org/x/text's and the Go standard library's).
// TestLicenses checks both copies.
var (
	//go:embed licenses/linkclean-MIT.txt
	mitLicense string
	//go:embed licenses/go-BSD-3-Clause.txt
	bsdLicense string
)

// runAbout runs -licenses or -version through write.
func runAbout(e env, write func(io.Writer, *linkclean.Cleaner)) int {
	keep, err := linkclean.New(linkclean.Config{})
	if err != nil {
		return errorf(e, "%v", err)
	}
	w := bufio.NewWriter(e.Stdout)
	write(w, keep)
	if err := w.Flush(); err != nil {
		return errorf(e, "writing output: %v", err)
	}
	return exitOK
}

// writeLicenses writes the version line, linkclean's MIT license, the rule
// data's notices and the BSD-3-Clause notice of golang.org/x/net,
// golang.org/x/text and the Go standard library. Write errors surface from
// the caller's Flush.
func writeLicenses(w io.Writer, keep *linkclean.Cleaner) {
	io.WriteString(w, versionLine()+"\n\n"+
		"linkclean (github.com/Espresso-Aficionados/linkclean), MIT License:\n\n"+mitLicense+
		"\nEmbedded rule data:\n\n"+keep.Notices()+"\n"+
		"\ngolang.org/x/net, golang.org/x/text and the Go standard library, BSD-3-Clause:\n\n"+bsdLicense)
}

// writeVersion writes the version line and one line per rule set:
// "<name> <license>[ <commit> <sha256>]".
func writeVersion(w io.Writer, keep *linkclean.Cleaner) {
	io.WriteString(w, versionLine()+"\n")
	for _, s := range keep.Sources() {
		line := s.Name + " " + s.License
		if s.Commit != "" {
			line += " " + s.Commit + " " + s.SHA256
		}
		io.WriteString(w, line+"\n")
	}
}

// versionLine is "linkclean <version> <goversion>[ replaced]", without a
// newline.
func versionLine() string {
	bi, ok := debug.ReadBuildInfo()
	v, replaced := moduleVersion(bi, ok)
	gov := runtime.Version()
	if ok && bi != nil && bi.GoVersion != "" {
		gov = bi.GoVersion
	}
	line := "linkclean " + v + " " + gov
	if replaced {
		line += " replaced"
	}
	return line
}

// moduleVersion returns the version of the linkclean module in bi: the main
// module when linkclean itself is built, else the dependency (go tool
// linkclean). A replaced module reports the replacement's version and
// replaced, never the replacement's path, which may be a local path. A
// "(devel)" main module gets its VCS revision ("(devel)-<12 hex>[-dirty]")
// when the build recorded one; a "(devel)" dependency does not, since the
// vcs settings describe the main module. Without build info, or without
// linkclean in it, the version is "(unknown)".
func moduleVersion(bi *debug.BuildInfo, ok bool) (version string, replaced bool) {
	if !ok || bi == nil {
		return "(unknown)", false
	}
	m, isMain := &bi.Main, true
	if m.Path != modulePath {
		m, isMain = nil, false
		for _, d := range bi.Deps {
			if d != nil && d.Path == modulePath {
				m = d
				break
			}
		}
		if m == nil {
			return "(unknown)", false
		}
	}
	version = m.Version
	if m.Replace != nil {
		version, replaced = m.Replace.Version, true
	}
	if version != "" && version != "(devel)" {
		return version, replaced
	}
	version = "(devel)"
	if !isMain {
		return version, replaced
	}
	var rev string
	var dirty bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return version, replaced
	}
	version += "-" + rev[:min(12, len(rev))]
	if dirty {
		version += "-dirty"
	}
	return version, replaced
}
