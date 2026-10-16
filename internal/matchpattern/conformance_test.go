// The vectors in this file are re-derived from the cases in Chromium's
// extensions/common/url_pattern_unittest.cc, restricted to the subset this
// package implements. No test code is copied. Chromium's notice:
//
// Copyright 2012 The Chromium Authors
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are
// met:
//
//   - Redistributions of source code must retain the above copyright
//     notice, this list of conditions and the following disclaimer.
//   - Redistributions in binary form must reproduce the above copyright
//     notice, this list of conditions and the following disclaimer in the
//     documentation and/or other materials provided with the distribution.
//   - Neither the name of Google LLC nor the names of its contributors may
//     be used to endorse or promote products derived from this software
//     without specific prior written permission.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
// "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
// LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
// A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
// OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
// SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
// LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
// DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
// THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
// (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
// OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

package matchpattern

import "testing"

// Parse results for the subset (Chromium's ParseResult cases that apply to
// http/https patterns).
func TestChromiumParse(t *testing.T) {
	bad := []string{
		"http",                 // missing scheme separator
		"http:",                // missing scheme separator
		"http:/",               // missing scheme separator
		"about://",             // invalid scheme
		"ftp://foo/*",          // invalid scheme (outside the subset)
		"file:///foo",          // invalid scheme (outside the subset)
		"HTTP://foo/*",         // scheme is case-sensitive
		"http://",              // empty host
		"http:///",             // empty host
		"http://:1234/",        // empty host
		"http://*./",           // empty host after "*."
		"http://foo",           // empty path
		"http://*foo/bar",      // invalid host wildcard
		"http://foo.*.bar/baz", // invalid host wildcard
		"http://fo.*.ba:123/baz",
		"http:/bar",          // wrong scheme separator
		"http://bar:abc/*",   // invalid port
		"http://bar:/*",      // invalid port
		"http://bar:65536/*", // invalid port
		"http://bar:-1/*",    // invalid port
		"http://[]:8888/*",   // empty IPv6 host
		"http://[::1/*",      // unterminated IPv6 literal
		"http://[::1]x/*",    // garbage after IPv6 literal
		"<all_urls>",         // valid in Chromium, outside the subset (§4.2)
	}
	for _, p := range bad {
		if _, err := Parse(p, true); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", p)
		}
	}
	good := []string{
		"http://*/*",
		"http://*.google.com/foo*bar",
		"http://*.google.com:1234/foo*bar",
		"http://*.google.com:0/foo*bar",
		"http://*.google.com:65535/foo*bar",
		"http://127.0.0.1/*",
		"http://[2607:f8b0:4005:805::200e]/*",
		"http://[2607:f8b0:4005:805::200e]:8888/*",
		"https://www.google.com/",
	}
	for _, p := range good {
		if _, err := Parse(p, false); err != nil {
			t.Errorf("Parse(%q): %v", p, err)
		}
	}
}

func TestChromiumMatch(t *testing.T) {
	runMatchCases(t, []matchCase{
		// Basic exact host, path glob.
		{"http://google.com/foo*", "http://google.com/foobar", "", true},
		{"http://google.com/foo*", "https://google.com/foobar", "", false},
		{"http://google.com/foo*", "http://google.com/bar", "", false},
		{"http://google.com/foo*", "http://www.google.com/foo", "", false},

		// Subdomain match, including the apex.
		{"http://*.google.com/foo*bar", "http://google.com/foobar", "", true},
		{"http://*.google.com/foo*bar", "http://www.google.com/foo?bar", "", true},
		{"http://*.google.com/foo*bar", "http://monkey.images.google.com/foooobar", "", true},
		{"http://*.google.com/foo*bar", "http://yahoo.com/foobar", "", false},
		{"http://*.google.com/foo*bar", "http://www.google.com/foo", "", false},
		{"http://*.google.com/foo*bar", "http://notgoogle.com/foobar", "", false},

		// Wildcard host and scheme.
		{"*://*/*", "http://google.com/", "", true},
		{"*://*/*", "https://127.0.0.1/x", "", true},
		{"http://*/*", "https://google.com/", "", false},
		{"https://*/*", "https://google.com/", "", true},

		// IP addresses: "*" matches them, exact matches them, a subdomain
		// pattern only by equality.
		{"http://127.0.0.1/*", "http://127.0.0.1/", "", true},
		{"http://*.0.0.1/*", "http://127.0.0.1/", "", false},
		{"http://*.0.0.1/*", "http://0.0.1/", "", true},
		{"http://*/*", "http://[::1]/", "", true},
		{"http://[2607:f8b0:4005:805::200e]/*", "http://[2607:f8b0:4005:805::200e]/x", "", true},
		{"http://[2607:F8B0:4005:805::200E]/*", "http://[2607:f8b0:4005:805::200e]/x", "", true},

		// Port: an explicit pattern port is compared with the effective port.
		{"http://www.example.com:8080/foo", "http://www.example.com:8080/foo", "", true},
		{"http://www.example.com:8080/foo", "http://www.example.com/foo", "", false},
		{"http://www.example.com:80/foo", "http://www.example.com/foo", "", true},
		{"https://www.example.com:443/foo", "https://www.example.com/foo", "", true},
		{"https://www.example.com:443/foo", "https://www.example.com:443/foo", "", true},
		{"https://www.example.com:443/foo", "https://www.example.com:8443/foo", "", false},
		{"https://www.example.com/foo", "https://www.example.com:8443/foo", "", true},
		{"*://*:8443/*", "https://x.example:8443/", "", true},

		// The path glob sees the query (pathForRequest) but not the fragment.
		{"http://x.example/foo?bar", "http://x.example/foo?bar", "", true},
		{"http://x.example/foo?bar*", "http://x.example/foo?bar=1#frag", "", true},
		{"http://x.example/*frag", "http://x.example/a#frag", "", false},

		// '?' is literal, never a single-character wildcard.
		{"http://x.example/a?c", "http://x.example/abc", "", false},
		{"http://x.example/a?c", "http://x.example/a?c", "", true},

		// Paths are case-sensitive; hosts are not.
		{"http://x.example/Foo*", "http://x.example/foo", "", false},
		{"http://X.Example/Foo*", "http://x.EXAMPLE/Foo", "", true},

		// "P/*" also matches "P"; an empty path is "/".
		{"http://x.example/foo/*", "http://x.example/foo", "", true},
		{"http://x.example/foo/*", "http://x.example/foo/", "", true},
		{"http://x.example/foo/*", "http://x.example/foobar", "", false},
		{"http://x.example/*", "http://x.example", "", true},
		{"http://x.example/", "http://x.example?q", "", false},

		// NORMAL unescape: decodes ordinary bytes on both sides.
		{"http://x.example/A*", "http://x.example/%41bc", "", true},
		{"http://x.example/%41*", "http://x.example/Abc", "", true},
		{"http://x.example/café.html", "http://x.example/caf%C3%A9.html", "", true},
		{"http://x.example/café.html", "http://x.example/caf%c3%a9.html", "", true},
		{"http://x.example/caf%C3%A9.html", "http://x.example/caf%c3%a9.html", "", true},

		// NORMAL never decodes %2F %3F %23 %25 %20 or control escapes.
		{"http://x.example/a/b", "http://x.example/a%2Fb", "", false},
		{"http://x.example/a?b", "http://x.example/a%3Fb", "", false},
		{"http://x.example/a#b", "http://x.example/a%23b", "", false},
		{"http://x.example/a%b", "http://x.example/a%25b", "", false},
		{"http://x.example/a b", "http://x.example/a%20b", "", false},
		{"http://x.example/a\nb", "http://x.example/a%0Ab", "", false},
		// ...but the raw form still matches when the pattern is written the same way.
		{"http://x.example/a%2Fb", "http://x.example/a%2Fb", "", true},
		{"http://x.example/a%2F*", "http://x.example/a%2Fb", "", true},
	})
}

// Our own Brave-behaviour vectors: the matcher as brave-core's
// components/query_filter and debounce services apply it to the vendored
// lists (written from the list semantics; no brave-core code is copied).
func TestBraveBehaviour(t *testing.T) {
	runMatchCases(t, []matchCase{
		// Apex match of a clean-urls host scope.
		{"*://*.youtube.com/*", "https://youtube.com/watch?v=x&si=y", "", true},
		{"*://*.youtube.com/*", "https://m.youtube.com/watch?v=x&si=y", "", true},
		// Path + query glob of a debounce include.
		{"*://*.youtube.com/watch?*", "https://www.youtube.com/watch?v=1", "", true},
		{"*://*.youtube.com/watch?*", "https://www.youtube.com/watch", "", false},
		{"*://www.awin1.com/cread.php?*", "https://www.awin1.com/cread.php?awinmid=1&ued=x", "", true},
		{"*://www.awin1.com/cread.php?*", "https://awin1.com/cread.php?awinmid=1", "", false},
		// Exact-path includes with no '*': only that path.
		{"*://www.tkqlhce.com/click-", "https://www.tkqlhce.com/click-", "", true},
		{"*://www.tkqlhce.com/click-", "https://www.tkqlhce.com/click-1-2", "", false},
		{"*://www.tkqlhce.com/click-", "https://www.tkqlhce.com/click-?x=1", "", false},
		{"*://t.lever-analytics.com/email-link?", "https://t.lever-analytics.com/email-link?", "", true},
		{"*://t.lever-analytics.com/email-link?", "https://t.lever-analytics.com/email-link", "", false},
		{"*://t.lever-analytics.com/email-link?", "https://t.lever-analytics.com/email-link?u=1", "", false},
		// A scheme-restricted include.
		{"https://buy.geni.us/Proxy.ashx?*", "http://buy.geni.us/Proxy.ashx?GR_URL=x", "", false},
		{"https://buy.geni.us/Proxy.ashx?*", "https://buy.geni.us/Proxy.ashx?GR_URL=x", "", true},
		// Query text reachable through a mid-glob literal (upstream semantics).
		{"*://*/*viewtopic.php*", "https://evil.example/auth?next=/viewtopic.php", "", true},
	})
}

func TestChromiumParseCanonicalHost(t *testing.T) {
	for _, c := range []struct{ in, host string }{
		{"http://WWW.Example.COM/*", "www.example.com"},
		{"http://*.Example.com./*", "example.com"},
		{"http://example.com.../*", "example.com"},
		{"http://[2001:DB8::1]/*", "[2001:db8::1]"},
	} {
		_, h := mustParse(t, c.in, false).Host()
		if h != c.host {
			t.Errorf("%s: host %q, want %q", c.in, h, c.host)
		}
	}
}
