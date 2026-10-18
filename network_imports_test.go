package linkclean

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// networkImport reports whether a package that promises no I/O must not
// import p: net, net/http and every other net/* package except net/netip and
// net/url, os/exec, syscall, golang.org/x/net other than idna and
// publicsuffix, and code.dny.dev/ssrf.
func networkImport(p string) bool {
	switch p {
	case "net/netip", "net/url", "golang.org/x/net/idna", "golang.org/x/net/publicsuffix":
		return false
	case "net", "os/exec", "syscall", "code.dny.dev/ssrf":
		return true
	}
	return strings.HasPrefix(p, "net/") || strings.HasPrefix(p, "golang.org/x/net/") ||
		strings.HasPrefix(p, "code.dny.dev/ssrf/")
}

// TestNoNetworkImports backs the package doc's "no function in this package
// performs I/O": no non-test file of the root module, whatever its build
// constraints, imports a network package (networkImport), except under
// unshorten/, the opt-in network package. Directories with their own go.mod
// (the tools module), testdata and directories go ignores are skipped.
func TestNoNetworkImports(t *testing.T) {
	fset := token.NewFileSet()
	dirs := map[string]bool{}
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == "." {
				return nil
			}
			if n := d.Name(); n == "testdata" || strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_") || p == "unshorten" {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		dirs[filepath.ToSlash(filepath.Dir(p))] = true
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: %v", fset.Position(imp.Pos()), err)
			}
			if networkImport(path) {
				t.Errorf("%s: imports %q; only package unshorten may use the network", fset.Position(imp.Pos()), path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{".", "discordlink", "internal/rawurl", "internal/matchpattern", "internal/engine"} {
		if !dirs[want] {
			t.Errorf("package directory %s not checked", want)
		}
	}
	if dirs["unshorten"] || dirs["tools/rulegen"] {
		t.Error("unshorten or the tools module was checked")
	}
}

func TestNetworkImportList(t *testing.T) {
	for _, p := range []string{"net", "net/http", "net/http/httptrace", "net/rpc", "net/smtp", "os/exec", "syscall",
		"golang.org/x/net/http2", "golang.org/x/net/proxy", "code.dny.dev/ssrf"} {
		if !networkImport(p) {
			t.Errorf("networkImport(%q) = false", p)
		}
	}
	for _, p := range []string{"net/netip", "net/url", "golang.org/x/net/idna", "golang.org/x/net/publicsuffix", "os", "netx", "strings"} {
		if networkImport(p) {
			t.Errorf("networkImport(%q) = true", p)
		}
	}
}
