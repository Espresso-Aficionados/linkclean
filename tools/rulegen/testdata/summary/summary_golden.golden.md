<!-- rulegen:summary -->
## Rule data summary

### Header

- Gitlink: `575544b5a89c` → `a1e15b11d22c`
- Last synced: `575544b5a89c` → `a1e15b11d22c`
- Compare: https://github.com/brave/adblock-lists/compare/575544b5a89cf3c1ed4961ee13fa543a62732a9d...a1e15b11d22c6f9056c23fdb16ede44c6ed71beb
- note: no upstream clone (pass -upstream or set LINKCLEAN_UPSTREAM_DIR); sync state not checked
- **Hold lapsed**: hold lapsed: vendored bytes changed (bbbbbbbbbbbb → 656db6f90d9c) for `brave-lists/clean-urls.json` `bbbbbbbbbbbb` (fixture: waiting on upstream, expires 2026-12-01)

Changed vendored files (sha256, base's lock → head):

- `brave-lists/clean-urls.json`: `e93f931581b4` → `656db6f90d9c`
- `brave-lists/query-filter.json`: `310f0ddc5339` → `5b1e539dd43e`
- `brave-lists/debounce.json`: `318232c1defe` → `aaefcfc10064`

Hold entries for rules/acks.json (ready to paste into `holds`):

```json
{"file": "brave-lists/clean-urls.json", "sha256": "656db6f90d9c7780acb9579f8a7f520c291fac9f368421587d35361d07ade36e", "expires": "YYYY-MM-DD", "reason": "…", "by": "…"},
{"file": "brave-lists/query-filter.json", "sha256": "5b1e539dd43ea891e5d861dddd84f711b1ecb4a2bab283f1b0b57014b74d58f1", "expires": "YYYY-MM-DD", "reason": "…", "by": "…"},
{"file": "brave-lists/debounce.json", "sha256": "aaefcfc10064c84dc030727ff5e8e57c9dd966ce5f9ab727faa383e26c21201a", "expires": "YYYY-MM-DD", "reason": "…", "by": "…"}
```

### Counts

| | rules | params | global params | scoped params |
|---|---|---|---|---|
| clean-urls | 3 | 5 → 6 (+20.0%) | 2 → 3 (+50.0%) | 3 |
| query-filter | 1 | 1 → 2 (+100.0%) | 1 → 2 (+100.0%) | 0 |
| union (distinct pairs) | | 6 → 8 (+33.3%) | 3 → 5 (+66.7%) | 3 |

Debounce: rules 2, include instances 3 → 4 (+33.3%).

Params moved between the two files (not removals):

- (`global`, `fbclid`): clean-urls → query-filter

### Pairs

⚠ risky name, ★ corpus host, ⚑ affiliate-class candidate.

Added (4):

- ★ (`*://*.news.example/*`, `ns_mchannel`)
- ⚠★ (`*://*.shop.example/*`, `id`) — risky: never-strip
- ★⚑ (`global`, `irpid`) — affiliate-class candidate
- ★ (`global`, `mc_cid`)

Removed (2):

- ⚠★ (`*://*.news.example/*`, `share`) — risky: risky-params.txt
- ⚠★ (`*://*.news.example/*`, `st`) — risky: never-strip

### Rule IDs

Added (0):

none

Removed (0):

none

### Overlay hygiene

- **new** stale keep-override: `news-keep-st` keeps `st`, which no Brave rule strips in its scope any more

### Acks

- ack lapsed (bound to aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa): acks[0] counts `union.global_params` (fixture: an old ack)
- ack expired (2026-10-01): acks[0] counts `union.global_params` (fixture: an old ack)

### Watch

brave-core `components/query_filter/browser/utils.cc` at HEAD matches tools/rulegen/watch.json

- `kConditionalQueryStringTrackers`: unchanged (849066adfcf6)
- `kExemptedHostnames`: unchanged (4bc1b641f171)

### Debounce

Include changes:

- rule 0 (`redirect`, `url`): added `*://x.affnet.example/go`

Notes:

- **new** `*://x.affnet.example/go`: matches one exact path only (rule 0)

### Behaviour diff

3 of 4 corpus items changed:

| Item | | Input | Base | Head |
|---|---|---|---|---|
| `shop-1` | **broke** | `https://www.shop.example/p?id=7&sref=a&utm_source=x` | `https://www.shop.example/p?id=7` | `https://www.shop.example/p` |
| `news-1` | changed | `https://www.news.example/a?st=gift&share=1` | `https://www.news.example/a?st=gift` | `https://www.news.example/a?st=gift&share=1` |
| `other-1` | improved | `https://www.other.example/?page=2&mc_cid=5` | `https://www.other.example/?page=2&mc_cid=5` | `https://www.other.example/?page=2` |
