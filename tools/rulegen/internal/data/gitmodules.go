package data

import (
	"errors"
	"fmt"
	"strings"
)

// SubmoduleEntry is one [submodule "name"] section of .gitmodules. Where a
// key repeats, the last value wins, as in git.
type SubmoduleEntry struct {
	Name, Path, URL, Branch string
}

// gmSection is a parsed config section with every value of every key.
type gmSection struct {
	name, sub string // name lowercased; sub as written
	hasSub    bool
	keys      map[string][]string // lowercased key → values in order
}

// ParseGitmodules parses .gitmodules in git-config syntax (comments, quoted
// values, escapes, line continuations, case-insensitive section and key
// names). Sections other than submodule are skipped, as git skips them when
// it reads submodule configuration.
func ParseGitmodules(b []byte) ([]SubmoduleEntry, error) {
	secs, err := parseConfig(string(b))
	if err != nil {
		return nil, err
	}
	var out []SubmoduleEntry
	for _, s := range secs {
		if s.name != "submodule" || !s.hasSub {
			continue
		}
		out = append(out, SubmoduleEntry{Name: s.sub, Path: last(s.keys["path"]), URL: last(s.keys["url"]), Branch: last(s.keys["branch"])})
	}
	return out, nil
}

// CheckMapping checks that .gitmodules maps Submodule to CanonicalURL (with
// or without ".git") on branch master: exactly one submodule section has
// that path, its path, url and branch are each set once, and the file has
// no sections other than submodule ones.
func CheckMapping(gitmodules []byte) error {
	secs, err := parseConfig(string(gitmodules))
	if err != nil {
		return fmt.Errorf("%s: %w", GitmodulesPath, err)
	}
	var errs []error
	var found []gmSection
	for _, s := range secs {
		if s.name != "submodule" || !s.hasSub {
			errs = append(errs, fmt.Errorf("%s: unexpected section [%s]", GitmodulesPath, s.display()))
			continue
		}
		if last(s.keys["path"]) == Submodule || s.sub == Submodule {
			found = append(found, s)
		}
	}
	switch len(found) {
	case 0:
		errs = append(errs, fmt.Errorf("%s: no submodule with path %s", GitmodulesPath, Submodule))
	case 1:
		s := found[0]
		for _, k := range []string{"path", "url", "branch"} {
			if n := len(s.keys[k]); n != 1 {
				errs = append(errs, fmt.Errorf("%s: [%s] has %d %s values, want 1", GitmodulesPath, s.display(), n, k))
			}
		}
		if p := last(s.keys["path"]); p != Submodule {
			errs = append(errs, fmt.Errorf("%s: [%s] path = %q, want %q", GitmodulesPath, s.display(), p, Submodule))
		}
		if u := last(s.keys["url"]); u != CanonicalURL && u != CanonicalURL+".git" {
			errs = append(errs, fmt.Errorf("%s: %s url = %q, want %s", GitmodulesPath, Submodule, u, CanonicalURL))
		}
		if br := last(s.keys["branch"]); br != Branch {
			errs = append(errs, fmt.Errorf("%s: %s branch = %q, want %s", GitmodulesPath, Submodule, br, Branch))
		}
	default:
		errs = append(errs, fmt.Errorf("%s: %d submodule sections name %s, want 1", GitmodulesPath, len(found), Submodule))
	}
	return errors.Join(errs...)
}

func (s gmSection) display() string {
	if s.hasSub {
		return fmt.Sprintf("%s %q", s.name, s.sub)
	}
	return s.name
}

func last(vs []string) string {
	if len(vs) == 0 {
		return ""
	}
	return vs[len(vs)-1]
}

// parseConfig parses git-config text into sections; a header that repeats
// continues the earlier section, as git treats it.
func parseConfig(src string) ([]gmSection, error) {
	src = strings.TrimPrefix(strings.ReplaceAll(src, "\r\n", "\n"), "\ufeff")
	var secs []gmSection
	cur := -1
	line := 1
	i := 0
	errAt := func(msg string) error { return fmt.Errorf("line %d: %s", line, msg) }
	for i < len(src) {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == ' ' || c == '\t':
			i++
		case c == '#' || c == ';':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '[':
			s, n, err := parseHeader(src[i:])
			if err != nil {
				return nil, errAt(err.Error())
			}
			i += n
			if rest := restOfLine(src[i:]); rest != "" {
				return nil, errAt("text after section header")
			}
			cur = -1
			for k, e := range secs {
				if e.name == s.name && e.hasSub == s.hasSub && e.sub == s.sub {
					cur = k
				}
			}
			if cur < 0 {
				secs = append(secs, s)
				cur = len(secs) - 1
			}
		case isAlpha(c):
			if cur < 0 {
				return nil, errAt("key outside a section")
			}
			j := i
			for j < len(src) && (isAlpha(src[j]) || '0' <= src[j] && src[j] <= '9' || src[j] == '-') {
				j++
			}
			key := strings.ToLower(src[i:j])
			for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
				j++
			}
			val := ""
			if j < len(src) && src[j] == '=' {
				v, n, nl, err := parseValue(src[j+1:])
				if err != nil {
					return nil, errAt(err.Error())
				}
				val, j, line = v, j+1+n, line+nl
			} else if j < len(src) && src[j] != '\n' {
				return nil, errAt("malformed key " + strings.TrimSpace(src[i:min(j+1, len(src))]))
			}
			secs[cur].keys[key] = append(secs[cur].keys[key], val)
			i = j
		default:
			return nil, errAt(fmt.Sprintf("unexpected %q", c))
		}
	}
	return secs, nil
}

// parseHeader parses "[name]", "[name "sub"]" or the old "[name.sub]" at the
// start of s and returns the section and the bytes consumed.
func parseHeader(s string) (gmSection, int, error) {
	i := 1
	j := i
	for j < len(s) && (isAlpha(s[j]) || '0' <= s[j] && s[j] <= '9' || s[j] == '-' || s[j] == '.') {
		j++
	}
	if j == i {
		return gmSection{}, 0, errors.New("malformed section header")
	}
	sec := gmSection{name: strings.ToLower(s[i:j]), keys: map[string][]string{}}
	if j < len(s) && s[j] == ']' {
		if name, sub, ok := strings.Cut(s[i:j], "."); ok {
			sec.name, sec.sub, sec.hasSub = strings.ToLower(name), strings.ToLower(sub), true
		}
		return sec, j + 1, nil
	}
	if strings.Contains(sec.name, ".") {
		return gmSection{}, 0, errors.New("malformed section header")
	}
	for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
		j++
	}
	if j >= len(s) || s[j] != '"' {
		return gmSection{}, 0, errors.New("malformed section header")
	}
	j++
	var sub strings.Builder
	for {
		if j >= len(s) || s[j] == '\n' {
			return gmSection{}, 0, errors.New("unterminated subsection name")
		}
		c := s[j]
		if c == '"' {
			break
		}
		if c == '\\' {
			j++
			if j >= len(s) || s[j] == '\n' {
				return gmSection{}, 0, errors.New("unterminated subsection name")
			}
			c = s[j]
		}
		sub.WriteByte(c)
		j++
	}
	j++
	if j >= len(s) || s[j] != ']' {
		return gmSection{}, 0, errors.New("malformed section header")
	}
	sec.sub, sec.hasSub = sub.String(), true
	return sec, j + 1, nil
}

// parseValue parses a value after '=' up to the end of its (possibly
// continued) line, as git's parse_value does: unquoted leading and
// trailing whitespace is dropped, and an unquoted '#' or ';' starts a
// comment. It returns the
// value, the bytes consumed (not including the final newline) and the
// number of newlines consumed by continuations.
func parseValue(s string) (string, int, int, error) {
	var b strings.Builder
	quoted := false
	var space []byte // unquoted whitespace not yet known to be inside the value
	nl := 0
	i := 0
	for ; i < len(s); i++ {
		c := s[i]
		if c == '\n' {
			break
		}
		if !quoted && (c == ' ' || c == '\t') {
			if b.Len() > 0 {
				space = append(space, c)
			}
			continue
		}
		if !quoted && (c == '#' || c == ';') {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			break
		}
		b.Write(space)
		space = space[:0]
		switch c {
		case '"':
			quoted = !quoted
		case '\\':
			i++
			if i >= len(s) {
				return "", 0, 0, errors.New("backslash at end of input")
			}
			switch s[i] {
			case '\n':
				nl++
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			case '\\', '"':
				b.WriteByte(s[i])
			default:
				return "", 0, 0, fmt.Errorf("bad escape \\%c", s[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	if quoted {
		return "", 0, 0, errors.New("unterminated quoted value")
	}
	return b.String(), i, nl, nil
}

// restOfLine returns the non-blank, non-comment text before the next
// newline.
func restOfLine(s string) string {
	end := strings.IndexByte(s, '\n')
	if end < 0 {
		end = len(s)
	}
	t := strings.TrimSpace(s[:end])
	if strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") {
		return ""
	}
	return t
}

func isAlpha(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }
