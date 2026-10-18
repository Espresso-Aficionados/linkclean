package unshorten

// Config rows: the default User-Agent from the build info, New's validation,
// defaults and shared state, and WithCleaner. resolveConfig's own table is
// resolveconfig_test.go's.

import (
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Espresso-Aficionados/linkclean"
)

const uaSuffix = " (+https://github.com/Espresso-Aficionados/linkclean)"

func TestUserAgent(t *testing.T) {
	dep := func(version string, replace *debug.Module) *debug.BuildInfo {
		return &debug.BuildInfo{
			Main: debug.Module{Path: "example.com/bot", Version: "v1.0.0"},
			Deps: []*debug.Module{
				{Path: "golang.org/x/net", Version: "v0.61.0"},
				{Path: modulePath, Version: version, Replace: replace},
			},
		}
	}
	for _, tc := range []struct {
		name string
		bi   *debug.BuildInfo
		want string
	}{
		{"no build info", nil, "devel"},
		{"main", &debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "v0.3.0"}}, "v0.3.0"},
		{"main devel", &debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "(devel)"}}, "devel"},
		{"main empty", &debug.BuildInfo{Main: debug.Module{Path: modulePath}}, "devel"},
		{"main pseudo", &debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "v0.3.1-0.20261018000000-abcdef123456"}}, "v0.3.1-0.20261018000000-abcdef123456"},
		{"dep", dep("v0.3.0", nil), "v0.3.0"},
		{"dep replace with version", dep("v0.3.0", &debug.Module{Path: "example.com/fork/linkclean", Version: "v0.3.2-fork"}), "v0.3.2-fork"},
		{"dep replace by directory", dep("v0.3.0", &debug.Module{Path: "../linkclean"}), "devel"},
		{"dep devel", dep("(devel)", nil), "devel"},
		{"not a dependency", &debug.BuildInfo{Main: debug.Module{Path: "example.com/bot", Version: "v1.0.0"}}, "devel"},
		{"main is not linkclean", &debug.BuildInfo{Main: debug.Module{Path: "example.com/bot", Version: "v9.9.9"}, Deps: []*debug.Module{{Path: "example.com/other", Version: "v1.2.3"}}}, "devel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := "linkclean/" + tc.want + uaSuffix
			if got := uaFrom(tc.bi); got != want {
				t.Errorf("uaFrom = %q, want %q", got, want)
			}
		})
	}
	ua := defaultUA()
	bi, _ := debug.ReadBuildInfo()
	if ua != uaFrom(bi) || !strings.HasPrefix(ua, "linkclean/") || !strings.HasSuffix(ua, uaSuffix) || strings.ContainsAny(ua, "\r\n\x00") {
		t.Errorf("defaultUA() = %q", ua)
	}
	if defaultUA() != ua {
		t.Error("defaultUA is not stable")
	}
}

// TestNewConfigErrors: New rejects a negative count or duration, an unknown
// GenericPolicy and a UserAgent with CR, LF or NUL, naming each field, all
// in one joined error. No request is made.
func TestNewConfigErrors(t *testing.T) {
	for _, tc := range []struct {
		cfg    Config
		fields []string
	}{
		{Config{MaxHops: -1}, []string{"Config.MaxHops"}},
		{Config{MaxConcurrentRequests: -1}, []string{"Config.MaxConcurrentRequests"}},
		{Config{MaxConcurrentRequestsPerHost: -2}, []string{"Config.MaxConcurrentRequestsPerHost"}},
		{Config{MaxInFlight: -1}, []string{"Config.MaxInFlight"}},
		{Config{MaxInFlightPerHost: -1}, []string{"Config.MaxInFlightPerHost"}},
		{Config{Timeout: -time.Second}, []string{"Config.Timeout"}},
		{Config{PositiveTTL: -1}, []string{"Config.PositiveTTL"}},
		{Config{NegativeTTL: -1}, []string{"Config.NegativeTTL"}},
		{Config{Generic: Skip + 1}, []string{"Config.Generic"}},
		{Config{UserAgent: "bot\r\nX-Evil: 1"}, []string{"Config.UserAgent"}},
		{Config{UserAgent: "bot\x00"}, []string{"Config.UserAgent"}},
		{Config{MaxHops: -1, Timeout: -1, UserAgent: "a\nb"}, []string{"Config.MaxHops", "Config.Timeout", "Config.UserAgent"}},
	} {
		r, err := New(tc.cfg)
		if err == nil || r != nil {
			t.Errorf("New(%+v) = %v, %v; want an error", tc.cfg, r, err)
			continue
		}
		for _, f := range tc.fields {
			if !strings.Contains(err.Error(), f+" ") {
				t.Errorf("New(%+v) error %q does not name %s", tc.cfg, err, f)
			}
		}
		if errors.Is(err, ErrUnguardedTransport) {
			t.Errorf("a Config error wraps ErrUnguardedTransport: %v", err)
		}
	}
}

// TestNewDefaults: the zero Config gives the documented defaults, and an
// over-large MaxHops is clamped to 5.
func TestNewDefaults(t *testing.T) {
	r := newTestResolver(t, Config{})
	want, err := resolveConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	got := r.st.cfg
	got.onRequest, want.onRequest = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cfg = %+v, want %+v", got, want)
	}
	if got.maxReq != 32 || got.maxReqHost != 4 || got.maxFlight != 256 || got.maxFlightHost != 16 || got.maxHops != 4 ||
		got.timeout != 6*time.Second || got.posTTL != 24*time.Hour || got.negTTL != 10*time.Minute || got.policy != PreviewOnly || got.ua != defaultUA() {
		t.Errorf("defaults %+v", got)
	}
	if r.c != linkclean.Default() || r.st.reg != DefaultRegistry() || r.st.log != slog.Default() {
		t.Errorf("Cleaner, Registry or Logger is not the default")
	}
	if _, ok := r.st.cache.(negativeSetter); !ok || r.st.lim == nil {
		t.Errorf("Cache %T, Limiter %T; want NewMemoryCache's and a Limiter", r.st.cache, r.st.lim)
	}
	if c := newTestResolver(t, Config{MaxHops: 9}); c.st.cfg.maxHops != 5 {
		t.Errorf("MaxHops 9 gives %d, want 5", c.st.cfg.maxHops)
	}
}

// TestNewBuildsState: New builds one client that never follows redirects and has
// no jar and no timeout, a slot channel per allowlisted host (Match hosts
// and FetchHosts) with MaxConcurrentRequestsPerHost slots, MaxConcurrentRequests
// global slots, and uses the given Cache, Limiter and Logger.
func TestNewBuildsState(t *testing.T) {
	reg := rvRegistry(t,
		rvEntry("a.example.net"),
		Entry{Name: "b", Match: []string{"*://b.example.net/*", "*://www.b.example.net/*"}, FetchHosts: []string{"api.b.example.net", "a.example.net"}, Strategy: Redirect()},
	)
	cache, lim := NoCache(), NoLimit()
	log := slog.New(slog.NewTextHandler(&rvLog{}, nil))
	called := 0
	r := newTestResolver(t, Config{
		Registry: reg, Cache: cache, Limiter: lim, Logger: log, OnRequest: func(RequestInfo) { called++ },
		MaxConcurrentRequests: 3, MaxConcurrentRequestsPerHost: 2, Generic: Follow, UserAgent: "test-agent",
	})
	st := r.st
	if st.reg != reg || st.cache != cache || st.lim != lim || st.log != log {
		t.Error("Registry, Cache, Limiter or Logger not used as given")
	}
	if st.cfg.policy != Follow || st.cfg.ua != "test-agent" || st.cfg.onRequest == nil {
		t.Errorf("cfg %+v", st.cfg)
	}
	if cap(st.global) != 3 {
		t.Errorf("global slots %d, want 3", cap(st.global))
	}
	var hosts []string
	for h, ch := range st.hostSlots {
		hosts = append(hosts, h)
		if cap(ch) != 2 {
			t.Errorf("host %s has %d slots, want 2", h, cap(ch))
		}
	}
	slices.Sort(hosts)
	if want := []string{"a.example.net", "api.b.example.net", "b.example.net", "www.b.example.net"}; !slices.Equal(hosts, want) {
		t.Errorf("slot hosts %q, want %q", hosts, want)
	}
	c := st.client
	if c.Jar != nil || c.Timeout != 0 || c.CheckRedirect == nil || c.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Errorf("client Jar %v, Timeout %v; want no jar, no timeout and ErrUseLastResponse", c.Jar, c.Timeout)
	}
	if c.Transport == nil {
		t.Error("no transport")
	}
}

// TestWithCleaner: a view shares all network state, uses its own Cleaner
// (nil means linkclean.Default()), and leaves r unchanged.
func TestWithCleaner(t *testing.T) {
	strip := rvStrip(t)
	r := newTestResolver(t, Config{Cleaner: strip})
	if r.c != strip {
		t.Fatal("Config.Cleaner not used")
	}
	v := r.WithCleaner(nil)
	if v.st != r.st || v.c != linkclean.Default() || r.c != strip {
		t.Errorf("WithCleaner(nil): shared %v, Cleaner default %v, parent unchanged %v", v.st == r.st, v.c == linkclean.Default(), r.c == strip)
	}
	w := v.WithCleaner(strip)
	if w.st != r.st || w.c != strip || w == r {
		t.Error("WithCleaner(strip) must return a new view sharing the state")
	}
}
