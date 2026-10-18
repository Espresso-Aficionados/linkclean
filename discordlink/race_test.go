package discordlink

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Espresso-Aficionados/linkclean"
)

// pureProc is a goroutine-safe Processor whose result depends only on the
// URL: odd-numbered paths change, the rest do not.
type pureProc struct{}

func (pureProc) Process(_ context.Context, u string) (linkclean.Result, error) {
	n, _ := strconv.Atoi(u[strings.LastIndexByte(u, '/')+1:])
	if n%2 == 1 {
		return result(u, "https://b.com/"+strconv.Itoa(n/4)), nil // pairs share a result
	}
	return result(u, u), nil
}

// TestConcurrentProcess calls Process from many goroutines with a shared
// Offline Processor and a shared concurrent fake, against results computed
// serially. Run under -race.
func TestConcurrentProcess(t *testing.T) {
	var msgs []string
	tracked := []string{
		"https://example.com/a?utm_source=x",
		"https://example.com/b?fbclid=1&id=2",
		"https://example.com/c?gclid=3",
		"https://example.com/d",
		"https://example.com/a?utm_medium=y",
	}
	for i := range 12 {
		var b strings.Builder
		for j := range i + 1 {
			u := tracked[(i+j)%len(tracked)]
			if (i+j)%3 == 0 {
				u = "||" + u + "||"
			}
			b.WriteString(u + " ")
			b.WriteString("https://a.com/" + strconv.Itoa(i*7+j) + " ")
		}
		msgs = append(msgs, b.String())
	}
	type run struct {
		p Processor
		o Options
	}
	runs := []run{
		{Offline(linkclean.Default()), Options{}},
		{Offline(linkclean.MustNew(linkclean.Config{})), Options{MaxLinks: 4}},
		{pureProc{}, Options{}},
		{pureProc{}, Options{Concurrency: 2, MaxLinks: 20}},
		{pureProc{}, Options{Concurrency: 1}},
	}
	type outcome struct {
		cs []Change
		un int
	}
	want := make([][]outcome, len(runs))
	for i, r := range runs {
		for _, m := range msgs {
			cs, un := Process(context.Background(), m, r.p, r.o)
			want[i] = append(want[i], outcome{cs, un})
		}
	}
	if len(want[0][len(msgs)-1].cs) == 0 || len(want[2][len(msgs)-1].cs) == 0 {
		t.Fatal("the inputs no longer produce changes")
	}
	done := make(chan bool)
	for g := 0; g < 8; g++ {
		go func() {
			defer func() { done <- true }()
			for k := 0; k < 3; k++ {
				for i, r := range runs {
					for j := (g + k) % 5; j < len(msgs); j += 5 {
						cs, un := Process(context.Background(), msgs[j], r.p, r.o)
						if !reflect.DeepEqual(outcome{cs, un}, want[i][j]) {
							t.Errorf("run %d, message %d: concurrent result differs", i, j)
						}
					}
				}
			}
		}()
	}
	for g := 0; g < 8; g++ {
		<-done
	}
}
