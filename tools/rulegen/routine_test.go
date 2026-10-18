package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// routineDocs are the files the review routine follows or reads.
var routineDocs = []string{"ROUTINE.md", filepath.Join("..", "..", "CONTRIBUTING.md"), filepath.Join("..", "..", "README.md")}

// routineGhAllowed are the gh subcommands that work without GitHub's
// GraphQL API, which the routine's environment refuses. gh's PR, issue,
// label and other GraphQL-backed subcommands are not among them. ROUTINE.md
// names auth status only to forbid gating on it.
var routineGhAllowed = []string{"workflow run", "run list", "run view", "run watch", "repo clone", "auth token", "auth setup-git", "auth status"}

// TestRoutineRESTOnly keeps every gh command the routine might run on REST,
// and every merge pinned to a head.
func TestRoutineRESTOnly(t *testing.T) {
	ghCmd := regexp.MustCompile(`\bgh\s+([a-z][a-z-]*)(?:\s+([a-z][a-z-]*))?`)
	for _, path := range routineDocs {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ReplaceAll(string(b), "\\\n", " ")
		for _, m := range ghCmd.FindAllStringSubmatch(text, -1) {
			ok := m[1] == "api"
			for _, a := range routineGhAllowed {
				// A first word alone names the family in prose ("every gh run command").
				ok = ok || m[1]+" "+m[2] == a || m[2] == "" && strings.HasPrefix(a, m[1]+" ")
			}
			if !ok {
				t.Errorf("%s: %q is not a REST gh command", path, m[0])
			}
		}
		for line := range strings.SplitSeq(text, "\n") {
			if strings.Contains(line, "gh api graphql") {
				t.Errorf("%s: GraphQL call: %s", path, line)
			}
			if strings.Contains(line, "-X PUT") && strings.Contains(line, "/merge ") && !strings.Contains(line, "-f sha=<") {
				t.Errorf("%s: merge not pinned to a head: %s", path, line)
			}
			if strings.Contains(line, "auto_merge") && strings.Contains(line, "-X ") {
				t.Errorf("%s: the routine never arms or disarms auto-merge: %s", path, line)
			}
		}
	}
}

// TestRoutineRequiredChecks keeps the checks the routine reads equal to the
// eight it names.
func TestRoutineRequiredChecks(t *testing.T) {
	b, err := os.ReadFile("ROUTINE.md")
	if err != nil {
		t.Fatal(err)
	}
	const named = "`test (1.26.x)`, `test (stable)`,\n  `rules-provenance`, `rules-schema`, `rules-compile`, `rules-counts`,\n  `rules-risky`, `rules-golden`."
	const read = "'test (1.26.x)' 'test (stable)' rules-provenance rules-schema rules-compile rules-counts rules-risky rules-golden < "
	for _, s := range []string{named, read} {
		if !strings.Contains(string(b), s) {
			t.Errorf("ROUTINE.md lacks %q", s)
		}
	}
}
