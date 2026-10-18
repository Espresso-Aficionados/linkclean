package unshorten

import (
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/unshorten/internal/dialhook"
)

// allowedImports is, per package directory, everything its non-test files
// may import. Every package directory under unshorten must have a row.
var allowedImports = map[string][]string{
	".": {
		"bytes", "container/list", "context", "crypto/tls", "encoding/json", "errors", "fmt", "html", "io",
		"log/slog", "math", "net", "net/http", "net/http/httptrace", "net/netip", "net/url", "reflect", "regexp",
		"runtime", "runtime/debug", "slices", "strconv", "strings", "sync", "sync/atomic", "syscall", "time",
		"unicode/utf8", "weak",
		"code.dny.dev/ssrf",
		"github.com/Espresso-Aficionados/linkclean",
		"github.com/Espresso-Aficionados/linkclean/internal/matchpattern",
		"github.com/Espresso-Aficionados/linkclean/internal/rawurl",
		"github.com/Espresso-Aficionados/linkclean/unshorten/internal/dialhook",
		"github.com/Espresso-Aficionados/linkclean/unshorten/internal/flight",
	},
	"internal/flight":   {"fmt", "runtime/debug", "sync"},
	"internal/dialhook": {"errors", "net/netip", "sync/atomic"},
}

// TestImports parses every non-test .go file under the package directory,
// whatever its build constraints, and fails on an import outside its
// directory's allowedImports row, and on a package directory without a row.
func TestImports(t *testing.T) {
	fset := token.NewFileSet()
	files := map[string]int{}
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); p != "." && (n == "testdata" || strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		dir := filepath.ToSlash(filepath.Dir(p))
		allowed, ok := allowedImports[dir]
		if !ok {
			t.Errorf("%s: package directory %s has no allowedImports row", p, dir)
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		files[dir]++
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: %v", fset.Position(imp.Pos()), err)
			}
			if !slices.Contains(allowed, path) {
				t.Errorf("%s: imports %q, not in allowedImports[%q]", fset.Position(imp.Pos()), path, dir)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for dir := range allowedImports {
		if files[dir] == 0 {
			t.Errorf("allowedImports[%q]: no non-test .go files found", dir)
		}
	}
}

// TestNetworkSealed checks that TestMain's guards are in place: requests
// through http.DefaultTransport fail and the fail-closed Resolve seam is
// installed (installNet replaces it only for its test's duration).
func TestNetworkSealed(t *testing.T) {
	if _, ok := http.DefaultTransport.(failingTransport); !ok {
		t.Errorf("http.DefaultTransport is %T, want failingTransport", http.DefaultTransport)
	}
	if dialhook.Resolve() == nil {
		t.Error("no Resolve seam installed")
	}
	if _, ok := origDefaultTransport.(*http.Transport); !ok {
		t.Errorf("origDefaultTransport is %T", origDefaultTransport)
	}
}
