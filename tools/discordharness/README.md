# discordharness

The private-channel harness for `discordlink`. It settles the grammar points
`discordlink/testdata/find.txtar` marks `// unverified:` and checks the reply
format `Repost` produces, by sending messages to a private Discord channel
and recording what Discord does with them.

It has two halves:

- `discordlink/harness_payloads_test.go` (root module, test-only, no network)
  builds every message and checks it. With `-harness-out` it writes them to
  `payloads.json`: per case its ID, phase, content, flags, reply target and
  `Find`'s prediction.
- `tools/discordharness` (this directory, standard library only) sends
  `payloads.json` through Discord's REST API v10, reads every message back,
  and writes `run.json` and `observations.md`.

CI runs both halves' tests (the sender's against `httptest` servers only);
it never sends anything.

## What it sends

- **Phase G** (`grammar`): one message per case, `G01`, `G02`, …, whose first
  line is the case ID. Controls (a bare link, `<url>`, a masked link, a
  spoiler), every `find.txtar` row carrying a marker, the rows added for
  the grammar details, and two oracle rows per rule class. Test hosts are
  rewritten to `example.com/<host>`; cases whose answer shows in the embed
  link to this repository's GitHub page with a per-case query string, or to
  `en.wikipedia.org/wiki/Mercury_(planet)` for the balanced-`)` cases.
- **Phase R** (`reply`): an original message, then replies to it with bodies
  from `discordlink.Repost` and an `Annotate` shaped like a cleaning bot's
  reply: subtext lines, a spoilered subtext line, a masked
  `[search](<https://duckduckgo.com/?q=…>)` link (with and without
  `SUPPRESS_EMBEDS`), a `%XX`-only search target, and a spoilered URL line
  with an encoded `||`.
- **Phase B** (`bidi`): one raw message with a U+202E on its first line, to
  see whether it reorders the second.

Every message is sent with `allowed_mentions` parsing nothing, so nothing
pings anyone.

## Setup

You need, once:

1. **A bot token for a test application you control.** Never a production
   bot's token. The bot needs no gateway connection and no privileged
   intents: the harness only uses REST.
2. **A private text channel** in a server (guild) you control. Deny
   `@everyone` the **View Channel** permission on the channel; the sender
   refuses any channel whose `@everyone` overwrite does not deny it.
3. **The bot's permissions in that channel:** View Channel, Send Messages,
   Read Message History and Embed Links (the unfurl cases need it). Not
   Manage Messages: a bot can delete its own messages without it.
4. **The token in a file of its own**, readable by you only and outside every
   git work tree, for example:

   ```sh
   mkdir -p ~/.config/linkclean-harness
   chmod 700 ~/.config/linkclean-harness
   ( umask 077 && cat > ~/.config/linkclean-harness/token )   # paste the token, then Ctrl-D
   ```

   The sender refuses a token file with any group or other permission bit,
   or inside a git work tree. Never pass the token as an argument or an
   environment variable; the sender has no way to accept one.
5. **The guild and channel IDs**: in Discord, turn on Developer Mode
   (Settings, Advanced), then right-click the server and the channel and
   choose Copy ID.

## Run

From the repository root, in an interactive terminal (the sender asks for
confirmation on `/dev/tty` and refuses without one):

```sh
DIR=$(mktemp -d)    # absolute, outside any git work tree
go test ./discordlink -run '^TestHarnessPayloads$' -harness-out "$DIR"
go -C tools run ./discordharness -payloads "$DIR/payloads.json" \
    -token-file ~/.config/linkclean-harness/token \
    -guild <guild ID> -channel <channel ID> -phase all -out "$DIR"
```

`-phase` is `grammar`, `reply`, `bidi` or `all`. Each `-out` directory holds
one run: to run phases separately, give each its own `-out`.

The sender then:

1. checks the token file and `-out`;
2. runs the preflight: `GET /users/@me` must be a bot, and
   `GET /channels/<channel ID>` must be a text channel of `-guild` whose
   `@everyone` overwrite denies View Channel;
3. shows the bot, server, channel and message count, and waits for you to
   type the channel name;
4. sends each case to `-channel` only, at least 1.2 s apart, honouring
   `429` `Retry-After`, and records each message in `$DIR/run.json` as soon
   as it exists;
5. reads every message back until it has an embed or 20 s have passed since
   its send, and checks: the content came back unchanged, the flags sent
   are set, and the reply cases have no embed;
6. writes `$DIR/observations.md`.

## Observe

Open `$DIR/observations.md` and fill in the empty cells: the desktop client
for every row; one mobile client too (and the web client if convenient) for
the rows marked "user copy" and for Phases R and B. For each case, note
exactly which characters are clickable (hover, or long-press, shows the
target), whether an embed appears, whether spoilered content stays hidden,
and whether the "leaving Discord" prompt shows the exact target. For Phase R
and B, the table names what each case checks.

For the marker cases, the file's "User copies" section holds each message's
content: paste it by hand, from your own user account in a normal client (no
automation), and note whether it renders as the bot's message does.

## Record the results

Copy the filled table to `tools/discordharness/results/<date>.md` and commit
it. It must hold no Discord IDs, names, tokens or local paths;
`observations.md` is written without them, so check anything you add. Never
commit `run.json` or `payloads.json`.

Then each `// unverified:` marker in `discordlink/testdata/find.txtar` is
either removed or rewritten as `// verified <date> <case>: <note>` with the
expectation set to what was observed, and `discordlink`'s doc comment
(`Unverified grammar`) is updated to match. A grammar change the results
force goes in its own commit with its own golden diff.

## Clean up

```sh
go -C tools run ./discordharness -cleanup \
    -token-file ~/.config/linkclean-harness/token \
    -guild <guild ID> -channel <channel ID> -out "$DIR"
```

It deletes only the message IDs recorded in `$DIR/run.json`, in that run's
channel, after the same preflight and confirmation, and only when the token's
bot is the one that sent them. It marks each deleted message in `run.json`,
so it can be re-run after an interruption.
