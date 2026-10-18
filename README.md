# linkclean

linkclean removes tracking parameters from http(s) URLs and unwraps known
redirect wrappers. It is a pure, offline string transform for Go:

```go
if out := linkclean.Clean(in); out != in {
	// in carried trackers or a wrapper; out is the cleaned URL
}
```

`Cleaner.Explain` reports what was removed and why. The rules are Brave's
`clean-urls.json`, `query-filter.json` and `debounce.json`, embedded unmodified
from one pinned commit, plus an overlay maintained here. See the package
documentation for the guarantees, the guards and the deviations from Brave.

For chat bots, the `discordlink` package finds the links in a Discord message
as Discord's clients parse them, cleans them, and renders the cleaned URLs as
reply bodies that fit Discord's limits.

## How rule updates work

A submodule gitlink at `tools/upstream/adblock-lists` tracks Brave's
`master`. Each Monday Dependabot proposes moving it. On Tuesday a review
routine copies the three lists into `rules/brave/` with `rulegen sync`,
checks the result with the same gates CI runs, and merges it. A change to the
lists is then tagged as a patch release, but only if CI on the merged commit
is green; an upstream commit that leaves the lists unchanged releases
nothing. Anything the gates or the routine flag waits for a human. Rule
changes therefore arrive as ordinary module versions: upgrade linkclean to
pick them up. `CONTRIBUTING.md` describes the life cycle, acks and holds.

## License

The code is MIT (`LICENSE`). The embedded Brave lists in `rules/brave/` and
`rules/overlay/debounce-classes.json` are MPL-2.0; `NOTICE.md` lists them.

If you distribute a binary built with linkclean, MPL-2.0 §3.2 requires you to
tell recipients where the MPL-covered source is. This module's source together
with `rules/brave/NOTICE` and `NOTICE.md` suffices, and `Cleaner.Notices()`
returns the same attribution at run time for an about page or a `--licenses`
flag. There are no obligations for your own code.
