package brave

import (
	"slices"
	"strconv"

	"github.com/Espresso-Aficionados/linkclean/internal/rule"
)

// CompileStrip compiles clean-urls.json or query-filter.json (design §4.5.2)
// into Strip rules of rule set set. The schema is [{include, exclude?,
// params}]: include and params must be non-empty arrays of non-empty strings,
// no other key is allowed, and no pattern may use a "label.*" host.
//
// A rule's ID is min(include), plus " -" + min(exclude) when exclude is
// non-empty; colliding IDs get "#k" (k >= 2) in file order. Params are stored
// as written; the engine matches them canonically.
//
// Every problem is reported, as an errors.Join of *rule.Error (RuleID
// "#<index>" until an ID can be formed). On error the rules are nil: an
// element is never skipped.
func CompileStrip(set string, data []byte) ([]rule.Rule, error) {
	l := &errList{set: set}
	v, err := decodeStrict(data)
	if err != nil {
		l.add("", "", "%v", err)
		return nil, l.err()
	}
	elems, ok := v.([]any)
	if !ok {
		l.add("", "", "want array of rules, have %s", kind(v))
		return nil, l.err()
	}
	out := make([]rule.Rule, 0, len(elems))
	ids := newIDs()
	for i, e := range elems {
		id := "#" + strconv.Itoa(i)
		m, ok := l.object(id, "", e, "include", "exclude", "params")
		if !ok {
			continue
		}
		n := len(l.errs)
		include := l.patterns(id, m, "include", true)
		exclude := l.patterns(id, m, "exclude", false)
		if len(l.errs) == n {
			key := slices.Min(include)
			if len(exclude) > 0 {
				key += " -" + slices.Min(exclude)
			}
			id = ids.next(key)
		}
		params := l.strs(id, m, "params", true)
		out = append(out, rule.Rule{
			ID:      id,
			Action:  rule.Strip,
			Include: include,
			Exclude: nilIfEmpty(exclude),
			Params:  params,
		})
	}
	ids.check(l)
	if len(l.errs) > 0 {
		return nil, l.err()
	}
	return out, nil
}

// ids assigns rule IDs: a key's first use is the key itself, later uses get
// "#k" (k >= 2) in call order.
type ids struct {
	uses map[string]int
	all  []string
}

func newIDs() *ids { return &ids{uses: map[string]int{}} }

func (s *ids) next(key string) string {
	s.uses[key]++
	id := key
	if n := s.uses[key]; n > 1 {
		id += "#" + strconv.Itoa(n)
	}
	s.all = append(s.all, id)
	return id
}

// check reports an ID that occurs twice after suffixing, which happens only
// when a key itself ends in "#k" and equals an earlier suffixed key.
func (s *ids) check(l *errList) {
	seen := make(map[string]bool, len(s.all))
	for _, id := range s.all {
		if seen[id] {
			l.add(id, "", "duplicate rule ID %q after #k suffixing", id)
		}
		seen[id] = true
	}
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}
