package data_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/basetree"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/gittest"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
)

// TestSummaryDataContract runs the root module's TestSummaryData, with base
// set to the copy's own HEAD, and reads what it writes with
// ParseSummaryData. The decode is strict, so a field the writer and the
// reader spell differently fails here.
func TestSummaryDataContract(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the root module")
	}
	l := gittest.NewLinkclean(t, repo.Exec{Env: gittest.Isolate(t)}, gittest.Opts{FullModule: true})
	tmp := t.TempDir()
	base := filepath.Join(tmp, "base")
	if err := basetree.Write(context.Background(), l.Work, "HEAD", base); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(tmp, "summary.json")
	out, ok := l.GoTest(t, "^TestSummaryData$", map[string]string{
		data.SummaryEnv:      outPath,
		"LINKCLEAN_BASE_DIR": base,
	})
	if !ok {
		t.Fatalf("go test failed:\n%s", out)
	}
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	d, err := data.ParseSummaryData(b)
	if err != nil {
		t.Fatal(err)
	}
	if d.BaseError != "" || d.HeadError != "" {
		t.Errorf("errors: base %q, head %q", d.BaseError, d.HeadError)
	}
	if len(d.Corpus) == 0 || len(d.HeadRuleIDs) == 0 || len(d.HeadDebounceGroups) == 0 || len(d.CorpusScopes) == 0 {
		t.Errorf("empty sections: %d corpus, %d rule IDs, %d debounce groups, %d scopes",
			len(d.Corpus), len(d.HeadRuleIDs), len(d.HeadDebounceGroups), len(d.CorpusScopes))
	}
	// Base is head's own tree, so both sides agree.
	if !slices.Equal(d.BaseRuleIDs, d.HeadRuleIDs) {
		t.Errorf("base and head rule IDs differ: %d vs %d", len(d.BaseRuleIDs), len(d.HeadRuleIDs))
	}
	for _, c := range d.Corpus {
		if c.Base != c.Head || c.BrokenBase != c.BrokenHead {
			t.Errorf("corpus %s: base %q (broken %v), head %q (broken %v)", c.ID, c.Base, c.BrokenBase, c.Head, c.BrokenHead)
		}
	}
	for _, n := range d.Hygiene {
		if !n.AtBase || !slices.Contains([]string{data.HygieneStaleKeep, data.HygieneDuplicated, data.HygieneOverlap}, n.Kind) {
			t.Errorf("hygiene note %+v: unknown kind or not at base", n)
		}
	}
}
