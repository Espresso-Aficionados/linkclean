# Harness results

One file per harness run, `<date>.md`: the `observations.md` the sender
wrote, with the manual observations filled in (see `../README.md`).

These files are the evidence behind every `// verified` line in
`discordlink/testdata/find.txtar`. They hold case IDs, message contents,
flags, embeds and observations only: never Discord IDs, bot, server or
channel names, tokens or local paths. `run.json` and `payloads.json` are
never committed.
