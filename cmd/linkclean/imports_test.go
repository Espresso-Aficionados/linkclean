package main

import (
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// allowedImports is everything the command's non-test files may import:
// the standard library pieces it needs and the root package. Nothing from
// internal/, unshorten (the network) or discordlink.
var allowedImports = []string{
	"bufio",
	"bytes",
	"embed",
	"encoding/json",
	"flag",
	"fmt",
	"io",
	"os",
	"runtime",
	"runtime/debug",
	"strconv",
	"strings",
	"unicode/utf16",
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
