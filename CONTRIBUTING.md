# Contributing

## Rule data

The rules live in two places with different owners and licenses:

| Path | Owner | License |
|---|---|---|
| `rules/brave/*`, `rules.lock.json` | written only by `rulegen sync` from the pinned upstream commit; never edited by hand | MPL-2.0 (upstream) |
| `rules/overlay/overlay.json` | humans, by pull request | MIT |
| `rules/overlay/debounce-classes.json` | humans, by pull request | MPL-2.0 |

The vendored Brave files are checked byte for byte against the lock, so a hand
edit fails CI. To change how linkclean treats an upstream rule, add an overlay
entry (a `keep` neutralizes an upstream strip without editing the upstream file)
or change its debounce classification.

### Overlay entries need evidence

Every rule in `rules/overlay/overlay.json` must have a non-empty `ref` naming
its evidence, of one of these kinds:

- an item of the evaluation corpus (`testdata/corpus/eval-2026-10/`);
- a vendor document (link-format or parameter documentation);
- a public issue;
- a real URL you observed yourself, recorded with its identifiers replaced.

The evidence must be independent of other URL cleaners' lists. **Do not copy
entries, lists or regular expressions from ClearURLs (LGPL), url-cleaner
(AGPL), Unalix (LGPL) or brave-core code and tests (MPL).** A name you only
found in such a list does not ship until you have independent evidence for it.

Every overlay rule must also be named by at least one case in
`testdata/golden/positive.txtar` (`rules:` line); add a case built from the
entry's evidence. Overlay path globs may use `*` only as their last character,
and `urlRegexps` must not mention a host.

### Provenance history

The evaluation corpus was written by model agents that had ClearURLs' provider
lists open, and some parameter names in it were enumerated by reading those
lists rather than observed (TikTok `u_code`/`preview_pb`, Etsy
`organic_search_click`, eBay `_trkparms`/`hash`, the Walmart `ath*` family).
For those names a corpus item is circular evidence and does not count; they
stay out of `overlay.json` until a vendor document or a real observed URL
supports them. This project does not claim that nothing in it traces back to
other cleaners' lists; it records where each entry came from.

### Debounce classifications

Every include pattern of `rules/brave/debounce.json` needs exactly one entry in
`rules/overlay/debounce-classes.json`: `affiliate` (with `network`, and an
`idParam` or `idPath` locator with a `ref`, or a `why` when there is no single
beneficiary ID), `unwrap`, or `off` (with a `why`). Contributions to this file
are licensed under MPL-2.0.

## Human-owned test inputs

- `testdata/corpus/eval-2026-10/known-broken.txt`: corpus items accepted as
  broken, one `<id> # <reason>` per line. Adding a line needs a reason and
  maintainer sign-off.
- `testdata/keep-coverage-exempt.txt`: `(param, Brave include)` pairs that an
  affiliate or keep-override entry deliberately does not cover, one
  `<param> <include> # <reason>` per line.
- `testdata/golden/risky-params.txt`: extra parameter names that must never be
  stripped globally, one per line.
