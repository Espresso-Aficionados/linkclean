package discordlink_test

import (
	"context"
	"fmt"
	"strings"

	"github.com/Espresso-Aficionados/linkclean"
	"github.com/Espresso-Aficionados/linkclean/discordlink"
)

func ExampleFind() {
	content := "see https://a.example/x, <https://b.example/y>, " +
		"[the docs](https://c.example/z) and ||https://d.example/s||"
	kinds := map[discordlink.Kind]string{
		discordlink.Bare:             "bare",
		discordlink.Suppressed:       "suppressed",
		discordlink.Masked:           "masked",
		discordlink.MaskedSuppressed: "masked, suppressed",
	}
	for _, l := range discordlink.Find(content) {
		fmt.Printf("%s %s spoiler=%v label=%q embed=%v\n", l.URL, kinds[l.Kind], l.Spoiler, l.Label, l.WouldEmbed())
	}
	// Output:
	// https://a.example/x bare spoiler=false label="" embed=true
	// https://b.example/y suppressed spoiler=false label="" embed=false
	// https://c.example/z masked spoiler=false label="the docs" embed=true
	// https://d.example/s bare spoiler=true label="" embed=true
}

func ExampleProcess() {
	content := "read this https://example.com/post?id=7&utm_source=newsletter and https://example.com/about"
	cs, unexamined := discordlink.Process(context.Background(), content, discordlink.Offline(linkclean.Default()), discordlink.Options{})
	for _, c := range cs {
		fmt.Println(c.Link.URL, "->", c.Result.URL)
	}
	fmt.Println("unexamined:", unexamined)
	// Output:
	// https://example.com/post?id=7&utm_source=newsletter -> https://example.com/post?id=7
	// unexamined: 0
}

// A reply that names what was removed under each link and says how many
// links it left out, in one body.
func ExampleRepost() {
	content := "https://example.com/a?utm_source=x https://example.com/b?fbclid=y " +
		"||https://example.com/c?utm_medium=z||"
	// MaxLinks 2 leaves the third link unexamined.
	cs, unexamined := discordlink.Process(context.Background(), content,
		discordlink.Offline(linkclean.Default()), discordlink.Options{MaxLinks: 2})
	bodies := discordlink.Repost(cs, discordlink.RepostOptions{
		Wrap:      discordlink.WrapNone, // the caller sends with SUPPRESS_EMBEDS
		Header:    "Without tracking:",
		MaxBodies: 1,
		Annotate: func(c discordlink.Change, compact bool) ([]string, int) {
			var names []string
			for _, r := range c.Result.Removed {
				names = append(names, r.Name)
			}
			return []string{"-# removed " + strings.Join(names, ", ")}, 0
		},
		Unexamined: unexamined,
		Overflow: func(omitted, unexamined int) string {
			return fmt.Sprintf("-# %d more not shown, %d not checked", omitted, unexamined)
		},
	})
	for _, b := range bodies {
		fmt.Println(b)
	}
	// Output:
	// Without tracking:
	// https://example.com/a
	// -# removed utm_source
	// https://example.com/b
	// -# removed fbclid
	// -# 0 more not shown, 1 not checked
}
