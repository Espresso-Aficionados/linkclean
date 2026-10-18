package dialhook

import (
	"net/netip"
	"sync"
	"testing"
)

func TestSeams(t *testing.T) {
	if Resolve() != nil || AllowAddr() != nil {
		t.Fatal("seams are set at start")
	}
	r1 := SetResolve(func(host, port string) (string, bool) { return "1:" + host + ":" + port, true })
	r2 := SetResolve(func(host, port string) (string, bool) { return "", false })
	if _, ok := Resolve()("t.co", "443"); ok {
		t.Error("second Resolve not installed")
	}
	r2()
	if a, ok := Resolve()("t.co", "443"); !ok || a != "1:t.co:443" {
		t.Errorf("restore did not bring back the first seam: %q %v", a, ok)
	}
	r3 := SetResolve(nil)
	if Resolve() != nil {
		t.Error("SetResolve(nil) did not clear")
	}
	r3()
	r1()
	if Resolve() != nil {
		t.Error("restore did not clear")
	}

	want := netip.MustParseAddrPort("127.0.0.1:4433")
	ra := SetAllowAddr(func(a netip.AddrPort) bool { return a == want })
	if !AllowAddr()(want) || AllowAddr()(netip.MustParseAddrPort("127.0.0.1:4434")) {
		t.Error("AllowAddr seam wrong")
	}
	ra()
	if AllowAddr() != nil {
		t.Error("AllowAddr restore did not clear")
	}
}

// TestSeamsConcurrent loads while another goroutine sets and restores; run
// under -race.
func TestSeamsConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 1000 {
			SetResolve(func(string, string) (string, bool) { return "", false })()
		}
	}()
	go func() {
		defer wg.Done()
		for range 1000 {
			if f := Resolve(); f != nil {
				f("h", "443")
			}
		}
	}()
	wg.Wait()
	if Resolve() != nil {
		t.Error("seam left set")
	}
}
