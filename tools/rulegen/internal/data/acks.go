package data

import (
	"fmt"
	"strings"
	"time"
)

// Acks is rules/acks.json, schema 1: human-owned acks and holds, outside
// the sync write set. rulegen sync never reads it.
type Acks struct {
	Schema int    `json:"schema"`
	Acks   []Ack  `json:"acks"`
	Holds  []Hold `json:"holds"`
}

// Ack lets one gate metric pass for one vendored sha256 until Expires.
// Check "counts" and "golden" name a Metric (see AckMetrics) and its floor
// Min; check "risky" names a Scope and a Param instead.
type Ack struct {
	Check          string `json:"check"`
	Metric         string `json:"metric,omitempty"`
	Min            *int   `json:"min,omitempty"`
	Scope          string `json:"scope,omitempty"`
	Param          string `json:"param,omitempty"`
	VendoredSHA256 string `json:"vendored_sha256"`
	Expires        string `json:"expires"` // YYYY-MM-DD, inclusive, UTC
	Reason         string `json:"reason"`
	By             string `json:"by"`
}

// Hold keeps the vendored bytes with sha256 SHA256 of File (an upstream
// path of one of the three JSON files) off main until Expires.
type Hold struct {
	File    string `json:"file"`
	SHA256  string `json:"sha256"`
	Expires string `json:"expires"`
	Reason  string `json:"reason"`
	By      string `json:"by"`
}

// AckMetrics is the ack vocabulary: check → metric → true. A risky ack has
// no metric.
var AckMetrics = map[string]map[string]bool{
	"counts": {
		"clean-urls.rules": true, "clean-urls.params": true, "clean-urls.global_params": true, "clean-urls.scoped_params": true,
		"query-filter.rules": true, "query-filter.params": true, "query-filter.global_params": true, "query-filter.scoped_params": true,
		"union.params": true, "union.global_params": true, "union.scoped_params": true,
		"debounce.includes": true,
	},
	"golden": {"corpus.exact": true, "corpus.recall_removed": true},
	"risky":  {},
}

// acksJSON tracks the presence of the top-level keys.
type acksJSON struct {
	Schema *int    `json:"schema"`
	Acks   *[]Ack  `json:"acks"`
	Holds  *[]Hold `json:"holds"`
}

// ParseAcks decodes rules/acks.json strictly (no unknown or duplicate keys,
// schema 1, both lists present) and validates every entry. A decode error
// is the only error returned; otherwise each invalid entry yields one error
// and the result still holds every entry.
func ParseAcks(b []byte) (Acks, []error) {
	if err := checkDuplicateKeys(b); err != nil {
		return Acks{}, []error{fmt.Errorf("%s: %w", AcksPath, err)}
	}
	var j acksJSON
	if err := decodeStrict(b, &j); err != nil {
		return Acks{}, []error{fmt.Errorf("%s: %w", AcksPath, err)}
	}
	var a Acks
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: "+format, append([]any{AcksPath}, args...)...))
	}
	switch {
	case j.Schema == nil:
		bad("missing schema")
	case *j.Schema != 1:
		bad("schema %d, want 1", *j.Schema)
	default:
		a.Schema = 1
	}
	if j.Acks == nil {
		bad("missing acks")
	} else {
		a.Acks = *j.Acks
	}
	if j.Holds == nil {
		bad("missing holds")
	} else {
		a.Holds = *j.Holds
	}
	for i, k := range a.Acks {
		for _, p := range validateAck(k) {
			bad("acks[%d]: %s", i, p)
		}
	}
	for i, h := range a.Holds {
		for _, p := range validateHold(h) {
			bad("holds[%d]: %s", i, p)
		}
	}
	return a, errs
}

func validateAck(k Ack) []string {
	var ps []string
	metrics, ok := AckMetrics[k.Check]
	switch {
	case !ok:
		ps = append(ps, fmt.Sprintf("check %q is not one of counts, golden, risky", k.Check))
	case k.Check == "risky":
		if k.Scope == "" {
			ps = append(ps, "missing scope")
		}
		if k.Param == "" {
			ps = append(ps, "missing param")
		}
		if k.Metric != "" || k.Min != nil {
			ps = append(ps, "metric and min are not used by a risky ack")
		}
	default:
		if !metrics[k.Metric] {
			ps = append(ps, fmt.Sprintf("metric %q is not a %s metric", k.Metric, k.Check))
		}
		if k.Min == nil {
			ps = append(ps, "missing min")
		} else if *k.Min < 0 {
			ps = append(ps, "min is negative")
		}
		if k.Scope != "" || k.Param != "" {
			ps = append(ps, "scope and param are used only by a risky ack")
		}
	}
	if !isLowerHex(k.VendoredSHA256, 64) {
		ps = append(ps, fmt.Sprintf("vendored_sha256 %q is not 64 lowercase hex digits", k.VendoredSHA256))
	}
	return append(ps, validateCommon(k.Expires, k.Reason, k.By)...)
}

func validateHold(h Hold) []string {
	var ps []string
	if s, ok := SpecOf(h.File); !ok || s.Kind == KindLicense {
		ps = append(ps, fmt.Sprintf("file %q is not one of %s, %s, %s", h.File, Files[0].Upstream, Files[1].Upstream, Files[2].Upstream))
	}
	if !isLowerHex(h.SHA256, 64) {
		ps = append(ps, fmt.Sprintf("sha256 %q is not 64 lowercase hex digits", h.SHA256))
	}
	return append(ps, validateCommon(h.Expires, h.Reason, h.By)...)
}

func validateCommon(expires, reason, by string) []string {
	var ps []string
	if _, err := time.Parse(time.DateOnly, expires); err != nil {
		ps = append(ps, fmt.Sprintf("expires %q is not YYYY-MM-DD", expires))
	}
	if strings.TrimSpace(reason) == "" {
		ps = append(ps, "missing reason")
	}
	if strings.TrimSpace(by) == "" {
		ps = append(ps, "missing by")
	}
	return ps
}

// Expired reports whether an entry with this expires date has expired at
// now: expires is inclusive, so the entry lapses at 00:00 UTC on the day
// after. An unparseable date counts as expired, so it never satisfies a
// gate.
func Expired(expires string, now time.Time) bool {
	d, err := time.Parse(time.DateOnly, expires)
	if err != nil {
		return true
	}
	return !now.Before(d.AddDate(0, 0, 1))
}
