package matchpattern

import (
	"strings"
	"testing"
)

// FuzzMatchPattern: Parse and Match never panic; "*.d" never matches "x"+d;
// the glob matcher agrees with a backtracking reference; UnescapeNormal only
// shrinks its input and returns it unchanged when nothing decodes.
func FuzzMatchPattern(f *testing.F) {
	for _, s := range [][2]string{
		{"*://*.example.com/*", "/a?b=1"},
		{"*://*.amazon.*/dp/*", "/dp/B0?tag=x"},
		{"https://x.example:443/a*b*c", "/aXbYc"},
		{"*://*/*viewtopic.php*", "/auth?next=/viewtopic.php"},
		{"*://www.tkqlhce.com/click-", "/click-1-2"},
		{"*://x/foo/*", "/foo"},
		{"*://x/%41%2A*", "/A%2Fb%2541"},
		{"*://[::1]:8080/*", "/"},
		{"<all_urls>", "/"},
		{"*://*.Kelvin.example/*", "/"},
	} {
		f.Add(s[0], "www.example.com", s[1], "example.com", false)
	}
	f.Add("*://*.1/*", "[::1]", "/", "", true)
	f.Add("*://a*b*a*b/*", "x", "/ababababab", "", false)
	f.Fuzz(func(t *testing.T, pattern, host, path, site string, isIP bool) {
		for _, scheme := range []string{"http", "https"} {
			tg := &Target{Scheme: scheme, Host: host, IsIP: isIP, Port: "443", Site: site}
			tg.SetPath(path)
			if m, err := Parse(pattern, true); err == nil {
				m.Match(tg)
				if m.String() != pattern {
					t.Errorf("String() = %q, want %q", m.String(), pattern)
				}
			}
		}

		if m, err := Parse("*://*."+host+"/*", false); err == nil {
			if kind, d := m.Host(); kind == HostSuffix {
				tg := &Target{Scheme: "https", Host: "x" + d, Port: "443", Site: site}
				tg.SetPath(path)
				if m.Match(tg) {
					t.Errorf("*.%s matched host x%s", d, d)
				}
			}
		}

		if got, want := newGlob(pattern).match(path), refGlob(path, pattern); got != want {
			t.Errorf("glob %q vs %q = %v, reference %v", pattern, path, got, want)
		}

		u := UnescapeNormal(path)
		if len(u) > len(path) {
			t.Errorf("UnescapeNormal(%q) grew", path)
		}
		if !strings.Contains(path, "%") && u != path {
			t.Errorf("UnescapeNormal(%q) = %q without escapes", path, u)
		}
	})
}

// refGlob is a dynamic-programming '*'-only glob: dp[j] says whether the
// processed text prefix matches pat[:j].
func refGlob(text, pat string) bool {
	dp, next := make([]bool, len(pat)+1), make([]bool, len(pat)+1)
	dp[0] = true
	for j := 1; j <= len(pat) && pat[j-1] == '*'; j++ {
		dp[j] = true
	}
	for i := 0; i < len(text); i++ {
		next[0] = false
		for j := 1; j <= len(pat); j++ {
			if pat[j-1] == '*' {
				next[j] = next[j-1] || dp[j]
			} else {
				next[j] = dp[j-1] && pat[j-1] == text[i]
			}
		}
		dp, next = next, dp
	}
	return dp[len(pat)]
}

func TestRefGlobAgrees(t *testing.T) {
	for _, c := range []struct{ text, pat string }{
		{"", ""}, {"", "*"}, {"a", ""}, {"abc", "a*c"}, {"abc", "*b*"}, {"ab", "a*b*"},
		{"aXbYc", "a*b*c"}, {"abab", "a*b*a*b"}, {"aba", "a*ba"}, {"ab", "a*ba"},
		{"mississippi", "m*iss*ppi"}, {"mississippi", "m*iss*x"}, {"aaa", "a*a*a*a"},
	} {
		if got, want := newGlob(c.pat).match(c.text), refGlob(c.text, c.pat); got != want {
			t.Errorf("glob %q vs %q = %v, reference %v", c.pat, c.text, got, want)
		}
	}
}
