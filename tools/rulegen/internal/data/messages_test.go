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

// rootCopies maps each constant of the root gate's copy block
// (rules_provenance_test.go) to the value it must equal here. The Shared
// formats are printed by both modules; the rest is protocol between them
// (the STATUS file of the upstream clone, the paths and the upstream ref).
var rootCopies = map[string]string{
	"msgHeld":                 MsgHeld,
	"msgGitlinkBackwards":     MsgBackwards,
	"msgNotSynced":            MsgUnsynced,
	"msgUnreachable":          MsgUnreachable,
	"msgInputsUnchanged":      MsgInputsUnchanged,
	"msgGitlinkMissing":       MsgGitlinkMissing,
	"msgNotOnMaster":          MsgNotOnMaster,
	"msgHistoryRewritten":     MsgHistoryRewritten,
	"msgGitmodulesMapping":    MsgGitmodulesMapping,
	"msgGitmodulesChanged":    MsgGitmodulesChanged,
	"msgLockNotAncestor":      MsgLockNotAncestor,
	"msgNotGitlink":           MsgNotGitlink,
	"msgAckLapsed":            SumAckLapsed,
	"statusFile":              StatusFile,
	"statusOK":                StatusOK,
	"statusGitlinkMissing":    StatusGitlinkMissing,
	"statusUnreachablePrefix": StatusUnreachablePrefix,
	"submodulePath":           Submodule,
	"canonicalURL":            CanonicalURL,
	"upstreamRef":             MasterRef,
	"acksPath":                AcksPath,
}

// TestRootCopies checks the root gate's copies of this package's constants
// by name, so a copy that drifts fails even if the old text survives
// elsewhere in the root tests.
func TestRootCopies(t *testing.T) {
	f := filepath.Join(root, "rules_provenance_test.go")
	if _, err := os.Stat(f); err != nil {
		t.Skipf("root provenance gate not present: %v", err)
	}
	af, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, d := range af.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, sp := range gd.Specs {
			vs := sp.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if v, err := strconv.Unquote(lit.Value); err == nil {
						got[n.Name] = v
					}
				}
			}
		}
	}
	covered := map[string]bool{}
	for name, want := range rootCopies {
		covered[want] = true
		v, ok := got[name]
		switch {
		case !ok:
			t.Errorf("rules_provenance_test.go: no string constant %s (want %q)", name, want)
		case v != want:
			t.Errorf("rules_provenance_test.go: %s = %q, want %q", name, v, want)
		}
	}
	for _, m := range Shared {
		if !covered[m] {
			t.Errorf("Shared format %q has no entry in rootCopies", m)
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
