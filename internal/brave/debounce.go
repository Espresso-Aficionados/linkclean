package brave

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"

	"github.com/Espresso-Aficionados/linkclean/internal/rule"
)

// DebounceSet is the rule-set name of the compiled debounce rules.
const DebounceSet = "brave/debounce"

// DebounceStats is the include-instance accounting of design §4.5.3 and the
// rules-compile gate (§11.3). On current data: 48 rules, 190 instances, 49
// compiled rules over 139 instances, 51 instances skipped.
type DebounceStats struct {
	Rules             int      // upstream elements
	Instances         int      // upstream include instances
	CompiledRules     int      // Unwrap rules emitted
	CompiledInstances int      // include instances in emitted rules
	SkippedInstances  int      // instances classed off, or in an uncompiled element
	Stale             []string // classified patterns no longer upstream, sorted (a note, not an error)
}

// debounce.json actions. Only "redirect" without pref or
// redirect_url_template compiles in v1; the others must be classed off.
var debounceActions = []string{"redirect", "base64,redirect", "regex-path", "regex-path-template"}

// Classes of debounce-classes.json.
const (
	classAffiliate = "affiliate"
	classUnwrap    = "unwrap"
	classOff       = "off"
)

// networkRE is Rule.Network's charset (design §3), checked here so the error
// names the classification entry.
var networkRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 .&'()/-]{0,39}$`)

type debounceElem struct {
	include, exclude []string
	action, param    string
	prependScheme    string
	pref, template   bool // key present
}

// compiled reports whether v1 compiles the element (§4.5.3).
func (e *debounceElem) compiled() bool {
	return e.action == "redirect" && !e.pref && !e.template
}

// why describes what keeps an uncompiled element from compiling.
func (e *debounceElem) why() string {
	switch {
	case e.action != "redirect":
		return fmt.Sprintf("action %q", e.action)
	case e.pref:
		return `"pref"`
	}
	return `"redirect_url_template"`
}

// class is one debounce-classes.json entry.
type class struct {
	class, network, idParam, idPath string
}

// CompileDebounce compiles debounce.json through the classification table
// rules/overlay/debounce-classes.json (design §4.5.3) into Unwrap rules of set
// "brave/debounce", tagged "debounce".
//
// Each element's includes are grouped by (class, network, locator) in file
// order; each group is one rule, Affiliate iff the class is affiliate, with
// ID min(include of the group) and the "#k" collision suffix across the file.
// An include instance classed off, or in an element v1 does not compile, is
// skipped. Errors: an unclassified include; a non-off class on an instance of
// an uncompiled element, unless the same pattern is in a compiled element; any
// schema violation of either file; unbalanced accounting. Errors are an
// errors.Join of *rule.Error; on error the rules are nil, but stats are
// filled as far as the input allowed.
func CompileDebounce(debounce, classes []byte) ([]rule.Rule, DebounceStats, error) {
	l := &errList{set: DebounceSet}
	var st DebounceStats
	elems := l.debounceElems(debounce)
	cls := l.classes(classes)
	st.Rules = len(elems)
	if len(l.errs) > 0 {
		return nil, st, l.err()
	}

	inCompiled := map[string]bool{}
	for _, e := range elems {
		if e.compiled() {
			for _, p := range e.include {
				inCompiled[p] = true
			}
		}
	}

	var out []rule.Rule
	ids := newIDs()
	upstream := map[string]bool{}
	for i, e := range elems {
		id := "#" + strconv.Itoa(i)
		groups := map[class][]string{} // by (class, network, locator)
		var order []class
		for j, p := range e.include {
			st.Instances++
			upstream[p] = true
			c, ok := cls[p]
			switch {
			case !ok:
				l.add(id, fmt.Sprintf("include[%d]", j), "unclassified pattern %q: add it to debounce-classes.json", p)
				continue
			case !e.compiled():
				if c.class != classOff && !inCompiled[p] {
					l.add(id, fmt.Sprintf("include[%d]", j), "%q classed %s but %s is not compiled in v1", p, c.class, e.why())
					continue
				}
				st.SkippedInstances++
				continue
			case c.class == classOff:
				st.SkippedInstances++
				continue
			}
			if _, seen := groups[c]; !seen {
				order = append(order, c)
			}
			groups[c] = append(groups[c], p)
		}
		for _, k := range order {
			inc := groups[k]
			r := rule.Rule{
				ID:        ids.next(slices.Min(inc)),
				Action:    rule.Unwrap,
				Affiliate: k.class == classAffiliate,
				Tags:      []string{"debounce"},
				Include:   inc,
				Exclude:   nilIfEmpty(slices.Clone(e.exclude)),
				Unwrap:    &rule.UnwrapSpec{QueryParams: []string{e.param}, PrependScheme: e.prependScheme},
			}
			if r.Affiliate {
				r.Network, r.IDParam, r.IDPathRegexp = k.network, k.idParam, k.idPath
			}
			out = append(out, r)
			st.CompiledRules++
			st.CompiledInstances += len(inc)
		}
	}
	ids.check(l)
	for _, p := range slices.Sorted(maps.Keys(cls)) {
		if !upstream[p] {
			st.Stale = append(st.Stale, p)
		}
	}
	if len(l.errs) == 0 && st.CompiledInstances+st.SkippedInstances != st.Instances {
		l.add("", "", "accounting: %d compiled + %d skipped instances != %d upstream", st.CompiledInstances, st.SkippedInstances, st.Instances)
	}
	if len(l.errs) > 0 {
		return nil, st, l.err()
	}
	return out, st, nil
}

// debounceElems decodes debounce.json strictly: [{include, exclude, action,
// param, prepend_scheme?, pref?, redirect_url_template?}] (§11.3
// rules-schema).
func (l *errList) debounceElems(data []byte) []debounceElem {
	v, err := decodeStrict(data)
	if err != nil {
		l.add("", "debounce.json", "%v", err)
		return nil
	}
	a, ok := v.([]any)
	if !ok {
		l.add("", "debounce.json", "want array of rules, have %s", kind(v))
		return nil
	}
	out := make([]debounceElem, len(a))
	for i, ev := range a {
		id := "#" + strconv.Itoa(i)
		m, ok := l.object(id, "", ev, "include", "exclude", "action", "param", "prepend_scheme", "pref", "redirect_url_template")
		if !ok {
			continue
		}
		e := &out[i]
		e.include = l.patterns(id, m, "include", true)
		if _, ok := m["exclude"]; !ok {
			l.add(id, "exclude", "missing")
		}
		e.exclude = l.patterns(id, m, "exclude", false)
		e.action, _ = l.str(id, "", m, "action", true)
		if e.action != "" && !slices.Contains(debounceActions, e.action) {
			l.add(id, "action", "unknown action %q", e.action)
		}
		e.param, _ = l.str(id, "", m, "param", true)
		var present bool
		if e.prependScheme, present = l.str(id, "", m, "prepend_scheme", false); present &&
			e.prependScheme != "" && e.prependScheme != "http" && e.prependScheme != "https" {
			l.add(id, "prepend_scheme", "want \"http\" or \"https\", have %q", e.prependScheme)
		}
		_, e.pref = l.str(id, "", m, "pref", false)
		_, e.template = l.str(id, "", m, "redirect_url_template", false)
	}
	return out
}

// classes decodes debounce-classes.json strictly (§11.3 rules-schema):
// {"schema": 1, "patterns": {pattern: {class, network?, idParam?, idPath?,
// ref?, why?}}}, where network is present iff the class is affiliate, at most
// one locator (idParam or idPath) is set, only on affiliate and with a ref,
// why is set on every off entry and every affiliate entry without a locator,
// and idPath compiles with exactly one capture group.
func (l *errList) classes(data []byte) map[string]class {
	const file = "debounce-classes.json"
	v, err := decodeStrict(data)
	if err != nil {
		l.add("", file, "%v", err)
		return nil
	}
	top, ok := l.object("", file, v, "schema", "patterns")
	if !ok {
		return nil
	}
	if s, ok := top["schema"].(float64); !ok || s != 1 {
		l.add("", file+".schema", "want 1, have %v", top["schema"])
	}
	pm, ok := top["patterns"].(map[string]any)
	if !ok {
		l.add("", file+".patterns", "want object, have %s", kind(top["patterns"]))
		return nil
	}
	out := make(map[string]class, len(pm))
	for _, p := range slices.Sorted(maps.Keys(pm)) {
		f := fmt.Sprintf("%s[%q]", file, p)
		m, ok := l.object("", f, pm[p], "class", "network", "idParam", "idPath", "ref", "why")
		if !ok {
			continue
		}
		var c class
		var hasNet, hasRef, hasWhy bool
		var hasParam, hasPath bool
		c.class, _ = l.str("", f, m, "class", true)
		c.network, hasNet = l.str("", f, m, "network", false)
		c.idParam, hasParam = l.str("", f, m, "idParam", false)
		c.idPath, hasPath = l.str("", f, m, "idPath", false)
		_, hasRef = l.str("", f, m, "ref", false)
		_, hasWhy = l.str("", f, m, "why", false)
		locator := hasParam || hasPath

		aff := c.class == classAffiliate
		switch c.class {
		case classAffiliate, classUnwrap, classOff, "":
		default:
			l.add("", f+".class", "want affiliate, unwrap or off, have %q", c.class)
		}
		switch {
		case aff && !hasNet:
			l.add("", f+".network", "missing: required for class affiliate")
		case !aff && hasNet:
			l.add("", f+".network", "set on class %q: only affiliate entries name a network", c.class)
		case aff && !networkRE.MatchString(c.network):
			l.add("", f+".network", "%q does not match %s", c.network, networkRE)
		}
		switch {
		case hasParam && hasPath:
			l.add("", f, "both idParam and idPath set: at most one locator")
		case locator && !aff:
			l.add("", f, "locator on class %q: only affiliate entries have one", c.class)
		case locator && !hasRef:
			l.add("", f+".ref", "missing: required with a locator")
		}
		if hasPath {
			if re, err := regexp.Compile(c.idPath); err != nil {
				l.add("", f+".idPath", "%v", err)
			} else if n := re.NumSubexp(); n != 1 {
				l.add("", f+".idPath", "want exactly one capture group, have %d", n)
			}
		}
		if !hasWhy && (c.class == classOff || (aff && !locator)) {
			l.add("", f+".why", "missing: required for class off and for affiliate without a locator")
		}
		out[p] = c
	}
	return out
}
