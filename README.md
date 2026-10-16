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

## License

The code is MIT (`LICENSE`). The embedded Brave lists in `rules/brave/` and
`rules/overlay/debounce-classes.json` are MPL-2.0; `NOTICE.md` lists them.

If you distribute a binary built with linkclean, MPL-2.0 §3.2 requires you to
tell recipients where the MPL-covered source is. This module's source together
with `rules/brave/NOTICE` and `NOTICE.md` suffices, and `Cleaner.Notices()`
returns the same attribution at run time for an about page or a `--licenses`
flag. There are no obligations for your own code.
