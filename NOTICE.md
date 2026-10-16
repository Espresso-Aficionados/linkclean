# Notices

linkclean is licensed under the MIT License (see `LICENSE`), except for the
following data files, which are licensed under the Mozilla Public License 2.0
(MPL-2.0):

- `rules/brave/` (`clean-urls.json`, `query-filter.json`, `debounce.json`):
  copied verbatim from [brave/adblock-lists](https://github.com/brave/adblock-lists).
  `rules/brave/NOTICE` names the upstream commit and the hash of every file;
  `rules/brave/LICENSE` is the upstream license text at that commit.
- `rules/overlay/debounce-classes.json`: written for this project, but its keys
  quote the upstream `debounce.json` patterns, so it carries the same license.
  The notice is in `rules/overlay/debounce-classes.json.license`.

These files are embedded in the `linkclean` package. A program that
distributes a binary built with it must tell recipients where the MPL-covered
source is (MPL-2.0 §3.2): this module's source, `rules/brave/NOTICE` and this
file suffice. `Cleaner.Notices()` returns the same attribution at run time.
