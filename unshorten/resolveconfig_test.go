package unshorten

import (
	"strings"
	"testing"
	"time"
)

func TestResolveConfigDefaults(t *testing.T) {
	rc, err := resolveConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if rc.maxReq != 32 || rc.maxReqHost != 4 || rc.maxFlight != 256 || rc.maxFlightHost != 16 || rc.maxHops != 4 {
		t.Errorf("bounds = %d/%d/%d/%d, hops %d", rc.maxReq, rc.maxReqHost, rc.maxFlight, rc.maxFlightHost, rc.maxHops)
	}
	if rc.timeout != 6*time.Second || rc.posTTL != 24*time.Hour || rc.negTTL != 10*time.Minute {
		t.Errorf("durations = %v %v %v", rc.timeout, rc.posTTL, rc.negTTL)
	}
	if rc.policy != PreviewOnly || rc.onRequest != nil {
		t.Error("policy or onRequest not zero")
	}
	if rc.ua != defaultUA() || !strings.HasPrefix(rc.ua, "linkclean/") ||
		!strings.HasSuffix(rc.ua, " (+https://github.com/Espresso-Aficionados/linkclean)") {
		t.Errorf("ua = %q", rc.ua)
	}
}

func TestResolveConfigValues(t *testing.T) {
	called := false
	rc, err := resolveConfig(Config{
		MaxConcurrentRequests: 8, MaxConcurrentRequestsPerHost: 2, MaxInFlight: 4, MaxInFlightPerHost: 1,
		MaxHops: 9, Timeout: time.Second, PositiveTTL: time.Minute, NegativeTTL: time.Second,
		Generic: Skip, UserAgent: "bot/1", OnRequest: func(RequestInfo) { called = true },
	})
	if err != nil {
		t.Fatal(err)
	}
	if rc.maxReq != 8 || rc.maxReqHost != 2 || rc.maxFlight != 4 || rc.maxFlightHost != 1 {
		t.Errorf("bounds = %+v", rc)
	}
	if rc.maxHops != 5 {
		t.Errorf("MaxHops 9 → %d, want 5", rc.maxHops)
	}
	if rc.timeout != time.Second || rc.posTTL != time.Minute || rc.negTTL != time.Second || rc.policy != Skip || rc.ua != "bot/1" {
		t.Errorf("values = %+v", rc)
	}
	rc.onRequest(RequestInfo{})
	if !called {
		t.Error("onRequest not kept")
	}
	if rc, _ := resolveConfig(Config{MaxHops: 1}); rc.maxHops != 1 {
		t.Errorf("MaxHops 1 → %d", rc.maxHops)
	}
}

func TestResolveConfigErrors(t *testing.T) {
	for _, tt := range []struct {
		cfg  Config
		want string
	}{
		{Config{MaxConcurrentRequests: -1}, "MaxConcurrentRequests"},
		{Config{MaxConcurrentRequestsPerHost: -1}, "MaxConcurrentRequestsPerHost"},
		{Config{MaxInFlight: -1}, "MaxInFlight"},
		{Config{MaxInFlightPerHost: -1}, "MaxInFlightPerHost"},
		{Config{MaxHops: -1}, "MaxHops"},
		{Config{Timeout: -1}, "Timeout"},
		{Config{PositiveTTL: -1}, "PositiveTTL"},
		{Config{NegativeTTL: -time.Second}, "NegativeTTL"},
		{Config{Generic: Skip + 1}, "Generic"},
		{Config{UserAgent: "a\r\nX-Evil: 1"}, "UserAgent"},
		{Config{UserAgent: "a\x00"}, "UserAgent"},
		{Config{UserAgent: "a\n"}, "UserAgent"},
	} {
		_, err := resolveConfig(tt.cfg)
		if err == nil || !strings.Contains(err.Error(), "Config."+tt.want+" ") {
			t.Errorf("%+v: err = %v, want one naming %s", tt.cfg, err, tt.want)
		}
	}
	_, err := resolveConfig(Config{MaxHops: -1, Timeout: -1})
	if err == nil || !strings.Contains(err.Error(), "MaxHops") || !strings.Contains(err.Error(), "Timeout") {
		t.Errorf("two errors: %v", err)
	}
}
