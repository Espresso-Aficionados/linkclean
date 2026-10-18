# Changelog

Every manual (non-data) release has a `## vX.Y.Z` section here; a minor
release lists the golden rows it changed. Data refreshes are patch releases
cut by `rulegen release` and are not listed.

## v0.3.0

- `unshorten`: opt-in resolution of short links over the network, for an
  allowlisted set of shorteners that the package documentation lists with
  every host it contacts. `New` and `Resolver` (`Resolve`, `Process`,
  `Match`, `WithCleaner`); `Registry` and `DefaultRegistry`; the strategies
  `Redirect`, `BodyRegexp`, `TikTokOEmbed`, `BitlyInfo` and `TinyURLPreview`
  (bit.ly and tinyurl.com are not registered by default); the dial-guarded
  transport (`NewTransport`, `GuardTransport`, `GuardControl`,
  `GuardDialer`); `NewMemoryCache` and `NewHostLimiter`. The package
  documentation's Network behaviour and Privacy sections state what is
  requested and what a shortener learns.
- New module requirement `code.dny.dev/ssrf` v0.3.0 (MIT), used by
  `unshorten` only.
- `internal/rawurl`: `rawurl.RequestForm`, a URL as `unshorten` requests it
  (no fragment, scheme and host canonical, everything else as written).
- `linkclean.Result`'s documentation describes a Result from `unshorten`'s
  `Process`.
- Golden rows added: N1, N2, N3, N4, N5 in `testdata/golden/network.txtar`,
  the network halves of the unshortening cases. No existing row changed.

## v0.2.0

- `discordlink`: `Find` (Discord's client link grammar, linear time),
  `Process`/`Offline`, `Repost` with the `Annotate`/`MaxBodies`/`Unexamined`/
  `Overflow` hook. Some grammar points, among them the `)` rule,
  masked-link previews, upper-case schemes and server previews versus client
  links, are not yet confirmed against Discord's clients; the package
  documentation lists them under Unverified grammar.
- No golden rows changed.

## v0.1.0

- Core engine: Brave clean-urls, query-filter and debounce rules plus the
  linkclean overlay; `Clean`, `Explain`, `New` and `Default`.
- Rule data pinned to brave/adblock-lists `8846788d`, with provenance,
  schema, compile, count, risky-parameter and golden-corpus gates in CI.
- `tools/rulegen`: the upstream sync, CI base data, PR summary, liveness
  and release tooling behind weekly rule-data updates.
