package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/Espresso-Aficionados/linkclean"
)

const ctModule = "github.com/Espresso-Aficionados/linkclean"

// ctGo runs the go command with no module downloads (GOPROXY=off), so the
// test never uses the network; the module cache is populated because this
// test binary was built from it.
func ctGo(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly", "GOWORK=off")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

func ctRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestLicenses: the embedded copies equal their sources, and -licenses prints
// the version line, linkclean's MIT text, Notices() and the Go BSD text.
func TestLicenses(t *testing.T) {
	mit := ctRead(t, "licenses/linkclean-MIT.txt")
	bsd := ctRead(t, "licenses/go-BSD-3-Clause.txt")
	if !bytes.Equal(mit, ctRead(t, "../../LICENSE")) {
		t.Error("licenses/linkclean-MIT.txt differs from the root LICENSE")
	}
	for _, mod := range []string{"golang.org/x/net", "golang.org/x/text"} {
		dir := ctGo(t, "list", "-m", "-f", "{{.Dir}}", mod)
		if dir == "" {
			t.Fatalf("go list -m %s: no directory; the module cache lacks it", mod)
		}
		if !bytes.Equal(bsd, ctRead(t, filepath.Join(dir, "LICENSE"))) {
			t.Errorf("licenses/go-BSD-3-Clause.txt differs from %s's LICENSE", mod)
		}
	}
	// Some distributions (Homebrew) ship a GOROOT without LICENSE.
	if goroot := ctGo(t, "env", "GOROOT"); goroot != "" {
		if b, err := os.ReadFile(filepath.Join(goroot, "LICENSE")); err == nil {
			if !bytes.Equal(bsd, b) {
				t.Error("licenses/go-BSD-3-Clause.txt differs from GOROOT/LICENSE")
			}
		} else {
			t.Logf("skipping the GOROOT comparison: %v", err)
		}
	}

	ver := ctRun(nil, "-version")
	versionLine, _, ok := strings.Cut(ver.stdout, "\n")
	if ver.code != 0 || !ok {
		t.Fatalf("-version: code %d, stdout %q", ver.code, ver.stdout)
	}
	want := versionLine + "\n\n" +
		"linkclean (github.com/Espresso-Aficionados/linkclean), MIT License:\n\n" + string(mit) +
		"\nEmbedded rule data:\n\n" + linkclean.Default().Notices() + "\n" +
		"\ngolang.org/x/net, golang.org/x/text and the Go standard library, BSD-3-Clause:\n\n" + string(bsd)
	got := ctRun(nil, "-licenses")
	ctCheck(t, got, want, "", 0)
	if !strings.Contains(got.stdout, "MPL-2.0") {
		t.Error("-licenses lacks the MPL-2.0 notice")
	}
	if strings.Contains(strings.ToLower(got.stdout), "ssrf") {
		t.Error("-licenses names ssrf, which the command cannot include")
	}
}

// TestModuleVersion: moduleVersion resolves linkclean's own module, as
// the main module or as a dependency, and never reveals a replacement path.
func TestModuleVersion(t *testing.T) {
	const rev = "0123456789abcdef0123456789abcdef01234567"
	vcs := func(modified string) []debug.BuildSetting {
		s := []debug.BuildSetting{{Key: "vcs", Value: "git"}, {Key: "vcs.revision", Value: rev}, {Key: "vcs.time", Value: "2026-10-19T00:00:00Z"}}
		if modified != "" {
			s = append(s, debug.BuildSetting{Key: "vcs.modified", Value: modified})
		}
		return s
	}
	app := debug.Module{Path: "example.com/app", Version: "(devel)"}
	dep := func(m debug.Module) []*debug.Module {
		return []*debug.Module{{Path: "golang.org/x/net", Version: "v0.61.0"}, &m, {Path: "golang.org/x/text", Version: "v0.43.0"}}
	}
	rows := []struct {
		name     string
		bi       *debug.BuildInfo
		ok       bool
		version  string
		replaced bool
	}{
		{name: "main, tagged", ok: true, version: "v0.3.0",
			bi: &debug.BuildInfo{Main: debug.Module{Path: ctModule, Version: "v0.3.0"}}},
		{name: "main, pseudo-version", ok: true, version: "v0.3.1-0.20261019000000-0123456789ab",
			bi: &debug.BuildInfo{Main: debug.Module{Path: ctModule, Version: "v0.3.1-0.20261019000000-0123456789ab"}}},
		{name: "main, devel, dirty", ok: true, version: "(devel)-0123456789ab-dirty",
			bi: &debug.BuildInfo{Main: debug.Module{Path: ctModule, Version: "(devel)"}, Settings: vcs("true")}},
		{name: "main, devel, clean", ok: true, version: "(devel)-0123456789ab",
			bi: &debug.BuildInfo{Main: debug.Module{Path: ctModule, Version: "(devel)"}, Settings: vcs("false")}},
		{name: "main, devel, no vcs.modified", ok: true, version: "(devel)-0123456789ab",
			bi: &debug.BuildInfo{Main: debug.Module{Path: ctModule, Version: "(devel)"}, Settings: vcs("")}},
		{name: "main, empty version", ok: true, version: "(devel)-0123456789ab",
			bi: &debug.BuildInfo{Main: debug.Module{Path: ctModule}, Settings: vcs("false")}},
		{name: "main, devel, no vcs (go run)", ok: true, version: "(devel)",
			bi: &debug.BuildInfo{Main: debug.Module{Path: ctModule, Version: "(devel)"}}},
		{name: "dependency", ok: true, version: "v0.4.0",
			bi: &debug.BuildInfo{Main: app, Deps: dep(debug.Module{Path: ctModule, Version: "v0.4.0"}), Settings: vcs("true")}},
		{name: "dependency, replaced with a version", ok: true, version: "v0.4.1", replaced: true,
			bi: &debug.BuildInfo{Main: app, Deps: dep(debug.Module{Path: ctModule, Version: "v0.4.0",
				Replace: &debug.Module{Path: "example.com/fork", Version: "v0.4.1"}})}},
		{name: "dependency, replaced by a directory", ok: true, version: "(devel)", replaced: true,
			bi: &debug.BuildInfo{Main: app, Deps: dep(debug.Module{Path: ctModule, Version: "v0.4.0",
				Replace: &debug.Module{Path: "../fork"}}), Settings: vcs("true")}},
		{name: "devel dependency ignores the main module's vcs", ok: true, version: "(devel)",
			bi: &debug.BuildInfo{Main: app, Deps: dep(debug.Module{Path: ctModule, Version: "(devel)"}), Settings: vcs("true")}},
		{name: "a prefix of the path is not the module", ok: true, version: "(unknown)",
			bi: &debug.BuildInfo{Main: debug.Module{Path: ctModule + "x", Version: "v1.0.0"},
				Deps: dep(debug.Module{Path: ctModule + "/unshortenx", Version: "v1.0.0"})}},
		{name: "absent", ok: true, version: "(unknown)",
			bi: &debug.BuildInfo{Main: app, Deps: dep(debug.Module{Path: "example.com/other", Version: "v1.0.0"})}},
		{name: "no build info", ok: false, version: "(unknown)"},
		{name: "nil build info", ok: true, version: "(unknown)"},
		{name: "not ok", ok: false, version: "(unknown)",
			bi: &debug.BuildInfo{Main: debug.Module{Path: ctModule, Version: "v0.3.0"}}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			v, rep := moduleVersion(r.bi, r.ok)
			if v != r.version || rep != r.replaced {
				t.Errorf("moduleVersion = (%q, %v), want (%q, %v)", v, rep, r.version, r.replaced)
			}
			if strings.Contains(v, "fork") {
				t.Errorf("version %q reveals the replacement path", v)
			}
		})
	}
}

// TestVersion: -version prints the version line, then one line per
// rule source.
func TestVersion(t *testing.T) {
	got := ctRun(nil, "-version")
	if got.code != 0 || got.stderr != "" || !strings.HasSuffix(got.stdout, "\n") {
		t.Fatalf("code %d, stderr %q, stdout %q", got.code, got.stderr, got.stdout)
	}
	lines := strings.Split(strings.TrimSuffix(got.stdout, "\n"), "\n")
	if !regexp.MustCompile(`^linkclean \S+ go\S+( replaced)?$`).MatchString(lines[0]) {
		t.Errorf("first line %q does not match the version format", lines[0])
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		v, rep := moduleVersion(bi, ok)
		want := "linkclean " + v + " " + bi.GoVersion
		if rep {
			want += " replaced"
		}
		if lines[0] != want {
			t.Errorf("first line %q, want %q", lines[0], want)
		}
	}
	var want []string
	for _, s := range linkclean.Default().Sources() {
		l := s.Name + " " + s.License
		if s.Commit != "" {
			l += " " + s.Commit + " " + s.SHA256
		}
		want = append(want, l)
	}
	if len(want) == 0 {
		t.Fatal("Default().Sources() is empty")
	}
	if strings.Join(lines[1:], "\n") != strings.Join(want, "\n") {
		t.Errorf("source lines =\n%s\nwant\n%s", strings.Join(lines[1:], "\n"), strings.Join(want, "\n"))
	}
}
