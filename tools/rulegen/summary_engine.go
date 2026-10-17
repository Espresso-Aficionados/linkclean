package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
)

// summaryEngine runs the root module's TestSummaryData over head (root's
// working tree) and baseDir and reads what it writes. On failure it returns
// a note with the end of the go test output.
func summaryEngine(ctx context.Context, e *Env, root, baseDir, tmp string) (*data.SummaryData, string) {
	outPath := filepath.Join(tmp, "summary-data.json")
	env := []string{data.SummaryEnv + "=" + outPath, "LINKCLEAN_BASE_DIR=" + baseDir}
	stdout, stderr, err := e.Go(ctx, root, env, "test", "-count=1", "-run", "^TestSummaryData$", ".")
	if err != nil {
		return nil, fmt.Sprintf("`go test -run '^TestSummaryData$' .` failed (%v):\n%s", err, sumTail(append(stdout, stderr...), 30))
	}
	b, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Sprintf("TestSummaryData wrote nothing (%v):\n%s", err, sumTail(append(stdout, stderr...), 30))
	}
	d, err := data.ParseSummaryData(b)
	if err != nil {
		return nil, fmt.Sprintf("TestSummaryData output: %v", err)
	}
	return &d, ""
}

// sumTail returns the last n lines of b.
func sumTail(b []byte, n int) string {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = append([]string{"…"}, lines[len(lines)-n:]...)
	}
	return strings.Join(lines, "\n")
}

var sumSubtestRe = regexp.MustCompile(`^\s*--- (PASS|FAIL|SKIP): TestRules/([^/\s]+) `)

// summaryPreflight runs the root TestRules gates against head with base
// (and the upstream clone, when there is one) and renders the results.
func summaryPreflight(ctx context.Context, e *Env, root, baseDir, upstream string) []string {
	env := []string{"LINKCLEAN_BASE_DIR=" + baseDir, "LINKCLEAN_UPSTREAM_DIR=" + upstream}
	stdout, stderr, err := e.Go(ctx, root, env, "test", "-count=1", "-v", "-run", "^TestRules$", ".")
	out := append(stdout, stderr...)
	var lines []string
	if err == nil {
		lines = append(lines, "`go test -run '^TestRules$' .`: **ok**")
	} else {
		lines = append(lines, fmt.Sprintf("`go test -run '^TestRules$' .`: **failed** (%v)", err))
	}
	var rows []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if m := sumSubtestRe.FindStringSubmatch(sc.Text()); m != nil {
			rows = append(rows, fmt.Sprintf("| %s | %s |", m[2], m[1]))
		}
	}
	if len(rows) > 0 {
		lines = append(lines, "", "| Check | Result |", "|---|---|")
		lines = append(lines, rows...)
	}
	if err != nil {
		lines = append(lines, "", "<details><summary>go test output (end)</summary>", "", "```", sumTail(out, 80), "```", "", "</details>")
	}
	return lines
}
