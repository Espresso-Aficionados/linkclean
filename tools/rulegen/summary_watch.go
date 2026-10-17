package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
)

// The watch is informational: brave-core applies two lists in code that
// linkclean does not vendor (the conditional trackers and the exempted
// hosts), so summary fetches the file at brave-core's default-branch HEAD
// and compares the two blocks' sha256 with tools/rulegen/watch.json, which
// humans update after reviewing a change.

// sumWatchURL is the one URL the watch fetches. It is a constant, never read
// from watch.json, so a PR cannot point the fetch at another host.
const sumWatchURL = "https://raw.githubusercontent.com/brave/brave-core/HEAD/" + data.SumWatchSource

// sumWatchBlocks are the watched identifiers, in report order.
var sumWatchBlocks = []string{data.SumWatchConditional, data.SumWatchExempt}

// sumWatchFile is tools/rulegen/watch.json. Source records where the hashes
// were taken from (the file at a fixed brave-core commit); it is not fetched.
type sumWatchFile struct {
	Source string            `json:"source"`
	Blocks map[string]string `json:"blocks"`
}

// sumWatchBlock extracts the block of ident from a C++ source: from the line
// declaring ident to the first line, from that one on, that ends the
// declaration (its text ends with ';'), newlines included. ok is false when
// ident or the end is missing.
func sumWatchBlock(src []byte, ident string) (block []byte, ok bool) {
	lines := bytes.SplitAfter(src, []byte("\n"))
	start := -1
	for i, l := range lines {
		if start < 0 && sumDeclares(l, ident) {
			start = i
		}
		if start >= 0 && bytes.HasSuffix(bytes.TrimSpace(l), []byte(";")) {
			return bytes.Join(lines[start:i+1], nil), true
		}
	}
	return nil, false
}

// sumDeclares reports whether line holds ident as a whole word followed by
// '=' (a declaration, not a use).
func sumDeclares(line []byte, ident string) bool {
	i := bytes.Index(line, []byte(ident))
	if i < 0 {
		return false
	}
	if i > 0 && sumIdentByte(line[i-1]) {
		return false
	}
	rest := line[i+len(ident):]
	if len(rest) > 0 && sumIdentByte(rest[0]) {
		return false
	}
	return bytes.HasPrefix(bytes.TrimSpace(rest), []byte("="))
}

func sumIdentByte(c byte) bool {
	return c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// summaryWatch fetches brave-core's file through e.HTTP and compares its
// blocks with root's watch.json. Every failure is a note.
func summaryWatch(ctx context.Context, e *Env, root string) []string {
	var w sumWatchFile
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(data.WatchPath)))
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		err = dec.Decode(&w)
	}
	if err != nil {
		return []string{fmt.Sprintf("note: %s unreadable (%v); brave-core blocks not compared", data.WatchPath, err)}
	}
	src, err := sumFetch(ctx, e.HTTP, sumWatchURL)
	if err != nil {
		return []string{fmt.Sprintf("note: brave-core fetch failed (%v); blocks not compared", err)}
	}
	var lines []string
	changed := false
	for _, id := range sumWatchBlocks {
		want := w.Blocks[id]
		blk, ok := sumWatchBlock(src, id)
		switch {
		case !ok:
			changed = true
			lines = append(lines, fmt.Sprintf("- `%s`: not found in brave-core `%s` (watch.json %s)", id, data.SumWatchSource, sumShort(want)))
		case data.SHA256Hex(blk) != want:
			changed = true
			lines = append(lines, fmt.Sprintf("- `%s`: %s → %s", id, sumShort(want), sumShort(data.SHA256Hex(blk))))
		default:
			lines = append(lines, fmt.Sprintf("- `%s`: unchanged (%s)", id, sumShort(want)))
		}
	}
	head := fmt.Sprintf("brave-core `%s` at HEAD matches %s", data.SumWatchSource, data.WatchPath)
	if changed {
		head = fmt.Sprintf("**%s** (`%s` at HEAD vs %s); review it, then update %s. Informational only.", data.SumWatchChanged, data.SumWatchSource, data.WatchPath, data.WatchPath)
	}
	return append([]string{head, ""}, lines...)
}

// sumFetch GETs url with a deadline and a size cap.
func sumFetch(ctx context.Context, c *http.Client, url string) ([]byte, error) {
	if c == nil {
		return nil, errors.New("no HTTP client")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	const limit = 4 << 20
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if len(b) > limit {
		return nil, fmt.Errorf("GET %s: more than %d bytes", url, limit)
	}
	return b, nil
}

// sumShort shortens a hex id to data.ShortLen digits.
func sumShort(s string) string {
	if s == "" {
		return "(none)"
	}
	if len(s) > data.ShortLen {
		return s[:data.ShortLen]
	}
	return s
}
