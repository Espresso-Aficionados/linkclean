package main

import (
	"slices"
	"strings"
)

// relChangelogPath is the human-written release history (root only).
const relChangelogPath = "CHANGELOG.md"

// relChangelogSection returns the body of CHANGELOG.md's "## vX.Y.Z"
// section for v: the lines after the heading up to the next "#" or "##"
// heading, trimmed. A heading may carry text after the version
// ("## v0.2.0 (2026-10-20)").
func relChangelogSection(b []byte, v relVersion) (body string, ok bool) {
	head := "## " + v.String()
	var lines []string
	in := false
	for line := range strings.SplitSeq(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if in && (strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "# ")) {
			break
		}
		if in {
			lines = append(lines, line)
			continue
		}
		if rest, found := strings.CutPrefix(strings.TrimRight(line, " \t"), head); found && (rest == "" || rest[0] == ' ' || rest[0] == '\t') {
			in, ok = true, true
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n")), ok
}

// relGoldenRows returns the rows of a golden expectation file, by row ID:
// the "-- <id> --" sections of a .txtar, the "<id> <url>" lines of
// negative.txt. Other golden files have no row IDs.
func relGoldenRows(p string, b []byte) (ids []string, rows map[string]string) {
	rows = map[string]string{}
	add := func(id, text string) {
		if _, seen := rows[id]; !seen {
			ids = append(ids, id)
		}
		rows[id] += text
	}
	switch {
	case strings.HasSuffix(p, ".txtar"):
		id := ""
		for line := range strings.SplitSeq(string(b), "\n") {
			if name, ok := relTxtarMarker(line); ok {
				id = name
				add(id, "")
				continue
			}
			if id != "" {
				add(id, line+"\n")
			}
		}
	case strings.HasSuffix(p, "/negative.txt"):
		for line := range strings.SplitSeq(string(b), "\n") {
			t := strings.TrimSpace(line)
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			id, _, _ := strings.Cut(t, " ")
			add(id, t+"\n")
		}
	}
	return ids, rows
}

// relTxtarMarker parses a txtar file marker line "-- name --".
func relTxtarMarker(line string) (string, bool) {
	line = strings.TrimRight(line, "\r")
	if !strings.HasPrefix(line, "-- ") || !strings.HasSuffix(line, " --") || len(line) < 7 {
		return "", false
	}
	name := strings.TrimSpace(line[3 : len(line)-3])
	return name, name != ""
}

// relChangedRows lists the row IDs that differ between two versions of a
// golden file: changed and added rows in head order, then removed rows.
func relChangedRows(p string, oldB, newB []byte) []string {
	oldIDs, oldRows := relGoldenRows(p, oldB)
	newIDs, newRows := relGoldenRows(p, newB)
	var out []string
	for _, id := range newIDs {
		if old, ok := oldRows[id]; !ok || old != newRows[id] {
			out = append(out, id)
		}
	}
	for _, id := range oldIDs {
		if _, ok := newRows[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

// relListsToken reports whether text names id as a delimited token, so
// row "1" is not found inside "12" or "v0.1.0".
func relListsToken(text, id string) bool {
	const before, after = " \t\n(,[`'\"#/*", " \t\n),;:.]`'\"/*"
	for i := 0; ; {
		j := strings.Index(text[i:], id)
		if j < 0 {
			return false
		}
		j += i
		end := j + len(id)
		if (j == 0 || strings.IndexByte(before, text[j-1]) >= 0) && (end == len(text) || strings.IndexByte(after, text[end]) >= 0) {
			return true
		}
		i = j + 1
	}
}

// relUnlisted returns the IDs in ids that section does not name.
func relUnlisted(section string, ids []string) []string {
	var out []string
	for _, id := range ids {
		if !relListsToken(section, id) && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}
