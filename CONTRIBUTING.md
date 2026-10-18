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

## Package unshorten

`unshorten` is the only package that makes network requests, and its tests
never do: every test that sends a request runs over a loopback test network
(`unshorten/nettest_test.go`) that reaches production code by the real
hostnames with the real dial guard in the dial path, and the test binary
fails if anything tries a real lookup or dial. Never add a test that
contacts a shortener or any other outside host.

### Network golden rows (`testdata/golden/network.txtar`)

The network rows N1 to N5 are the unshortening cases' network halves; their
offline halves are rows of `positive.txtar`. `unshorten`'s
`TestNetworkGolden` replays each section, and the file's header restates
this grammar. Each `-- <id> --` section is one case of `key: value` lines,
the value being the rest of the line verbatim; blank lines and lines
starting with `#` are ignored.

| Key | Meaning |
|---|---|
| `in:` | the input URL (required, once) |
| `registry:` | entries added to `DefaultRegistry`, space-separated; `bit.ly` is the bit.ly entry that ships unregistered (added with `Registry.With`) |
| `generic:` | `PreviewOnly` (the default), `Follow` or `Skip` |
| `server:` | `METHOD URL → STATUS [LOCATION]`: the listener for URL's scheme and host answers exactly this request (path and query as written); any other request to it fails the case |
| `body:` | the `text/html` body of the previous `server:` line's response |
| `listen:` | `URL`: a listener for URL's scheme and host that answers 500 to every request; with `requests:` it shows that a host is never contacted |
| `requests:` | `URL N`: the listener for URL's scheme and host received N requests in total, after every run of the case |
| `want:` | the expected URL under `AffiliateKeep`, and under `AffiliateStrip` unless `want-strip:` is given (required) |
| `want-strip:` | the expected URL under `AffiliateStrip` |
| `err:` | `none` (the default), or the sentinel the error wraps besides `ErrUnresolved` (`ErrFallback`, `ErrNoMatch`, …) |
| `hops: none`, `hop: <JSON>` | `Process`'s Hops in order, under both policies; `hop-keep:`/`hop-strip:` (and `hops-keep: none`, `hops-strip: none`) under one policy only |
| `removed: none`, `removed: <JSON>` | `Process`'s Removed in order, under both policies; `removed-keep:`/`removed-strip:` under one policy only |
| `note:` | free text |

The JSON objects use `positive.txtar`'s field names; the listed fields are
compared and the count must match. Hops or Removed with no line are not
compared. Every case runs `Resolve` and then `Process` under
`AffiliateKeep`, then both again through a `WithCleaner(AffiliateStrip)`
view of the same Resolver; every run after the first must be served from
the cache, with no new request. Each run also checks `Clean(URL) == URL`
and `Process(in).URL == Resolve(in).URL`, and on an error `URL == Clean(in)`
and `errors.Is(err, ErrUnresolved)`.

A change to any file under `testdata/golden/` forces a minor release, and
its `CHANGELOG.md` section must list each changed, added or removed row ID
as its own word: `N1, N2, N3` lists three rows, `N1-N3` lists none.
`TestNetworkGolden` also pins the set of section IDs, so a new row updates
it.

### Recorded fixtures and the live checks

`unshorten/testdata/responses/<host>/<case>.txtar` are recorded (or, where
labelled, synthetic) shortener responses, replayed by
`TestRecordedFixtures`. They are evidence, not expectations, so changing
them forces no release; they carry synthetic `TOKENn` tokens only, never a
real short-link token.

The live suite (`unshorten/livenet_test.go`, build tag `livenet`) sends real
requests to the shorteners and is run only by the maintainer; CI only
compiles it. `unshorten/testdata/livenet/README.md` is the procedure: the
links file and deny list kept outside the repository, record mode for
drafting fixtures, check mode for gating `DefaultRegistry` before a release,
and pruning the entries the run did not prove.

## Rule updates

Brave's lists reach linkclean through a git submodule at
`tools/upstream/adblock-lists` that follows `brave/adblock-lists` `master`.
Nothing builds from it and nothing reads its worktree; only its gitlink (the
commit id) matters, so you never need `git submodule update`.

### The rules PR life cycle

1. **Monday:** when upstream `master` has a commit newer than the gitlink and
   at least 3 days old, Dependabot opens "Bump tools/upstream/adblock-lists
   from X to Y", labelled `rules-update`. It changes only the gitlink. CI's
   `rules-provenance` is red on it until the vendored bytes are synced, unless
   the upstream commit left the three files and `LICENSE` unchanged (a
   **no-op bump**, about 3 weeks in 4).
2. **Tuesday 11:00 UTC:** a review routine, running as the maintainer, follows
   [`tools/rulegen/ROUTINE.md`](tools/rulegen/ROUTINE.md). On a PR that only
   Dependabot and the routine have touched, it commits `rulegen sync`'s output
   (trailer `Rulegen-Sync: <G>`), merges `origin/main` into the branch when
   `main` moved (trailer `Rulegen-Merge: main`), posts a summary comment, and
   then does one of these:
   - **no-op bump:** merges it; no release;
   - **clean data change:** merges it and tags a patch release with
     `rulegen release -pr <n>`;
   - **held** (a hold in `rules/acks.json` on `main` matches the new bytes):
     closes it; Dependabot's next PR is checked against the hold again;
   - **can never merge** (its gitlink left upstream `master`, or `main`'s
     rules moved past it): closes it, so Dependabot proposes current upstream;
   - **needs a human** (a red check, a risky, corpus-host or affiliate-candidate
     pair in the summary, a lapsed hold, a LICENSE change, …): escalates
     (comments and alerts the maintainer) and leaves it open. It works the
     PR again the next week, so a fix that lands on `main` can still make it
     mergeable.
3. The routine never force-pushes, never pushes to `main` and never uses an
   admin bypass. It merges only once all eight checks are green on the head
   it verified, through GitHub's REST merge endpoint pinned to that head (no
   auto-merge), so a push after its review makes GitHub refuse the merge.

**An edited PR blocks the next one.** While a Dependabot submodule PR that
carries anyone else's commit is open, Dependabot opens no newer one. A commit
you push to the branch (a debounce classification, an ack, a hold, a
`known-broken.txt` line) makes the PR yours: the routine stops pushing to it,
merging it and closing it, and alerts "data path waiting on #n" every week
until you settle it. Settle it within the week:

- **merge it** once the checks are green, with a squash merge (never the
  bypass), then tag it:
  `GH_TOKEN="$(gh auth token)" go -C tools run ./rulegen release -root .. -pr <n>`;
- or **close it**. Its commits stay reachable at `refs/pull/<n>/head`
  (`git fetch origin pull/<n>/head`), and Dependabot proposes current
  upstream on its next run.

Do not comment `@dependabot rebase` or `@dependabot recreate` on a rules PR:
recreate drops every commit that is not Dependabot's, and rebase on an
edited PR is undocumented.

**Takeover branches are optional.** To detach a rules change from
Dependabot's PR anyway, create `rules/takeover-<short G>` from the PR head,
open a PR from it labelled `rules-update`, and close Dependabot's PR. The
routine reports a takeover PR and never syncs, merges or closes it; you merge
it and run `rulegen release -pr <n>`. It does not unblock the data path:
Dependabot's next PR is cumulative and usually needs the same decision.

A change that is not data (an overlay or engine change, a reclassification of
an existing debounce entry, a golden-row change) goes through a normal PR to
`main` and a manual release:
`GH_TOKEN="$(gh auth token)" go -C tools run ./rulegen release -root .. -manual -bump minor|patch`,
with a `## vX.Y.Z` section in `CHANGELOG.md`. The tool refuses a bump below
the minimum the changes need.

### Acks and holds (`rules/acks.json`)

`rules/acks.json` is human-owned and is never read or written by `rulegen
sync` or Dependabot. Add entries on the rules PR branch (fast; it makes the
PR yours, see above) or through a normal PR to `main` (the only way a hold
outlives a closed PR). The routine only ever removes entries, in a monthly
cleanup PR.

An **ack** accepts one gate failure for specific vendored bytes. It is bound
to a vendored file's sha256 and has an `expires` date (inclusive, UTC). An ack
whose sha256 no longer matches (lapsed) or that has expired never satisfies a
gate, and the PR summary lists it.

```json
{"schema": 1,
 "acks": [
  {"check": "counts", "metric": "union.global_params", "min": 200,
   "vendored_sha256": "<sha256 of the new clean-urls.json>", "expires": "2026-12-01",
   "reason": "upstream deduplicated the global list (brave/adblock-lists#NNNN)", "by": "<you>"},
  {"check": "risky", "scope": "global", "param": "id",
   "vendored_sha256": "<sha256 of the new clean-urls.json>", "expires": "2026-12-01",
   "reason": "…", "by": "<you>"}
 ],
 "holds": []}
```

- `counts` metrics: `clean-urls.*` and `query-filter.*` (`rules`, `params`,
  `global_params`, `scoped_params`), `union.params`, `union.global_params`,
  `union.scoped_params`, `debounce.includes`. `min` is the lowest value
  accepted.
- `golden` metrics: `corpus.exact`, `corpus.recall_removed`, with `min`.
- `risky`: `scope` `global` and a `param` (scoped risky pairs never fail the
  gate; the routine escalates them instead), no
  `metric` or `min`.
- `vendored_sha256` is the sha256 of a vendored Brave file the PR changes (for
  `debounce.includes`, of `debounce.json`); the summary and `rules.lock.json`
  show it.

A **hold** keeps given upstream bytes off `main`, for example while an
upstream fix is pending. It names the upstream path and the sha256 of the
held bytes, and matches while it is unexpired and a PR proposes exactly those
bytes. A matching hold fails `rules-provenance` with "held: …". All three
lists come from one upstream commit, so holding one file keeps the whole
update out. The summary prints a ready-to-paste hold entry for every vendored
file a PR changes:

```json
"holds": [
  {"file": "brave-lists/clean-urls.json", "sha256": "<sha256 of the held bytes>",
   "expires": "2026-11-15", "reason": "waiting on brave/adblock-lists#NNNN", "by": "<you>"}
]
```

Put a hold on `main` to keep it across weeks: the routine closes a held PR
that only it has touched, and checks Dependabot's next PR against the hold
again. A hold is never released silently: when upstream changes the held file
again, or the hold expires while upstream still serves the held bytes, the
summary says "hold lapsed" and the routine escalates for a human to decide.

### Running rulegen locally

`rulegen` lives in the nested, stdlib-only module `tools/`. Run it from the
repository root; `-root ..` is relative to `tools/`, and clone and output
directories are best given as absolute paths. It needs git 2.38 or later.

```sh
up=$(mktemp -d)/upstream
go -C tools run ./rulegen upstream -root .. -out "$up"       # blobless clone of the pinned URL, no worktree
go -C tools run ./rulegen sync -root .. -upstream "$up"      # write rules/brave/*, NOTICE and the lock for the gitlink
go -C tools run ./rulegen sync -root .. -upstream "$up" -check   # exit 0 iff nothing would change
go -C tools run ./rulegen summary -root .. -base origin/main -upstream "$up" -validate -out "$PWD/out/summary.md"
```

To run every gate as CI does, materialise the base data and point the gates
at both directories (`git fetch origin main` first):

```sh
base=$(mktemp -d)/base
printf '{"before":"%s"}' "$(git rev-parse origin/main)" > "$base.event.json"
go -C tools run ./rulegen basedir -root .. -event push -event-path "$base.event.json" -out "$base"
LINKCLEAN_BASE_DIR="$base" LINKCLEAN_UPSTREAM_DIR="$up" go test -run '^TestRules$' -v .
```

Without `LINKCLEAN_BASE_DIR` the base-relative parts (relative count floors,
`Risky`, corpus deltas, holds, the acks-changed and upstream checks) are
skipped with one log line each; without `LINKCLEAN_UPSTREAM_DIR` the upstream
provenance checks are skipped. `go -C tools run ./rulegen <command> -h` lists
each command's flags.
