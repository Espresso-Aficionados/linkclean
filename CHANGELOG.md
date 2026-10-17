# Changelog

Every manual (non-data) release has a `## vX.Y.Z` section here; a minor
release lists the golden rows it changed. Data refreshes are patch releases
cut by `rulegen release` and are not listed.

## v0.1.0

- Core engine: Brave clean-urls, query-filter and debounce rules plus the
  linkclean overlay; `Clean`, `Explain`, `New` and `Default`.
- Rule data pinned to brave/adblock-lists `8846788d`, with provenance,
  schema, compile, count, risky-parameter and golden-corpus gates in CI.
- `tools/rulegen`: the upstream sync, CI base data, PR summary, liveness
  and release tooling behind weekly rule-data updates.
