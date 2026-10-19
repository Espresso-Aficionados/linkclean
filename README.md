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

## Unshortening (opt-in, network)

Package `unshorten` resolves an allowlisted set of short links (t.co,
amzn.to, pin.it and others; the package documentation lists every host it
contacts) over the network, so a shortener sees one request from your server
instead of a click from every reader. It is the only package here that makes
network requests. Inputs are cleaned offline before any request, results are
cleaned again, and a failure falls back to the offline clean:

```go
r, err := unshorten.New(unshorten.Config{}) // one Resolver per process
if err != nil {
	// handle err
}
// A view with another affiliate policy, sharing r's cache, rate limits and
// request bounds.
strip := r.WithCleaner(linkclean.MustNew(linkclean.Config{Affiliate: linkclean.AffiliateStrip}))

res, err := strip.Resolve(ctx, "https://amzn.to/abc")
// res.URL is usable whatever err is; err says why resolution stopped.
```

A `*unshorten.Resolver` is a `discordlink.Processor`. The default transport
refuses non-public destinations at dial time. Before deploying it, read the
[privacy statement](https://pkg.go.dev/github.com/Espresso-Aficionados/linkclean/unshorten#hdr-Privacy):
each request tells the shortener your server's IP address and which link was
posted, and a shortener may count it as a click.

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

linkclean also depends on `golang.org/x/net` and `golang.org/x/text`
(BSD-3-Clause, the Go project's license). A binary must reproduce their
copyright notice, as it must for the Go standard library under the same
license.

`unshorten` depends on [`code.dny.dev/ssrf`](https://pkg.go.dev/code.dny.dev/ssrf)
(MIT). A binary that includes `unshorten` must carry ssrf's copyright and
permission notice, which `NOTICE.md` reproduces; programs that import only
`linkclean` and `discordlink` do not include it.
