package rawurl

import (
	"strings"
	"testing"
)

func TestRequestForm(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://t.co/x", "https://t.co/x"},
		{"https://t.co/x#frag", "https://t.co/x"},
		{"https://t.co/x#", "https://t.co/x"},
		{"HTTP://T.CO/x?a=1#f", "http://t.co/x?a=1"},
		{"HtTpS://t.co/x", "https://t.co/x"},
		{"https://T.CO./x", "https://t.co/x"},
		{"https://ｔ.co/x", "https://t.co/x"},
		{"https://t.co", "https://t.co"},
		{"https://t.co#f", "https://t.co"},
		{"https://t.co?", "https://t.co?"},
		{"https://t.co/x?", "https://t.co/x?"},
		{"https://t.co/x?#f", "https://t.co/x?"},
		{"https://t.co:443/x", "https://t.co:443/x"},
		{"http://t.co:80/x", "http://t.co:80/x"},
		{"https://t.co:0443/x", "https://t.co:0443/x"},
		{"https://t.co:/x", "https://t.co:/x"},
		{"http://::/x#f", "http://::/x"},
		{"https://[::1]:/x", "https://[::1]:/x"},
		{"https://User:Pw@T.co:8080/x", "https://User:Pw@t.co:8080/x"},
		{"https://a@b@T.CO/x", "https://a@b@t.co/x"},
		{"https://@t.co/x", "https://@t.co/x"},
		{"https://[2001:DB8::1]:443/x#f", "https://[2001:db8::1]:443/x"},
		{"https://t.co/A%2Fb?Q=%41&x#y", "https://t.co/A%2Fb?Q=%41&x"},
		{"https://bücher.example/x", "https://xn--bcher-kva.example/x"},
		{"https://t.co/a\\b", "https://t.co/a\\b"},
	}
	for _, tt := range tests {
		u, skip := Parse(tt.in)
		if skip != OK {
			t.Fatalf("Parse(%q) = %v", tt.in, skip)
		}
		if got := u.RequestForm(); got != tt.want {
			t.Errorf("RequestForm(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRequestFormNoAlloc(t *testing.T) {
	for _, s := range []string{"https://t.co/x?a=1", "https://u@t.co:443", "http://t.co?"} {
		u, _ := Parse(s)
		if n := testing.AllocsPerRun(100, func() { _ = u.RequestForm() }); n != 0 {
			t.Errorf("RequestForm(%q) allocates %v times", s, n)
		}
		if got := u.RequestForm(); got != s {
			t.Errorf("RequestForm(%q) = %q", s, got)
		}
	}
}

// FuzzRequestForm checks RequestForm's properties for every input Parse
// accepts: the result parses, has no fragment and the same host view, and
// RequestForm is idempotent.
func FuzzRequestForm(f *testing.F) {
	for _, s := range []string{
		"https://t.co/x#a",
		"HTTP://T.CO/x",
		"https://T.CO./x?",
		"https://ｔ.co/x",
		"https://u:p@t.co:0443/x?q#f",
		"https://t.co:/x",
		"https://[::FFFF:127.0.0.1]:80/x",
		"https://web.archive.org。/web/x",
		"https://bücher.example",
		"http://0x7f.1/",
		"https://a@b@c.example?#",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		u, skip := Parse(s)
		if skip != OK {
			return
		}
		rf := u.RequestForm()
		if strings.IndexByte(rf, '#') >= 0 {
			t.Fatalf("RequestForm(%q) = %q has a '#'", s, rf)
		}
		v, skip := Parse(rf)
		if skip != OK {
			t.Fatalf("Parse(RequestForm(%q)) = %v (form %q)", s, skip, rf)
		}
		if v.HasFragment {
			t.Fatalf("RequestForm(%q) = %q has a fragment", s, rf)
		}
		if v.Host != u.Host || v.Port != u.Port || v.Path != u.Path || v.HasQuery != u.HasQuery || v.Query != u.Query {
			t.Fatalf("RequestForm(%q) = %q changed a part: %+v, want %+v", s, rf, v, u)
		}
		if again := v.RequestForm(); again != rf {
			t.Fatalf("RequestForm not idempotent on %q: %q then %q", s, rf, again)
		}
	})
}
