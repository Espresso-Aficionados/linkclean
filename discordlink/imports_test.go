package discordlink

import (
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// allowedImports is everything the package's non-test files may import:
// no regexp (the matchers are hand-written; regexp is for the test
// reference only), no Discord library and no network.
var allowedImports = []string{
	"context",
	"strings",
	"sync",
	"sync/atomic",
	"unicode/utf8",
	"github.com/Espresso-Aficionados/linkclean",
}

// TestImports parses every non-test .go file in the package directory,
// whatever its build constraints, and fails on an import outside
// allowedImports.
func TestImports(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	n := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		n++
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: %v", fset.Position(imp.Pos()), err)
			}
			if !slices.Contains(allowedImports, p) {
				t.Errorf("%s: imports %q, not in allowedImports", fset.Position(imp.Pos()), p)
			}
		}
	}
	if n == 0 {
		t.Fatal("no non-test .go files found")
	}
}
