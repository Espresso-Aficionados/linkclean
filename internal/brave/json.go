package brave

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/Espresso-Aficionados/linkclean/internal/matchpattern"
	"github.com/Espresso-Aficionados/linkclean/internal/rule"
)

// The schemas are checked on generic JSON values rather than with struct
// decoding: encoding/json matches field names case-insensitively, silently
// turns null into a zero value and keeps the last of two equal keys, and a
// strict schema must reject all three.

// decodeStrict decodes data as exactly one JSON value. Objects become
// map[string]any and arrays []any. Duplicate object keys and trailing data are
// errors.
func decodeStrict(data []byte) (any, error) {
	if err := CheckDuplicateKeys(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

// CheckDuplicateKeys walks the token stream and reports the first key that
// occurs twice in one object.
func CheckDuplicateKeys(data []byte) error {
	type frame struct {
		keys    map[string]bool // nil for arrays
		wantKey bool
	}
	var stack []frame
	// value marks that the enclosing object, if any, has consumed a value.
	value := func() {
		if n := len(stack); n > 0 && stack[n-1].keys != nil {
			stack[n-1].wantKey = true
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				value()
				stack = append(stack, frame{keys: map[string]bool{}, wantKey: true})
			case '[':
				value()
				stack = append(stack, frame{})
			default:
				stack = stack[:len(stack)-1]
			}
		case string:
			if n := len(stack); n > 0 && stack[n-1].keys != nil && stack[n-1].wantKey {
				if stack[n-1].keys[t] {
					return fmt.Errorf("duplicate key %q", t)
				}
				stack[n-1].keys[t] = true
				stack[n-1].wantKey = false
				continue
			}
			value()
		default:
			value()
		}
	}
}

// kind names the JSON type of a decoded value for error messages.
func kind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

// errList collects *rule.Error values for one rule set.
type errList struct {
	set  string
	errs []error
}

func (l *errList) add(id, field, format string, args ...any) {
	l.errs = append(l.errs, &rule.Error{Set: l.set, RuleID: id, Field: field, Err: fmt.Errorf(format, args...)})
}

func (l *errList) err() error { return errors.Join(l.errs...) }

// object returns v as an object after checking that every key is one of
// known. Unknown keys are reported in sorted order.
func (l *errList) object(id, field string, v any, known ...string) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		l.add(id, field, "want object, have %s", kind(v))
		return nil, false
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if !slices.Contains(known, k) {
			l.add(id, join(field, k), "unknown field")
		}
	}
	return m, true
}

// str returns m[key], which must be a non-empty string when present.
func (l *errList) str(id, field string, m map[string]any, key string, required bool) (s string, present bool) {
	v, ok := m[key]
	if !ok {
		if required {
			l.add(id, join(field, key), "missing")
		}
		return "", false
	}
	s, isStr := v.(string)
	switch {
	case !isStr:
		l.add(id, join(field, key), "want string, have %s", kind(v))
	case s == "":
		l.add(id, join(field, key), "empty string")
	}
	return s, true
}

// strs returns m[key], which must be an array of non-empty strings when
// present. required means present and non-empty. On an invalid element it
// returns nil, so callers can index the result like the input.
func (l *errList) strs(id string, m map[string]any, key string, required bool) []string {
	v, ok := m[key]
	if !ok {
		if required {
			l.add(id, key, "missing")
		}
		return nil
	}
	a, isArr := v.([]any)
	if !isArr {
		l.add(id, key, "want array of strings, have %s", kind(v))
		return nil
	}
	if required && len(a) == 0 {
		l.add(id, key, "empty")
	}
	out := make([]string, len(a))
	n := len(l.errs)
	for i, e := range a {
		s, isStr := e.(string)
		switch {
		case !isStr:
			l.add(id, fmt.Sprintf("%s[%d]", key, i), "want string, have %s", kind(e))
		case s == "":
			l.add(id, fmt.Sprintf("%s[%d]", key, i), "empty string")
		}
		out[i] = s
	}
	if len(l.errs) > n {
		return nil
	}
	return out
}

// patterns is strs for include/exclude lists; it also rejects the linkclean
// "label.*" host extension, which Brave data must not use (§4.5.2). The
// engine rejects it too, for every "brave/" set; checking here as well makes
// the error name the upstream element ("#<index>") rather than the compiled
// rule, which for debounce is a group of elements. The rest of the pattern
// grammar is the engine's to check.
func (l *errList) patterns(id string, m map[string]any, key string, required bool) []string {
	ps := l.strs(id, m, key, required)
	for i, p := range ps {
		if mp, err := matchpattern.Parse(p, true); err == nil {
			if kind, _ := mp.Host(); kind == matchpattern.HostLabel {
				l.add(id, fmt.Sprintf("%s[%d]", key, i), "pattern %q: label.* hosts are a linkclean extension, not allowed in Brave data", p)
			}
		}
	}
	return ps
}

func join(field, key string) string {
	if field == "" {
		return key
	}
	return field + "." + key
}
