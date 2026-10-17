<!-- rulegen:summary -->
## Rule data summary

### Header

- Gitlink: `575544b5a89c` → `c45478f0918d`
- Last synced: `575544b5a89c` → `c45478f0918d`
- Compare: https://github.com/brave/adblock-lists/compare/575544b5a89cf3c1ed4961ee13fa543a62732a9d...c45478f0918d23dce75ae21ec1924939d1d7bad7
- Synced: the vendored bytes are upstream@c45478f0918d's

Changed vendored files (sha256, base's lock → head):

- `brave-lists/clean-urls.json`: `e93f931581b4` → `8fe66a61b5e5`

Hold entries for rules/acks.json (ready to paste into `holds`):

```json
{"file": "brave-lists/clean-urls.json", "sha256": "8fe66a61b5e54cfcc769ba173fbbb65d16e6e44ad4fa61944483937c9c683273", "expires": "YYYY-MM-DD", "reason": "…", "by": "…"}
```

### Counts

| | rules | params | global params | scoped params |
|---|---|---|---|---|
| clean-urls | 3 | 5 → 6 (+20.0%) | 2 → 3 (+50.0%) | 3 |
| query-filter | 1 | 1 | 1 | 0 |
| union (distinct pairs) | | 6 → 7 (+16.7%) | 3 → 4 (+33.3%) | 3 |

Debounce: rules 2, include instances 3.

### Pairs

⚠ risky name, ★ corpus host, ⚑ affiliate-class candidate.

Added (1):

- ★ (`global`, `utm_medium`)

Removed (0):

none

### Rule IDs

Added (0):

none

Removed (0):

none

### Overlay hygiene

none

### Acks

No lapsed or expired entries.

### Watch

brave-core `components/query_filter/browser/utils.cc` at HEAD matches tools/rulegen/watch.json

- `kConditionalQueryStringTrackers`: unchanged (849066adfcf6)
- `kExemptedHostnames`: unchanged (4bc1b641f171)

### Debounce

Include changes:

none

Notes:

none

### Behaviour diff

No corpus output changed (4 items).
