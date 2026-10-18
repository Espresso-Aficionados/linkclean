package unshorten

import (
	"bytes"
	"strings"
	"testing"
)

// txtarFile is one "-- name --" section of a txtar archive.
type txtarFile struct {
	Name string
	Data []byte
	Line int // line of the section marker
}

// parseTxtar splits data into its leading comment and its sections, as
// golang.org/x/tools/txtar does (without the dependency). It is a copy of the
// root package's helper, which test files here cannot import.
func parseTxtar(data []byte) (comment []byte, files []txtarFile) {
	lines := bytes.SplitAfter(data, []byte("\n"))
	cur := -1
	for i, l := range lines {
		s := strings.TrimRight(string(l), "\r\n")
		if strings.HasPrefix(s, "-- ") && strings.HasSuffix(s, " --") && len(s) > 6 {
			files = append(files, txtarFile{Name: strings.TrimSpace(s[3 : len(s)-3]), Line: i + 1})
			cur = len(files) - 1
			continue
		}
		if cur < 0 {
			comment = append(comment, l...)
		} else {
			files[cur].Data = append(files[cur].Data, l...)
		}
	}
	return comment, files
}

func TestParseTxtar(t *testing.T) {
	comment, files := parseTxtar([]byte("# c\n-- meta.json --\n{}\n-- body --\n-- x --\na\r\nb\n"))
	if string(comment) != "# c\n" {
		t.Errorf("comment %q", comment)
	}
	want := []txtarFile{{"meta.json", []byte("{}\n"), 2}, {"body", nil, 4}, {"x", []byte("a\r\nb\n"), 5}}
	if len(files) != len(want) {
		t.Fatalf("got %d files", len(files))
	}
	for i, f := range files {
		if f.Name != want[i].Name || !bytes.Equal(f.Data, want[i].Data) || f.Line != want[i].Line {
			t.Errorf("file %d = %+v, want %+v", i, f, want[i])
		}
	}
}
