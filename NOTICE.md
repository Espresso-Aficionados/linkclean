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

## simple-markdown and marked.js (MIT)

`discordlink`'s comments (`discordlink/find_rules.go` and others) quote the
regular expressions of [simple-markdown](https://github.com/Khan/simple-markdown)
0.7.3, whose inline rules are adapted from
[marked.js](https://github.com/markedjs/marked) (copyright (c) 2011-2014
Christopher Jeffrey, as simple-markdown's source header credits);
`tools/discordref/ref.mjs` quotes them too. simple-markdown's `LICENSE`:

> The MIT License (MIT)
>
> Copyright (c) 2018 Khan Academy, Aria Buckles, Christopher Jeffrey
>
> Permission is hereby granted, free of charge, to any person obtaining a copy
> of this software and associated documentation files (the "Software"), to deal
> in the Software without restriction, including without limitation the rights
> to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
> copies of the Software, and to permit persons to whom the Software is
> furnished to do so, subject to the following conditions:
>
> The above copyright notice and this permission notice shall be included in all
> copies or substantial portions of the Software.
>
> THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
> IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
> FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
> AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
> LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
> OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
> SOFTWARE.
