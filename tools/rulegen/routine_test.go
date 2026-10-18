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

// routineGhStart finds where a gh invocation's words begin.
var routineGhStart = regexp.MustCompile(`(?:^|[^A-Za-z0-9_./-])gh[ \t]+`)

// routineMergePath is the REST merge endpoint of a PR.
var routineMergePath = regexp.MustCompile(`(?:^|/)pulls/[^/]+/merge$`)

// routineWords splits s into shell words, quotes removed, up to the end of
// the shell command it starts: an unquoted newline, |, ;, &, ), a backtick
// (the end of a Markdown code span) or a # comment. < and > stay in words:
// the routine's placeholders (<n>, <sha>) use them.
func routineWords(s string) []string {
	var words []string
	var w strings.Builder
	in, quote := false, byte(0)
	flush := func() {
		if in {
			words = append(words, w.String())
		}
		w.Reset()
		in = false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				w.WriteByte(c)
			}
		case quote == '"':
			switch {
			case c == '"':
				quote = 0
			case c == '\\' && i+1 < len(s):
				i++
				w.WriteByte(s[i])
			default:
				w.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, in = c, true
		case c == ' ' || c == '\t':
			flush()
		case strings.IndexByte("\n|;&)`", c) >= 0, c == '#' && !in:
			flush()
			return words
		default:
			w.WriteByte(c)
			in = true
		}
	}
	flush()
	return words
}

// routineAPIValued are the gh api flags that take a value.
var routineAPIValued = map[string]bool{
	"-X": true, "--method": true, "-f": true, "--raw-field": true, "-F": true, "--field": true,
	"--input": true, "-H": true, "--header": true, "-q": true, "--jq": true, "-t": true,
	"--template": true, "-p": true, "--preview": true, "--hostname": true, "--cache": true,
}

// routineGhProblems lists what is wrong with the gh commands in text: a
// GraphQL-backed subcommand, any GraphQL call, a PR merge not pinned to a
// head, and any write to auto-merge.
func routineGhProblems(text string) []string {
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, "api.github.com/graphql") {
			out = append(out, "GraphQL endpoint: "+line)
		}
	}
	for _, loc := range routineGhStart.FindAllStringIndex(text, -1) {
		words := routineWords(text[loc[1]:])
		cmd := "gh " + strings.Join(words, " ")
		// gh's global flags may come before the subcommand.
		i := 0
		for i < len(words) && strings.HasPrefix(words[i], "-") {
			switch words[i] {
			case "-R", "--repo", "--hostname":
				i++
			}
			i++
		}
		if i >= len(words) {
			continue
		}
		if words[i] != "api" {
			sub, next := words[i], ""
			if i+1 < len(words) && !strings.HasPrefix(words[i+1], "-") {
				next = words[i+1]
			}
			ok := false
			for _, a := range routineGhAllowed {
				// A first word alone names the family in prose ("every `gh run` command").
				ok = ok || sub+" "+next == a || next == "" && strings.HasPrefix(a, sub+" ")
			}
			if !ok {
				out = append(out, "not a REST gh command: "+cmd)
			}
			continue
		}
		method, path, body := "", "", false
		pinned := false
		for a := words[i+1:]; len(a) > 0; a = a[1:] {
			w := a[0]
			if !strings.HasPrefix(w, "-") || w == "-" {
				if path == "" {
					path = w
				}
				continue
			}
			name, val, eq := w, "", false
			if strings.HasPrefix(w, "--") {
				name, val, eq = strings.Cut(w, "=")
			} else if len(w) > 2 {
				name, val, eq = w[:2], w[2:], true // -XPUT, -fsha=…
			}
			if routineAPIValued[name] && !eq && len(a) > 1 {
				a = a[1:]
				val = a[0]
			}
			switch name {
			case "-X", "--method":
				method = strings.ToUpper(val)
			case "-f", "--raw-field", "-F", "--field":
				body = true // gh sends POST unless told otherwise
				pinned = pinned || strings.HasPrefix(val, "sha=") && len(val) > len("sha=")
			case "--input":
				body = true
			}
		}
		if method == "" {
			method = "GET"
			if body {
				method = "POST"
			}
		}
		p, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "?")
		switch {
		case strings.Contains(strings.ToLower(p), "graphql"):
			out = append(out, "GraphQL call: "+cmd)
		case method != "GET" && routineMergePath.MatchString(p) && !pinned:
			out = append(out, "merge not pinned to a head: "+cmd)
		case method != "GET" && strings.Contains(p, "auto_merge"):
			out = append(out, "the routine never arms or disarms auto-merge: "+cmd)
		}
	}
	return out
}

// TestRoutineRESTOnly keeps every gh command the routine might run on REST,
// and every merge pinned to a head.
func TestRoutineRESTOnly(t *testing.T) {
	for _, path := range routineDocs {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range routineGhProblems(strings.ReplaceAll(string(b), "\\\n", " ")) {
			t.Errorf("%s: %s", path, p)
		}
	}
}

// TestRoutineGhProblems runs the matcher of TestRoutineRESTOnly on lines it
// must refuse and lines it must pass.
func TestRoutineGhProblems(t *testing.T) {
	const R = "repos/Espresso-Aficionados/linkclean"
	for _, tt := range []struct {
		line string
		bad  bool
	}{
		{"gh pr list", true},
		{"gh -R Espresso-Aficionados/linkclean pr list --state open", true},
		{"gh --repo Espresso-Aficionados/linkclean pr merge 5 --auto", true},
		{"gh --repo=Espresso-Aficionados/linkclean issue create", true},
		{"x=$(gh label list)", true},
		{"`gh pr view 5`", true},
		{"gh api graphql -f query=q", true},
		{"gh api -X POST graphql -f query=q", true},
		{"gh api --method POST /graphql -F query=@q.graphql", true},
		{"curl -X POST https://api.github.com/graphql", true},
		{"gh api --method PUT " + R + "/pulls/5/merge -f merge_method=squash", true},
		{`gh api -X PUT "` + R + `/pulls/5/merge" -f merge_method=squash`, true},
		{"gh api -XPUT " + R + "/pulls/5/merge -f merge_method=squash", true},
		{"gh api " + R + "/pulls/5/merge -f merge_method=squash", true},
		{"gh api -X PUT " + R + "/pulls/5/merge -f merge_method=squash -f sha=", true},
		{"gh api -X PUT " + R + "/pulls/5/merge -f merge_method=squash --jq sha=x", true},
		{"gh api --method PUT " + R + "/pulls/5/auto_merge", true},
		{"gh api -X DELETE " + R + "/pulls/5/ccr/auto_merge", true},
		{"gh api --method=PUT " + R + "/pulls/5/ccr/auto_merge", true},

		{"gh api -X PUT " + R + "/pulls/<n>/merge -f merge_method=squash -f sha=<V> --jq '[.merged, .sha] | @tsv'", false},
		{"gh api --method PUT " + R + "/pulls/5/merge --raw-field sha=0123abc -f merge_method=squash", false},
		{"gh api " + R + "/pulls/<n> --jq '[.merged, .merge_commit_sha] | @tsv'", false},
		{"gh api " + R + "/pulls/<n> --jq '.auto_merge != null'", false},
		{"gh api -X POST " + R + "/issues/<n>/comments -F body=@/tmp/lc/comment-<n>.md --jq .id", false},
		{`ID=$(gh api "` + R + `/issues/<n>/comments" --paginate --jq ".[] | .id" | tail -1)`, false},
		{"gh workflow run rules-alert.yml --repo Espresso-Aficionados/linkclean --ref main -f title=\"$t\"", false},
		{"gh run watch \"$id\" --repo Espresso-Aficionados/linkclean --exit-status", false},
		{"every `gh run` command", false},
		{"`gh api` (paths …)", false},
	} {
		if got := routineGhProblems(tt.line); (len(got) > 0) != tt.bad {
			t.Errorf("%s: problems %q, want any: %v", tt.line, got, tt.bad)
		}
	}
}

// TestRoutineRequiredChecks keeps the checks the routine reads equal to the
// eight it names, read from every check run on the commit.
func TestRoutineRequiredChecks(t *testing.T) {
	b, err := os.ReadFile("ROUTINE.md")
	if err != nil {
		t.Fatal(err)
	}
	const named = "`test (1.26.x)`, `test (stable)`,\n  `rules-provenance`, `rules-schema`, `rules-compile`, `rules-counts`,\n  `rules-risky`, `rules-golden`."
	const read = "'test (1.26.x)' 'test (stable)' rules-provenance rules-schema rules-compile rules-counts rules-risky rules-golden < "
	const runs = `gh api --paginate "repos/Espresso-Aficionados/linkclean/commits/<sha>/check-runs?filter=all&per_page=100" --jq '.check_runs[]' > /tmp/lc/checks-<sha>.json`
	for _, s := range []string{named, read, runs} {
		if !strings.Contains(string(b), s) {
			t.Errorf("ROUTINE.md lacks %q", s)
		}
	}
	if n := strings.Count(string(b), "/check-runs"); n != 1 {
		t.Errorf("ROUTINE.md reads check runs %d times, want once (Conventions)", n)
	}
}
