package linkclean_test

import (
	"fmt"

	"github.com/Espresso-Aficionados/linkclean"
)

func ExampleClean() {
	fmt.Println(linkclean.Clean("https://www.youtube.com/watch?v=dQw4w9WgXcQ&si=Xk3v&t=42"))
	// Output: https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=42
}

// Custom rules, a false-positive keep, and affiliate stripping (§3).
func ExampleNew() {
	c, err := linkclean.New(linkclean.Config{
		Extra: []linkclean.Rule{
			{ID: "keep-page", Action: linkclean.Keep, Include: []string{"*://*.example.com/*"}, Params: []string{"page"}},
			{ID: "strip-foo", Action: linkclean.Strip, Include: []string{"*://*/*"}, Params: []string{"foo_track", "page"}},
		},
		Affiliate:   linkclean.AffiliateStrip,
		DisableTags: []string{"fragment"},
	})
	if err != nil {
		panic(err)
	}
	for _, w := range c.Warnings() {
		fmt.Println("warning:", w)
	}
	fmt.Println(c.Clean("https://www.example.com/list?page=2&foo_track=1"))
	fmt.Println(c.Clean("https://www.amazon.com/dp/B0ABC?tag=foo-20&ref_=nav&th=1"))
	// Output:
	// https://www.example.com/list?page=2
	// https://www.amazon.com/dp/B0ABC?th=1
}

func ExampleCleaner_Explain() {
	c := linkclean.MustNew(linkclean.Config{Affiliate: linkclean.AffiliateStrip})
	r := c.Explain("https://www.amazon.com/Some-Title/dp/B0ABC/ref=sr_1_3?qid=1&tag=aff-20")
	fmt.Println(r.URL)
	for _, m := range r.Removed {
		cat, _ := m.Category.MarshalText()
		fmt.Printf("%s %s %q\n", cat, m.Name, m.Beneficiary.ID)
	}
	// Output:
	// https://www.amazon.com/Some-Title/dp/B0ABC
	// path /ref ""
	// tracker qid ""
	// affiliate tag "aff-20"
}
