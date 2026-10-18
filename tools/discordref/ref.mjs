// ref.mjs: the oracle for discordlink.Find. It runs simple-markdown's parser
// (https://github.com/Khan/simple-markdown, MIT licence, (c) Khan Academy,
// Aria Buckles, Christopher Jeffrey) with Discord's overrides and reports the
// http(s) links Discord's client would make clickable. The rule regexes come
// from the simple-markdown package passed on the command line; the Discord
// overrides below quote Discord's published rules. See README.md.
//
// Usage (node 20 or later):
//
//	node ref.mjs <simple-markdown.js> cases <cases.json>
//	    For each string in the JSON array, print one JSON line:
//	    {"in", "raw", "want", "capped", "depth"}.
//	node ref.mjs <simple-markdown.js> oracle <find.txtar> <seed> <count>
//	    Print discordlink/testdata/oracle.txtar: every golden input of
//	    find.txtar plus <count> template-generated inputs.
//
// The package file must be simple-markdown 0.7.3's simple-markdown.js, the
// file whose sha256 is PINNED below; any other file is refused.

import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { resolve } from 'node:path';

const PINNED = {
  version: '0.7.3',
  commit: 'f5bfed64a68d2689ee9137f35679b8b6b31193b7',
  sha256: 'd2caf4a7608e292712d85bb7c95ab0b73c2089ac8a519d4642fed3b15b159046',
};

const [smPath, mode, ...args] = process.argv.slice(2);
if (!smPath || !mode) {
  console.error('usage: node ref.mjs <simple-markdown.js> cases <cases.json> | oracle <find.txtar> <seed> <count>');
  process.exit(2);
}
const smSum = createHash('sha256').update(readFileSync(smPath)).digest('hex');
if (smSum !== PINNED.sha256) {
  console.error(`ref.mjs: ${smPath}: sha256 ${smSum}, want ${PINNED.sha256} (simple-markdown ${PINNED.version})`);
  process.exit(1);
}
const SM = createRequire(import.meta.url)(resolve(smPath));
const D = SM.defaultRules;

// ---- Discord's link grammar on top of simple-markdown ----

// Bare links: discord.py's _URL_REGEX without steam://, then hypothesis B for
// ')': while the URL ends in ')' and has more ')' than '(', drop the ')' and
// re-apply the last-character rule. A URL left with fewer than two
// characters after "://" no longer matches the regex and is not a link.
const URL_DPY = /^(https?:\/\/[^\s<]+[^<.,:;"'\]\s])/;
const URL_FULL = /^https?:\/\/[^\s<]+[^<.,:;"'\]\s]$/;
function trimParens(u) {
  while (u.endsWith(')')) {
    const o = (u.match(/\(/g) || []).length, c = (u.match(/\)/g) || []).length;
    if (c <= o) break;
    u = u.slice(0, -1);
    while (u.length && /[<.,:;"'\]\s]/.test(u[u.length - 1])) u = u.slice(0, -1);
  }
  return URL_FULL.test(u) ? u : null;
}

// The ten message-formatting tokens of Discord's developer reference. None
// can contain a link; they matter because they consume '<', '_' and ':'.
const NAME = "[-_'\\p{L}\\p{N}\\p{sc=Deva}\\p{sc=Thai}]{1,32}";
const MENTIONS = [
  /^<@!?\d+>/,                                        // user
  /^<@&\d+>/,                                         // role
  /^<#\d+>/,                                          // channel
  /^<@\$\d+>/,                                        // game profile
  new RegExp(`^<\\/${NAME}(?: ${NAME}){0,2}:\\d+>`, 'u'), // slash command
  /^<a?:\w+:\d+>/,                                    // custom emoji
  /^<t:-?\d+(?::[tTdDfFsSR])?>/,                      // timestamp
  /^<\d+:(?:customize|browse|guide|linked-roles(?::\d+)?)>/, // guild navigation
];

// The masked-link regex is simple-markdown's own, compiled with indices so
// the target's offsets (and whether '<' and '>' were consumed) are exact.
const LINK_D = new RegExp(D.link.match.regex.source, 'd');

// opts: filter (L1: a label containing http(s):// fails the rule), l2
// (MaskedSuppressed needs both '<' and '>'; otherwise '<' alone suffices),
// cap (0: uncapped; else the nesting limit of Find, with its Spoiler rule).
function makeParser(opts) {
  const paired = (rule, inner) => ({
    ...rule,
    match: (src, st, prev) => (opts.cap && (st.depth || 0) >= opts.cap ? null : rule.match(src, st, prev)),
    parse: (c, parse, st) => {
      const d = st.depth || 0;
      st.depth = d + 1;
      const node = { content: parse(inner(c), st) };
      node.capSpoiler = opts.cap > 0 && d + 1 === opts.cap && inner(c).includes('||');
      st.depth = d;
      return node;
    },
  });
  const rules = {
    codeBlock: {
      order: D.codeBlock.order,
      match: SM.anyScopeRegex(/^```(?:([a-z0-9_+\-.#]+?)\n)?\n*([^\n][^]*?)\n*```/i),
      parse: () => ({ content: '' }),
    },
    escape: D.escape,
    autolink: {
      order: D.autolink.order,
      match: D.autolink.match,
      parse: c => ({ type: 'link', kind: 'Suppressed', target: c[1], content: [] }),
    },
    mention: {
      order: D.autolink.order + 0.5,
      match: SM.inlineRegex({ exec: src => { for (const r of MENTIONS) { const m = r.exec(src); if (m) return m; } return null; } }),
      parse: () => ({ content: '' }),
    },
    url: {
      order: D.url.order,
      match: (src, st) => {
        if (!st.inline) return null;
        const m = URL_DPY.exec(src);
        if (!m) return null;
        const u = trimParens(m[1]);
        return u === null ? null : [u, u];
      },
      parse: c => ({ type: 'link', kind: 'Bare', target: c[1], content: [] }),
    },
    link: {
      order: D.link.order,
      match: (src, st) => {
        if (!st.inline) return null;
        const c = LINK_D.exec(src);
        if (!c) return null;
        if (opts.filter && /https?:\/\//.test(c[1])) return null;
        return c;
      },
      parse: c => {
        const [s, e] = c.indices[2];
        const lt = c[0][s - 1] === '<';
        const gt = c[0][e] === '>';
        const suppressed = opts.l2 ? lt && gt : lt;
        // The target is reported raw (content[Start:End]), not unescaped.
        return { type: 'link', kind: suppressed ? 'MaskedSuppressed' : 'Masked', target: c[2], label: c[1], content: [] };
      },
    },
    em: paired(D.em, c => c[2] || c[1]),
    strong: paired(D.strong, c => c[1]),
    u: paired(D.u, c => c[1]),
    // Discord's strikethrough.
    del: paired({ order: D.del.order, match: SM.inlineRegex(/^~~([\s\S]+?)~~(?!_)/) }, c => c[1]),
    inlineCode: { order: D.inlineCode.order, match: D.inlineCode.match, parse: () => ({ content: '' }) },
    spoiler: paired({ order: D.text.order, match: SM.inlineRegex(/^\|\|([\s\S]+?)\|\|/) }, c => c[1]),
    // Discord's text rule: simple-markdown's, breaking at every newline.
    text: { order: D.text.order, match: SM.anyScopeRegex(/^[\s\S]+?(?=[^0-9A-Za-z\s\u00c0-\uffff]|\n\n|\n|\w+:\S|$)/), parse: c => ({ content: c[0] }) },
  };
  const parse = SM.parserFor(rules);
  const PAIRED = new Set(['em', 'strong', 'u', 'del', 'spoiler']);
  // walk collects links in order. Links inside a masked link's label are
  // never reported (the label is not walked).
  function walk(nodes, spoiler, out) {
    let depth = 0;
    for (const n of nodes) {
      if (n.type === 'link') {
        if (/^https?:\/\//.test(n.target)) out.push({ kind: n.kind, url: n.target, spoiler, label: n.label ?? '' });
        continue;
      }
      if (Array.isArray(n.content)) {
        const d = walk(n.content, spoiler || n.type === 'spoiler' || n.capSpoiler, out) + (PAIRED.has(n.type) ? 1 : 0);
        depth = Math.max(depth, d);
      }
    }
    return depth;
  }
  return src => {
    const out = [];
    const depth = walk(parse(src, { inline: true }), false, out);
    return { links: out, depth };
  };
}

const rawParse = makeParser({ filter: false, l2: false, cap: 0 });
const wantParse = makeParser({ filter: true, l2: true, cap: 0 });
const cappedParse = makeParser({ filter: true, l2: true, cap: 16 });

function run(src) {
  const raw = rawParse(src), want = wantParse(src), capped = cappedParse(src);
  return { in: src, raw: raw.links, want: want.links, capped: capped.links, depth: want.depth };
}

// ---- Go string literals ----

function goQuote(s) {
  let q = '"';
  for (const ch of s) {
    const c = ch.codePointAt(0);
    if (ch === '"') q += '\\"';
    else if (ch === '\\') q += '\\\\';
    else if (ch === '\n') q += '\\n';
    else if (ch === '\t') q += '\\t';
    else if (ch === '\r') q += '\\r';
    else if (c < 0x20 || c === 0x7f) q += '\\x' + c.toString(16).padStart(2, '0');
    else if (c < 0x7f) q += ch;
    else if (c <= 0xffff) q += '\\u' + c.toString(16).padStart(4, '0');
    else q += '\\U' + c.toString(16).padStart(8, '0');
  }
  return q + '"';
}

// goUnquoteExpr parses find.txtar's string expressions ("a"*3 + "b"). It
// returns null for strings JS cannot hold (invalid UTF-8 from \x or octal
// escapes of 0x80 and up).
function goUnquoteExpr(s) {
  let out = '', i = 0;
  for (;;) {
    let v = '';
    if (s[i] === '`') {
      const j = s.indexOf('`', i + 1);
      v = s.slice(i + 1, j);
      i = j + 1;
    } else if (s[i] === '"') {
      const bytes = [];
      const pushStr = t => bytes.push(...Buffer.from(t, 'utf8'));
      i++;
      while (s[i] !== '"') {
        if (s[i] !== '\\') { const cp = s.codePointAt(i); const ch = String.fromCodePoint(cp); pushStr(ch); i += ch.length; continue; }
        const e = s[i + 1];
        const simple = { n: '\n', t: '\t', r: '\r', a: '\x07', b: '\b', f: '\f', v: '\v', '\\': '\\', '"': '"', "'": "'" };
        if (e in simple) { pushStr(simple[e]); i += 2; }
        else if (e === 'x') { bytes.push(parseInt(s.slice(i + 2, i + 4), 16)); i += 4; }
        else if (e === 'u') { pushStr(String.fromCodePoint(parseInt(s.slice(i + 2, i + 6), 16))); i += 6; }
        else if (e === 'U') { pushStr(String.fromCodePoint(parseInt(s.slice(i + 2, i + 10), 16))); i += 10; }
        else if (/[0-7]/.test(e)) { bytes.push(parseInt(s.slice(i + 1, i + 4), 8)); i += 4; }
        else throw new Error(`bad escape in ${s}`);
      }
      i++;
      const buf = Buffer.from(bytes);
      v = buf.toString('utf8');
      if (!Buffer.from(v, 'utf8').equals(buf)) return null;
    } else throw new Error(`bad string expression ${s}`);
    let n = 1;
    const m = /^\*(\d+)/.exec(s.slice(i));
    if (m) { n = +m[1]; i += m[0].length; }
    out += v.repeat(n);
    if (s.startsWith(' + ', i)) { i += 3; continue; }
    return out;
  }
}

function fmtLink(l) {
  let s = `${l.kind} ${goQuote(l.url)}`;
  if (l.spoiler) s += ' spoiler';
  if (l.label !== '') s += ` label=${goQuote(l.label)}`;
  return s;
}

// ---- The template generator ----

function mulberry32(a) {
  return () => {
    a |= 0; a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

const SCHEMES = ['https://', 'https://', 'http://', 'HTTPS://'];
const HOSTS = ['a.com', 'b.org/p', 'x.io/a_b', 'c.net/(z)', 'd.com/?q=1&r=2', 'e.co/%7C', 'f.com/x.', 'g.com/\u00e9'];
const DELIMS = ['(', ')', '[', ']', '<', '>', '||', '**', '__', '~~', '*', '_', '\\', '`', '```', '"', "'", '](', '](<'];
const MISC = ['<@1>', '<:a:1>', '<t:1:R>', '1', '#', '-', ' ', ' ', '\n', '\u00a0', '\u200b', '\u00e9', 'word', 'a'];

function generate(rand, count, seen) {
  const pick = a => a[Math.floor(rand() * a.length)];
  const rows = [];
  let tries = 0;
  while (rows.length < count) {
    if (++tries > count * 50) throw new Error('generator: too many rejected rows');
    const k = 2 + Math.floor(rand() * 9);
    let s = '';
    for (let j = 0; j < k; j++) {
      const r = rand();
      if (r < 0.3) s += pick(SCHEMES) + pick(HOSTS) + (rand() < 0.4 ? pick(DELIMS) : '');
      else if (r < 0.75) s += pick(DELIMS);
      else s += pick(MISC);
    }
    if (seen.has(s)) continue;
    const res = run(s);
    // Rows deeper than 15 would meet Find's nesting limit; the oracle is
    // uncapped, so keep only rows where the limit cannot matter.
    if (res.depth > 15) continue;
    seen.add(s);
    rows.push(res);
  }
  return rows;
}

function oracle(findTxtar, seed, count) {
  const goldens = [];
  for (const line of readFileSync(findTxtar, 'utf8').split('\n')) {
    if (!line.startsWith('in ')) continue;
    const v = goUnquoteExpr(line.slice(3));
    if (v !== null) goldens.push(v);
  }
  const seen = new Set();
  const rows = [];
  for (const g of goldens) {
    // simple-markdown's parse first turns tabs into spaces, CR LF and CR
    // into LF and drops form feeds; Find parses content as sent, so such
    // rows have no comparable answer.
    if (seen.has(g) || /[\t\r\f]/.test(g)) continue;
    const res = run(g);
    if (res.depth > 15) continue; // cap rows: find.txtar pins them
    seen.add(g);
    rows.push(res);
  }
  const gen = generate(mulberry32(seed), count, seen);
  const linked = gen.filter(r => r.raw.length > 0).length;
  if (linked < 0.7 * gen.length) throw new Error(`generator: only ${linked}/${gen.length} rows have links`);
  const head = readFileSync(new URL('../../discordlink/testdata/oracle.txtar', import.meta.url), 'utf8').split('\n-- ')[0];
  let txt = head
    .replace(/^simple-markdown: .*$/m, `simple-markdown: version ${PINNED.version}, commit ${PINNED.commit}, simple-markdown.js sha256 ${PINNED.sha256}`)
    .replace(/^generator seed: .*$/m, `generator seed: ${seed}, ${rows.length} golden rows, ${gen.length} generated rows (${linked} with links)`);
  txt = txt.replace(/\n*$/, '\n\n');
  let id = 0;
  for (const r of [...rows, ...gen]) {
    id++;
    txt += `-- ${id < rows.length + 1 ? 'g' : 't'}${String(id).padStart(4, '0')} --\n`;
    txt += `in ${goQuote(r.in)}\n`;
    for (const l of r.raw) txt += `raw ${fmtLink(l)}\n`;
    const same = r.raw.length === r.want.length && r.raw.every((l, i) => fmtLink(l) === fmtLink(r.want[i]));
    if (same) { if (r.raw.length) txt += 'want same\n'; }
    else if (r.want.length === 0) txt += 'want none\n';
    else for (const l of r.want) txt += `want ${fmtLink(l)}\n`;
  }
  process.stdout.write(txt);
}

if (mode === 'cases') {
  for (const c of JSON.parse(readFileSync(args[0], 'utf8'))) {
    const r = run(c);
    console.log(JSON.stringify({ ...r, raw: r.raw.map(fmtLink), want: r.want.map(fmtLink), capped: r.capped.map(fmtLink) }));
  }
} else if (mode === 'oracle') {
  oracle(args[0], +args[1], +args[2]);
} else {
  console.error(`ref.mjs: unknown mode ${mode}`);
  process.exit(2);
}
