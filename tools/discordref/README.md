# discordref

`ref.mjs` is the oracle behind `discordlink/testdata/oracle.txtar` and the
expectations in `discordlink/testdata/find.txtar`. It runs
[simple-markdown](https://github.com/Khan/simple-markdown), the parser
Discord's client is built on, with Discord's rule overrides, and prints the
http(s) links the client would make clickable. CI never runs it: the Go tests
read its committed output.

## Pinned input

| | |
|---|---|
| package | `simple-markdown` 0.7.3 (npm) |
| commit | `f5bfed64a68d2689ee9137f35679b8b6b31193b7` (the release's `gitHead`) |
| tarball | `https://registry.npmjs.org/simple-markdown/-/simple-markdown-0.7.3.tgz`, sha1 `e32150b2ec6f8287197d09869fd928747a9c5640`, sha256 `be733db62438d05bc59dd726ca300fd8faa1a87ef9c0e86c649dd754683b54fe` |
| file | `package/simple-markdown.js`, sha256 `d2caf4a7608e292712d85bb7c95ab0b73c2089ac8a519d4642fed3b15b159046` |

The package's `index.js` only re-exports `simple-markdown.js`, so the pin is
on `simple-markdown.js`. `ref.mjs` checks its sha256 and refuses any other
file.

simple-markdown is MIT-licensed, copyright (c) 2018 Khan Academy, Aria
Buckles and Christopher Jeffrey. `ref.mjs` loads its rules from the package
rather than copying them; the regexes quoted in `ref.mjs` and in
`discordlink`'s comments are simple-markdown's or Discord's.

## What it models

simple-markdown's default inline rules (`escape`, `autolink`, `url`, `link`,
`em`, `strong`, `u`, `inlineCode`, `text`) with these changes:

- Discord's rules: the ```` ``` ```` fence, `||spoiler||`, `~~del~~` with its
  `(?!_)`, and a text rule that breaks at every newline.
- Discord's ten message-formatting tokens (`<@id>`, `<@!id>`, `<@&id>`,
  `<#id>`, `<@$id>`, `</name sub:id>`, `<:name:id>`, `<a:name:id>`,
  `<t:ts[:style]>`, `<id:type>`), consumed as non-links.
- Bare links use discord.py's `_URL_REGEX` without `steam://`, then drop a
  trailing `)` while the link has more `)` than `(` (unverified point U1).
- Masked-link targets are reported raw, as `content[Start:End]`, not
  unescaped.
- Links inside a masked link's label are not reported.

Each input is parsed three ways:

- `raw`: as above.
- `want`: also with Find's documented choices: a label containing `http://`
  or `https://` makes the masked rule fail (L1), and a masked link is
  `MaskedSuppressed` only when both `<` and `>` around the target were
  consumed (L2).
- `capped`: `want` with Find's nesting limit of 16 paired rules: past it the
  paired rules are not tried, and the links in a span at the limit are
  spoilers if the span contains `||`.

simple-markdown rewrites its input before parsing (tabs to four spaces, CR LF
and CR to LF, form feeds dropped). Find parses content as sent, so the
`oracle` mode skips golden inputs containing a tab, CR or form feed.

## Usage

Needs node 20 or later and nothing else. Fetch the pinned package into a
directory of your choice and check it:

```sh
curl -fsSLo <dir>/simple-markdown-0.7.3.tgz https://registry.npmjs.org/simple-markdown/-/simple-markdown-0.7.3.tgz
tar -xzf <dir>/simple-markdown-0.7.3.tgz -C <dir>
shasum -a 256 <dir>/package/simple-markdown.js
```

Answers for a JSON array of strings, one JSON line each:

```sh
node tools/discordref/ref.mjs <dir>/package/simple-markdown.js cases <cases.json>
```

Regenerate the oracle corpus from the repository root (the seed and the
generated-row count are recorded in the file's header):

```sh
node tools/discordref/ref.mjs <dir>/package/simple-markdown.js oracle \
  discordlink/testdata/find.txtar <seed> <count> > <tmp>/oracle.txtar
mv <tmp>/oracle.txtar discordlink/testdata/oracle.txtar
go test ./discordlink -run 'TestFindOracle|TestFindGolden'
```

The output is deterministic for a given `find.txtar`, seed and count. The
`oracle` mode takes every `in` line of `find.txtar` (not shapes, and not
inputs JavaScript cannot hold, such as invalid UTF-8) and adds `<count>`
template-generated rows, keeping only rows that nest at most 15 paired rules
deep, so the nesting limit cannot change their answer. It fails if fewer
than 70% of the generated rows have a link in the `raw` answer.

A harness result that flips L1 or L2 changes `want`; flip the option in
`ref.mjs` and regenerate. A result that changes U1 or the grammar itself
changes `ref.mjs` and every column.
