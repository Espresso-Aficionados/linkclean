package data

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestSharedMessages checks that the root module's provenance gate spells
// every Shared format exactly as this package does: both sides print them,
// and the routine matches on them. It is skipped until the root gate that
// carries the copy exists.
func TestSharedMessages(t *testing.T) {
	if _, err := os.Stat(filepath.Join(root, "rules_provenance_test.go")); err != nil {
		t.Skipf("root provenance gate not present: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(root, "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	lits := map[string]bool{}
	fset := token.NewFileSet()
	for _, f := range files {
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					lits[s] = true
				}
			}
			return true
		})
	}
	for _, m := range Shared {
		if !lits[m] {
			t.Errorf("no string literal %q in the root *_test.go files", m)
		}
	}
}

func TestMessagesDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range Shared {
		if m == "" || seen[m] {
			t.Errorf("Shared: empty or repeated %q", m)
		}
		seen[m] = true
		if strings.Count(m, "%") != strings.Count(m, "%s")+strings.Count(m, "%d") {
			t.Errorf("Shared: %q uses a verb other than %%s or %%d", m)
		}
	}
}
