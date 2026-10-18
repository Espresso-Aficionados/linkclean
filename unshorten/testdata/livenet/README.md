# unshorten live checks

`unshorten/livenet_test.go` (build tag `livenet`) sends real requests to the
shorteners of a links file and checks each link against what the file
expects. It is how the default registry is proven before a release, and how
it is re-checked afterwards. Record mode also drafts the recorded fixtures
under `unshorten/testdata/responses/`.

- **Only the maintainer runs it.** CI never does: it only compiles the suite
  (`go vet -tags livenet ./unshorten/...`) and tests the scrubber and the
  file handling (`scrub_test.go`, `liveio_test.go`, untagged).
- **Egress: the workstation.** The workstation shares its egress with the
  deployment. Record mode and check mode both run there,
  and the results file says `egress: workstation`.
- **Nothing private enters the repository.** The links file, the deny list
  and every output directory must be outside every git work tree; the suite
  refuses otherwise. A short link's token can identify the account that
  shared it. Only the reviewed results file and reviewed fixtures are
  committed.

## Files here

| File | What it is |
|---|---|
| `links.schema.json` | JSON Schema of the links file. `TestLinksSchema` holds it to the Go types. |
| `links.example.json` | An example links file: one case per default entry and pattern, plus tinyurl and bit.ly. Its tokens are placeholders (`TOKENn`) and its expectations are predictions, not observations. `TestLinksExample` parses it. |
| `results.md.tmpl` | The results file's template (Go `text/template`). The suite renders it; `TestRenderResults` checks it. |
| `results-<date>.md` | Committed check-mode results, one per run that gated a release or found drift. |

## Setup (once)

From the repository root:

```sh
mkdir -m 0700 ~/lc-live
cp unshorten/testdata/livenet/links.example.json ~/lc-live/links.json
chmod 600 ~/lc-live/links.json
( umask 077 && : > ~/lc-live/deny.txt )
```

Edit `~/lc-live/links.json`. Per default entry, and per pattern for amazon's
`a.co`, `amzn.to`, `amzn.eu` and `amzn.asia`:

- a **good** case: a link you created from your own account (`"origin":
  "created"`), or, where you cannot create one (amzn.to needs Associates
  SiteStripe), a link already public in the wild (`"origin": "public"`). A
  public link is checked but never recorded, and the entry's committed
  positive fixture is derived by hand and labelled synthetic.
- an **invalid** case: an invented code (`"origin": "synthetic"`), which
  exercises the shortener's fallback or error path.
- Point your own links at a target you control, such as
  `https://github.com/Espresso-Aficionados/linkclean`, and set
  `"keep_destination": true` on them; any other destination is moved to
  `example.net` in the drafts, with its path shape kept.
- Set each `expect` to what you expect. `strategy` and `status` are those of
  the case's last request (the real HTTP status, also for body strategies);
  `hops` lists `Result.Hops` (status 0 for body and preview strategies).
  `links.schema.json` documents every field.

Put in `~/lc-live/deny.txt`, one per line, every string the drafts must never
hold: your usernames and display names on each platform, account and
profile IDs, the Associates tag, your name and email. Lines starting with `#`
are comments; an entry needs at least 3 characters.

## Record: draft the fixtures

From the repository root:

```sh
OUT=~/lc-live/record-$(date -u +%F)
mkdir -m 0700 "$OUT"
go test -tags livenet ./unshorten -run '^TestLive$' -count=1 -timeout 30m -v \
  -livenet.links ~/lc-live/links.json \
  -livenet.deny ~/lc-live/deny.txt \
  -livenet.out "$OUT" \
  -livenet.marketing \
  -livenet.record
```

`-livenet.marketing` registers tinyurl and bit.ly, which `DefaultRegistry`
does not, for the run; drop it to leave their cases failing as unmatched.
`-count=1` makes sure the run is never served from the test cache.

It writes `$OUT/record-<date>.md` (the same table as check mode, plus what
was drafted per case) and `$OUT/drafts/<host>/<case>.txtar`, one draft per
request, in the fixture format. Per link it:

1. resolves the link through `NewTransport()` wrapped in a recording
   RoundTripper (the dial guard stays in the dial path) and a cache that
   stores nothing, so every hop is fetched and every hop outcome is seen;
2. scrubs (`scrub_test.go`, rules v1): tokens become `TOKENn` consistently
   in the request, the Location, the body and `meta.expect`; a platform
   destination moves to `example.net`; body excerpts (the extracted element
   with 512 bytes either side, or the whole JSON) lose nonces, `data-*-id`
   values, `urn:li:` IDs, JWTs, UUIDs, form tokens and long opaque runs;
3. refuses the whole link, writing nothing of it, if any draft still holds
   something shaped like a cookie, a `Set-Cookie` or `Authorization` header,
   an email, an IP literal outside 192.0.2.0/24, 198.51.100.0/24,
   203.0.113.0/24 and 2001:db8::/32, a deny-list string, or an original
   token (also percent-decoded and HTML-unescaped);
4. replays the drafts as written through the real strategies and refuses the
   link unless every hop outcome (next, strategy, status, failure) matches
   the live run's.

A refusal names the rule and the byte offset, never the content. A refused
link, a public link and a transport failure (no response to record) get no
drafts: write those fixtures by hand from what the run showed.

Review every draft whole before committing it:

- nothing in it may identify an account: read the body excerpt, not just the
  header;
- the header says which request of which hop it is; a hop with two requests
  (a primary that failed, then its fallback) has two drafts, and only the
  last carries the hop's outcome;
- for an entry whose positive link you could not create, derive the positive
  fixture by hand from its recorded invalid-code response and the N2
  synthetic destination, and label its header `synthetic`;
- move it to `unshorten/testdata/responses/<host>/<case>.txtar` and run
  `go test ./unshorten -run TestRecordedFixtures`.

## Check: gate the registry

From the repository root, at the commit to be released:

```sh
OUT=~/lc-live/check-$(date -u +%F)
mkdir -m 0700 "$OUT"
go test -tags livenet ./unshorten -run '^TestLive$' -count=1 -timeout 30m -v \
  -livenet.links ~/lc-live/links.json \
  -livenet.deny ~/lc-live/deny.txt \
  -livenet.out "$OUT" \
  -livenet.marketing \
  -livenet.commit "$(git rev-parse HEAD)"
cp "$OUT"/results-*.md unshorten/testdata/livenet/
```

Check mode is the production path: `New` with the default guarded
transport, the default cache, `NewHostLimiter(1, 1)` and an `OnRequest`
collector. Per link it asserts `Match`'s entry, the outcome, the last
request's strategy and status, the final URL's host and path, the hops when
given, and that a failure returns `Clean(input)` and `Clean(URL) == URL`
holds. The test fails if any case fails; the results file is written
either way.

The results file names entries, outcomes, statuses and destination hosts
only; the suite refuses to write it if it holds a token from the links file
or anything the refusals above catch. Read it before committing it.

## After the check

The results' **Gating** table says, per registry entry, whether the run
proved it: a passing good case, a passing invalid case (not required of
Intermediate entries), no failing case, and every one of its hosts answering
in a passing case. Every unproven entry, or the patterns of its unreached
hosts, is removed from `DefaultRegistry`, from doc.go's host list
(`TestPackageDocHosts`) and from the README, and listed in doc.go's
`# Not registered by default` with a `With` example. tinyurl and bit.ly ship
unregistered at v0.3.0 whatever their rows say. v0.3.0 is tagged only after
this run and that pruning.

What the suite cannot see, and needs checking by hand:

- bit.ly and tinyurl register no click: the link's analytics after the run
  show none;
- fb.me answers a 3xx, not 200 with a meta refresh (the outcome shows it: a
  200 is `no_redirect`), and the TikTok removed-video behaviour (a case with
  a deleted video's link);
- amzn.eu and amzn.asia: the destination host in the results names the TLD.

Afterwards, run check mode by hand about monthly, and commit the results
when something changed.
