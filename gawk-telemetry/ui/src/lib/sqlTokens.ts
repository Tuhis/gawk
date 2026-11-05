// The SQL console's tokenizer (R36, docs/86 TU10): just enough to colour
// keywords, functions, strings, numbers and comments. Nothing is parsed; the
// engine has the last word on Run.

const KEYWORDS = new Set(
  (
    'select from where group by order limit offset as and or not in is null join on left right inner outer full ' +
    'cross desc asc interval case when then else end distinct having with union all between like ilike over ' +
    'partition window qualify true false exists cast filter using natural lateral values day days hour hours ' +
    'minute minutes second seconds month year epoch_ms'
  ).split(' '),
);


export type Tok = { t: 'kw' | 'fn' | 'st' | 'nu' | 'cm' | 'tx'; s: string };

export function tokenize(src: string): Tok[] {
  const out: Tok[] = [];
  const re = /(--[^\n]*)|('(?:[^']|'')*'?)|("(?:[^"]|"")*"?)|(\d+(?:\.\d+)?)|([A-Za-z_][A-Za-z0-9_]*)|(\s+|[^\sA-Za-z0-9_'"-]+|-)/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(src)) !== null) {
    if (m[1]) out.push({ t: 'cm', s: m[1] });
    else if (m[2]) out.push({ t: 'st', s: m[2] });
    else if (m[3]) out.push({ t: 'tx', s: m[3] });
    else if (m[4]) out.push({ t: 'nu', s: m[4] });
    else if (m[5]) {
      const rest = src.slice(re.lastIndex);
      if (/^\s*\(/.test(rest)) out.push({ t: 'fn', s: m[5] });
      else out.push({ t: KEYWORDS.has(m[5].toLowerCase()) ? 'kw' : 'tx', s: m[5] });
    } else out.push({ t: 'tx', s: m[0] });
  }
  return out;
}
