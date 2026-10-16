package query

import (
	"slices"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		q    string
		want []Segment
	}{
		{"", nil},
		{"a", []Segment{{Text: "a", Name: "a"}}},
		{"a=", []Segment{{Text: "a=", Name: "a", HasEq: true}}},
		{"=v", []Segment{{Text: "=v", Value: "v", HasEq: true}}},
		{"=k=v", []Segment{{Text: "=k=v", Value: "k=v", HasEq: true}}}, // DV-4: empty name
		{"a=1&b=2", []Segment{{Text: "a=1", Name: "a", Value: "1", HasEq: true}, {Text: "b=2", Name: "b", Value: "2", HasEq: true}}},
		{"&&", nil},
		{"&a&&b&", []Segment{{Text: "a", Name: "a"}, {Text: "b", Name: "b"}}},
		{"a=1;b=2", []Segment{{Text: "a=1;b=2", Name: "a", Value: "1;b=2", HasEq: true}}},
		{"u=https://x/?a=1", []Segment{{Text: "u=https://x/?a=1", Name: "u", Value: "https://x/?a=1", HasEq: true}}},
	}
	for _, tt := range tests {
		if got := Parse(nil, tt.q); !slices.Equal(got, tt.want) {
			t.Errorf("Parse(%q) = %+v, want %+v", tt.q, got, tt.want)
		}
	}
	// Parse appends.
	if got := Parse([]Segment{{Text: "x", Name: "x"}}, "y"); len(got) != 2 || got[1].Name != "y" {
		t.Errorf("Parse does not append: %+v", got)
	}
}

// A long query grows dst at most once, sized by its non-empty segments.
func TestParseGrowsOnce(t *testing.T) {
	for _, c := range []struct {
		q string
		n int
	}{
		{strings.Repeat("&", 8100), 0},
		{strings.Repeat("a=1&", 2025), 2025},
		{"&" + strings.Repeat("a&&", 100), 100},
	} {
		if allocs := testing.AllocsPerRun(10, func() { Parse(nil, c.q) }); allocs > 1 && !raceEnabled {
			t.Errorf("Parse(%.12q…): %v allocs, want <= 1", c.q, allocs)
		}
		if got := Parse(nil, c.q); len(got) != c.n {
			t.Errorf("Parse(%.12q…): %d segments, want %d", c.q, len(got), c.n)
		}
	}
}

func TestCanon(t *testing.T) {
	tests := []struct{ in, want string }{
		{"utm_source", "utm_source"},
		{"UTM_SOURCE", "utm_source"},
		{"utm%5Fsource", "utm_source"},
		{"utm%5fSource", "utm_source"},
		{"%41", "a"}, // decoded bytes are lowercased too
		{"%2541", "%41"},
		{"a+b", "a+b"},
		{"a%2Bb", "a+b"},
		{"a%20b", "a b"},
		{"%", "%"},
		{"%4", "%4"},
		{"%zz", "%zz"},
		{"%%41", "%a"},
		{"ÉtÉ", "ÉtÉ"}, // ASCII-only lowercasing
		{"%C3%89", "É"},
		{"\xff", "\xff"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := Canon(tt.in); got != tt.want {
			t.Errorf("Canon(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if got := string(AppendCanon([]byte("pre"), tt.in)); got != "pre"+tt.want {
			t.Errorf("AppendCanon(%q) = %q, want %q", tt.in, got, "pre"+tt.want)
		}
	}
}

func TestDecodeValue(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https%3A%2F%2Fexample.com%2Fa%2Bb", "https://example.com/a+b"},
		{"a+b", "a+b"}, // DV-12: '+' stays literal
		{"%2541", "%41"},
		{"100%", "100%"},
		{"%zz%4", "%zz%4"},
		{"%e9", "\xe9"},
		{"x%%41", "x%A"},
		{"%0A", "\n"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := DecodeValue(tt.in); got != tt.want {
			t.Errorf("DecodeValue(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestEncodeComponent(t *testing.T) {
	tests := []struct{ in, want string }{
		{"abcXYZ019-._~", "abcXYZ019-._~"},
		{"https://example.com/a?b=c&d", "https%3A%2F%2Fexample.com%2Fa%3Fb%3Dc%26d"},
		{"a+b c%", "a%2Bb%20c%25"},
		{"\x00\x7f\xff", "%00%7F%FF"},
		{"é", "%C3%A9"},
		{"", ""},
	}
	for _, tt := range tests {
		got := EncodeComponent(tt.in)
		if got != tt.want {
			t.Errorf("EncodeComponent(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if back := DecodeValue(got); back != tt.in {
			t.Errorf("DecodeValue(EncodeComponent(%q)) = %q", tt.in, back)
		}
	}
}

func TestIsFragmentParams(t *testing.T) {
	tests := []struct {
		f    string
		want bool
	}{
		{"utm_source=x", true},
		{"Echobox=1234", true},
		{"xtor=RSS-3208", true},
		{"gid=123", true},
		{"utm_source=x&tab=2", true},
		{"a[0]=1&b.c-d%5F=2", true},
		{"a=1&&b", true}, // "b" has no '=' but its name is fine
		{"a=1&", true},
		{"", false},
		{"section", false},
		{"p1012345", false},
		{"/route?x=1", false},
		{"route?x=1", false},
		{"a=b/c", false},
		{":~:text=foo", false},
		{"a b=1", false},
		{"=1", false},
		{"a=1&=2", false},
		{"a+b=1", false},
		{"é=1", false},
	}
	for _, tt := range tests {
		if got := IsFragmentParams(tt.f); got != tt.want {
			t.Errorf("IsFragmentParams(%q) = %v, want %v", tt.f, got, tt.want)
		}
	}
}

func TestAllocs(t *testing.T) {
	const q = "utm_source=newsletter&UTM_Medium=email&id=12345&utm%5Fcampaign=x&&b"
	dst := make([]Segment, 0, 16)
	if n := testing.AllocsPerRun(100, func() { dst = Parse(dst[:0], q) }); n != 0 {
		t.Errorf("Parse allocs = %v, want 0", n)
	}
	m := map[string]bool{"utm_campaign": true}
	var buf [64]byte
	hit := false
	if n := testing.AllocsPerRun(100, func() {
		for _, s := range dst {
			hit = m[string(AppendCanon(buf[:0], s.Name))] || hit
		}
	}); n != 0 {
		t.Errorf("AppendCanon lookup allocs = %v, want 0", n)
	}
	if !hit {
		t.Error("AppendCanon lookup missed utm%5Fcampaign")
	}
	if n := testing.AllocsPerRun(100, func() {
		_ = Canon("utm_source")
		_ = DecodeValue("plain-value")
		_ = EncodeComponent("plain-value")
	}); n != 0 {
		t.Errorf("unchanged Canon/DecodeValue/EncodeComponent allocs = %v, want 0", n)
	}
}

// FuzzQuery checks that tokenizing keeps every non-empty segment and that
// DecodeValue inverts EncodeComponent (§14).
func FuzzQuery(f *testing.F) {
	for _, s := range []string{"", "a=1&b=2", "&&", "=k=v", "a=%2541&B=%zz", "u=https%3A%2F%2Fx%2F%3Fa%3D1", "é=\xff;x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, q string) {
		segs := Parse(nil, q)
		texts := make([]string, len(segs))
		for i, s := range segs {
			texts[i] = s.Text
			want := s.Name
			if s.HasEq {
				want += "=" + s.Value
			}
			if want != s.Text || strings.IndexByte(s.Text, '&') >= 0 || strings.IndexByte(s.Name, '=') >= 0 {
				t.Fatalf("segment %+v of %q is inconsistent", s, q)
			}
		}
		var want []string
		for _, p := range strings.Split(q, "&") {
			if p != "" {
				want = append(want, p)
			}
		}
		if !slices.Equal(texts, want) && len(texts)+len(want) > 0 {
			t.Fatalf("segments %q, want %q", texts, want)
		}
		if got := DecodeValue(EncodeComponent(q)); got != q {
			t.Fatalf("DecodeValue(EncodeComponent(%q)) = %q", q, got)
		}
		if got, want := Canon(q), string(AppendCanon(nil, q)); got != want {
			t.Fatalf("Canon(%q) = %q, AppendCanon = %q", q, got, want)
		}
		if got, want := Canon(q), strings.ToLower(DecodeValue(q)); asciiOnly(DecodeValue(q)) && got != want {
			t.Fatalf("Canon(%q) = %q, want %q", q, got, want)
		}
	})
}

func asciiOnly(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func BenchmarkAppendCanon(b *testing.B) {
	var buf [64]byte
	b.ReportAllocs()
	for b.Loop() {
		AppendCanon(buf[:0], "UTM%5FSource")
	}
}

func BenchmarkParse(b *testing.B) {
	const q = "utm_source=newsletter&utm_medium=email&id=12345&utm_campaign=x"
	dst := make([]Segment, 0, 16)
	b.ReportAllocs()
	for b.Loop() {
		dst = Parse(dst[:0], q)
	}
}

func TestHasHTTPScheme(t *testing.T) {
	for v, want := range map[string]bool{
		"https://a":       true,
		"http://a":        true,
		"HTTPS%3A%2F%2Fa": true,
		"http%3A//a":      true,
		"https%253A":      false,
		"ftp://a":         false,
		"http:/":          false,
		"":                false,
		"httpx://":        false,
	} {
		if got := HasHTTPScheme(v); got != want {
			t.Errorf("HasHTTPScheme(%q) = %v", v, got)
		}
	}
}
