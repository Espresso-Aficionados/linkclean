package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Espresso-Aficionados/linkclean"
)

// Error texts of -explain and -json error records.
const (
	errLineTooLong = "line too long"
	errNotUTF8     = "not valid UTF-8"
)

// The vocabulary of -explain and -json: the golden files' words. A value
// outside a table prints "unknown"; TestVocabularyComplete keeps that a
// runtime fallback only.
var (
	skipNames = [...]string{
		linkclean.SkipScheme:      "scheme",
		linkclean.SkipUnparseable: "unparseable",
		linkclean.SkipAmbiguous:   "ambiguous",
		linkclean.SkipTooLong:     "toolong",
		linkclean.SkipArchive:     "archive",
		linkclean.SkipRoundCap:    "roundcap",
	}
	partNames = [...]string{
		linkclean.PartPath:     "path",
		linkclean.PartQuery:    "query",
		linkclean.PartFragment: "fragment",
		linkclean.PartNested:   "nested",
	}
	keepNames = [...]string{
		linkclean.KeptByRule:      "rule",
		linkclean.KeptAffiliate:   "affiliate",
		linkclean.KeptSignedGuard: "signed",
	}
)

func word(names []string, i int) string {
	if i < len(names) && names[i] != "" {
		return names[i]
	}
	return "unknown"
}

// skipName is "" for NotSkipped.
func skipName(s linkclean.SkipReason) string {
	if s == linkclean.NotSkipped {
		return ""
	}
	return word(skipNames[:], int(s))
}

func partName(p linkclean.Part) string       { return word(partNames[:], int(p)) }
func keepName(k linkclean.KeepReason) string { return word(keepNames[:], int(k)) }

func catName(c linkclean.Category) string {
	b, err := c.MarshalText()
	if err != nil {
		return "unknown"
	}
	return string(b)
}

// appendExplain appends the -explain record of r at position n. Every string
// taken from the Result is strconv.Quote'd, so the record holds no control,
// bidi or other non-printable character and no invalid UTF-8. It never
// includes r.Input, Removal.Value or Text, or Hop.From or To.
func appendExplain(b []byte, n int, r linkclean.Result) []byte {
	b = append(b, "line "...)
	b = strconv.AppendInt(b, int64(n), 10)
	b = append(b, "\nout "...)
	b = strconv.AppendQuote(b, r.URL)
	b = append(b, '\n')
	if r.Changed {
		b = append(b, "changed\n"...)
	}
	if r.Skipped != linkclean.NotSkipped {
		b = append(b, "skipped "...)
		b = append(b, skipName(r.Skipped)...)
		b = append(b, '\n')
	}
	if r.UnwrapAbandoned {
		b = append(b, "abandoned\n"...)
	}
	if r.Signed {
		b = append(b, "signed\n"...)
	}
	field := func(b []byte, key, val string) []byte {
		b = append(b, ' ')
		b = append(b, key...)
		b = append(b, '=')
		return strconv.AppendQuote(b, val)
	}
	affiliate := func(b []byte, c linkclean.Category, bf linkclean.Beneficiary) []byte {
		if c != linkclean.CatAffiliate {
			return b
		}
		b = field(b, "network", bf.Network)
		return field(b, "id", bf.ID)
	}
	for _, h := range r.Hops {
		b = append(b, "hop category="...)
		b = append(b, catName(h.Category)...)
		b = field(b, "rule", h.Rule)
		b = field(b, "host", h.FromHost)
		b = affiliate(b, h.Category, h.Beneficiary)
		b = append(b, '\n')
	}
	for _, m := range r.Removed {
		b = append(b, "removed part="...)
		b = append(b, partName(m.Part)...)
		b = append(b, " category="...)
		b = append(b, catName(m.Category)...)
		b = field(b, "name", m.Name)
		if m.Part == linkclean.PartNested {
			b = field(b, "in", m.NestedIn)
		}
		b = field(b, "rule", m.Rule)
		b = affiliate(b, m.Category, m.Beneficiary)
		b = append(b, '\n')
	}
	for _, k := range r.Kept {
		b = append(b, "kept reason="...)
		b = append(b, keepName(k.Reason)...)
		b = field(b, "name", k.Name)
		if k.Rule != "" {
			b = field(b, "rule", k.Rule)
		}
		b = append(b, '\n')
	}
	return append(b, '\n')
}

// appendExplainError appends the -explain record of an input at position n
// that could not be reported.
func appendExplainError(b []byte, n int, msg string) []byte {
	b = append(b, "line "...)
	b = strconv.AppendInt(b, int64(n), 10)
	b = append(b, "\nerror "...)
	b = strconv.AppendQuote(b, msg)
	return append(b, "\n\n"...)
}

// The -json schema. Every object has line and error; error is "" in a
// record and non-empty in an error object, which has no other fields. Fields
// may be added in a minor release.
type (
	jsonRecord struct {
		Line      int           `json:"line"`
		Error     string        `json:"error"`
		URL       string        `json:"url"`
		Changed   bool          `json:"changed"`
		Skipped   string        `json:"skipped"`
		Abandoned bool          `json:"abandoned"`
		Signed    bool          `json:"signed"`
		Hops      []jsonHop     `json:"hops"`
		Removed   []jsonRemoval `json:"removed"`
		Kept      []jsonKept    `json:"kept"`
	}
	jsonHop struct {
		Category string `json:"category"`
		Rule     string `json:"rule"`
		FromHost string `json:"fromHost"`
		Network  string `json:"network"`
		ID       string `json:"id"`
	}
	jsonRemoval struct {
		Part     string `json:"part"`
		Category string `json:"category"`
		Name     string `json:"name"`
		NestedIn string `json:"nestedIn"`
		Rule     string `json:"rule"`
		Network  string `json:"network"`
		ID       string `json:"id"`
	}
	jsonKept struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
		Rule   string `json:"rule"`
	}
	jsonError struct {
		Line  int    `json:"line"`
		Error string `json:"error"`
	}
)

// toRecord converts r to its -json record. ok is false when r.URL is not
// valid UTF-8: the record cannot carry it faithfully, so the caller writes an
// error object instead. Derived text fields never fail: invalid bytes become
// %XX (pctInvalid).
func toRecord(n int, r linkclean.Result) (rec jsonRecord, ok bool) {
	if !utf8.ValidString(r.URL) {
		return jsonRecord{}, false
	}
	rec = jsonRecord{
		Line: n, URL: r.URL, Changed: r.Changed, Skipped: skipName(r.Skipped),
		Abandoned: r.UnwrapAbandoned, Signed: r.Signed,
		Hops: []jsonHop{}, Removed: []jsonRemoval{}, Kept: []jsonKept{},
	}
	for _, h := range r.Hops {
		network, id := affiliateFields(h.Category, h.Beneficiary)
		rec.Hops = append(rec.Hops, jsonHop{
			Category: catName(h.Category), Rule: pctInvalid(h.Rule), FromHost: pctInvalid(h.FromHost),
			Network: network, ID: id,
		})
	}
	for _, m := range r.Removed {
		network, id := affiliateFields(m.Category, m.Beneficiary)
		var in string
		if m.Part == linkclean.PartNested {
			in = pctInvalid(m.NestedIn)
		}
		rec.Removed = append(rec.Removed, jsonRemoval{
			Part: partName(m.Part), Category: catName(m.Category), Name: pctInvalid(m.Name),
			NestedIn: in, Rule: pctInvalid(m.Rule), Network: network, ID: id,
		})
	}
	for _, k := range r.Kept {
		rec.Kept = append(rec.Kept, jsonKept{Name: pctInvalid(k.Name), Reason: keepName(k.Reason), Rule: pctInvalid(k.Rule)})
	}
	return rec, true
}

// affiliateFields returns the network and ID of a CatAffiliate entry, "" for
// any other category.
func affiliateFields(c linkclean.Category, b linkclean.Beneficiary) (network, id string) {
	if c != linkclean.CatAffiliate {
		return "", ""
	}
	return pctInvalid(b.Network), pctInvalid(b.ID)
}

// pctInvalid writes each byte of s that is not part of valid UTF-8 as %XX
// (uppercase hex).
func pctInvalid(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	const hex = "0123456789ABCDEF"
	b := make([]byte, 0, len(s)+8)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b = append(b, '%', hex[s[i]>>4], hex[s[i]&15])
		} else {
			b = append(b, s[i:i+size]...)
		}
		i += size
	}
	return string(b)
}

// appendJSON appends the -json line for r at position n: the record, or the
// "not valid UTF-8" error object with ok false.
func appendJSON(b []byte, n int, r linkclean.Result) (out []byte, ok bool, err error) {
	rec, ok := toRecord(n, r)
	if !ok {
		return appendJSONError(b, n, errNotUTF8), false, nil
	}
	out, err = appendEncoded(b, rec)
	return out, true, err
}

// appendJSONError appends the -json error object for position n.
func appendJSONError(b []byte, n int, msg string) []byte {
	out, err := appendEncoded(b, jsonError{Line: n, Error: msg})
	if err != nil {
		panic(err) // an int and a constant string always encode
	}
	return out
}

// appendEncoded appends v as one JSON line, with '&', '<' and '>' left as is
// and every non-printable rune at or above U+0080 written as a \u escape
// (encoding/json already escapes C0, U+2028 and U+2029). The rewrite keeps
// the JSON value: every string in the line is valid UTF-8 and the JSON
// structure itself is ASCII.
func appendEncoded(b []byte, v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return b, err
	}
	const hex = "0123456789abcdef"
	esc := func(b []byte, r rune) []byte {
		return append(b, '\\', 'u', hex[r>>12&15], hex[r>>8&15], hex[r>>4&15], hex[r&15])
	}
	for s := buf.Bytes(); len(s) > 0; {
		r, size := utf8.DecodeRune(s)
		switch {
		case r < utf8.RuneSelf || strconv.IsPrint(r):
			b = append(b, s[:size]...)
		case r > 0xFFFF:
			hi, lo := utf16.EncodeRune(r)
			b = esc(esc(b, hi), lo)
		default:
			b = esc(b, r)
		}
		s = s[size:]
	}
	return b, nil
}
