package data

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// subAdd is .gitmodules as `git submodule add -b master` writes it.
const subAdd = "[submodule \"tools/upstream/adblock-lists\"]\n\tpath = tools/upstream/adblock-lists\n\turl = https://github.com/brave/adblock-lists\n\tbranch = master\n"

func TestParseGitmodules(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []SubmoduleEntry
	}{
		{"submodule_add", subAdd, []SubmoduleEntry{{"tools/upstream/adblock-lists", "tools/upstream/adblock-lists", "https://github.com/brave/adblock-lists", "master"}}},
		{"syntax", "\ufeff# c\n; c\n[Submodule \"a \\\"q\\\" \\\\\"]\n  PATH=\"p q\" # trailing\n\turl = u\\\n2 ;c\n branch = \" b \"\n[core]\nbare\n",
			[]SubmoduleEntry{{"a \"q\" \\", "p q", "u2", " b "}}},
		{"whitespace_runs", "[submodule \"a\"]\n\turl = x  y  \n", []SubmoduleEntry{{Name: "a", URL: "x  y"}}},
		{"escapes", "[submodule \"a\"]\n\turl = a\\tb\\nc\n", []SubmoduleEntry{{Name: "a", URL: "a\tb\nc"}}},
		{"last_wins_and_merge", "[submodule \"a\"]\n\turl = one\n[submodule \"b\"]\n\turl = b\n[submodule \"a\"]\n\turl = two\n",
			[]SubmoduleEntry{{Name: "a", URL: "two"}, {Name: "b", URL: "b"}}},
		{"old_syntax_lowercased", "[submodule.ABC]\n\turl = x\n", []SubmoduleEntry{{Name: "abc", URL: "x"}}},
		{"crlf", "[submodule \"a\"]\r\n\tpath = p\r\n", []SubmoduleEntry{{Name: "a", Path: "p"}}},
		{"empty", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseGitmodules([]byte(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseGitmodules = %q, want %q", got, tt.want)
			}
			gitParity(t, tt.in)
		})
	}
	for _, in := range []string{
		"url = x\n",
		"[submodule \"a]\n",
		"[submodule \"a\"\n",
		"[ submodule \"a\"]\n",
		"[submodule \"a\"]\n\turl = \"x\n",
		"[submodule \"a\"]\n\turl = x\\q\n",
		"[submodule \"a\"]\n\t9url = x\n",
		"[submodule \"a\"]\n\turl x\n",
		"[submodule \"a\"]\n\turl # c\n",
		"[submodule \"a\"] url = x\n",
	} {
		if got, err := ParseGitmodules([]byte(in)); err == nil {
			t.Errorf("ParseGitmodules(%q) = %q, want an error", in, got)
		}
	}
}

// gitParity checks the submodule values against git's own reading, when git
// is available.
func gitParity(t *testing.T, in string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		return
	}
	p := filepath.Join(t.TempDir(), "gitmodules")
	if err := os.WriteFile(p, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "config", "--file", p, "--null", "--list")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git config: %v", err)
	}
	entries, err := ParseGitmodules([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	ours := map[string]string{}
	for _, e := range entries {
		for k, v := range map[string]string{"path": e.Path, "url": e.URL, "branch": e.Branch} {
			if v != "" {
				ours["submodule."+e.Name+"."+k] = v
			}
		}
	}
	theirs := map[string]string{}
	for rec := range strings.SplitSeq(string(out), "\x00") {
		k, v, _ := strings.Cut(rec, "\n")
		if !strings.HasPrefix(k, "submodule.") {
			continue
		}
		i := strings.LastIndexByte(k, '.')
		if key := k[i+1:]; key == "path" || key == "url" || key == "branch" {
			theirs[k] = v // git lists in file order, so the last value wins here too
		}
	}
	if !reflect.DeepEqual(ours, theirs) {
		t.Errorf("parse differs from git config:\n ours %q\n git  %q", ours, theirs)
	}
}

func TestCheckMapping(t *testing.T) {
	if err := CheckMapping([]byte(subAdd)); err != nil {
		t.Errorf("submodule add form: %v", err)
	}
	ok := []string{
		strings.Replace(subAdd, "adblock-lists\n\tbranch", "adblock-lists.git\n\tbranch", 1),
		subAdd + "[submodule \"other\"]\n\tpath = x\n\turl = https://example.com/x\n",
		"[submodule \"renamed\"]\n\tpath = tools/upstream/adblock-lists\n\turl = \"https://github.com/brave/adblock-lists\"\n\tbranch = master # pinned\n",
	}
	for _, in := range ok {
		if err := CheckMapping([]byte(in)); err != nil {
			t.Errorf("CheckMapping(%q): %v", in, err)
		}
	}
	bad := []struct{ name, in, want string }{
		{"fork", strings.Replace(subAdd, "github.com/brave/", "github.com/fork/", 1), "url ="},
		{"http", strings.Replace(subAdd, "https://", "http://", 1), "url ="},
		{"trailing_slash", strings.Replace(subAdd, "adblock-lists\n\tbranch", "adblock-lists/\n\tbranch", 1), "url ="},
		{"branch_main", strings.Replace(subAdd, "branch = master", "branch = main", 1), "branch ="},
		{"no_branch", strings.Replace(subAdd, "\tbranch = master\n", "", 1), "branch values"},
		{"repeated_url", subAdd + "\turl = https://github.com/fork/adblock-lists\n", "url values"},
		{"repeated_url_same", subAdd + "\turl = https://github.com/brave/adblock-lists\n", "url values"},
		{"two_sections", subAdd + "[submodule \"b\"]\n\tpath = tools/upstream/adblock-lists\n\turl = https://github.com/fork/x\n\tbranch = master\n", "2 submodule sections"},
		{"other_path", strings.Replace(subAdd, "path = tools/upstream/adblock-lists", "path = tools/upstream/other", 1), "path ="},
		{"none", "", "no submodule"},
		{"include", subAdd + "[include]\n\tpath = x\n", "unexpected section"},
		{"syntax", subAdd + "!garbage\n", "line 5"},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckMapping([]byte(tt.in))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("CheckMapping error %v, want it to contain %q", err, tt.want)
			}
		})
	}
	if !bytes.Contains([]byte(subAdd), []byte("\tpath")) {
		t.Fatal("subAdd must be tab-indented")
	}
}
