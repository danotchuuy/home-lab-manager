/**
 * Colors for the file editor. Each line is tokenized on its own, which keeps
 * it simple and fast but means something spanning lines, like a multi-line
 * XML comment or a YAML block of text, can be colored as if it were settings.
 * Joining a line's tokens always gives back the line exactly.
 */

export type Syntax = "json" | "yaml" | "xml" | "ini";
export type TokenKind = "key" | "string" | "number" | "literal" | "comment" | "punct" | "section";

export interface Token {
  text: string;
  kind?: TokenKind;
}

/** The largest file colored: rendering every token is too slow for typing beyond this. */
export const MAX_COLOR_SIZE = 256 * 1024;

/**
 * The longest line colored. Each colored piece rounds its width slightly, so on
 * a very long line, like minified JSON, the colors would drift from the real
 * text and the cursor. They'd be about 2px off by this length.
 */
const MAX_COLOR_LINE = 1000;

export function syntaxOf(path: string): Syntax | undefined {
  if (/\.json[c5]?$/i.test(path)) return "json";
  if (/\.ya?ml$/i.test(path)) return "yaml";
  if (/\.xml$/i.test(path)) return "xml";
  if (/\.(ini|cfg|conf|cnf|properties|toml|env)$/i.test(path)) return "ini";
  return undefined;
}

const NUMBER = /^[-+]?(\d[\d_]*)?(\.\d+)?([eE][-+]?\d+)?$/;
const LITERAL = /^(true|false|yes|no|on|off|null|~)$/i;

/** The kind of a whole value. */
function valueKind(value: string): TokenKind {
  if (NUMBER.test(value) && /\d/.test(value)) return "number";
  if (LITERAL.test(value)) return "literal";
  return "string";
}

/** Where a " #" comment starts in s, skipping quoted text; -1 if there's none. */
function commentStart(s: string): number {
  let quote = "";
  for (let i = 0; i < s.length; i++) {
    const c = s[i];
    if (quote) {
      if (c === "\\" && quote === '"') i++;
      else if (c === quote) quote = "";
    } else if (c === '"' || c === "'") {
      // An apostrophe inside a word doesn't open a quote.
      if (i === 0 || /\s/.test(s[i - 1])) quote = c;
    } else if (c === "#" && (i === 0 || /\s/.test(s[i - 1]))) {
      return i;
    }
  }
  return -1;
}

/** A value with any surrounding spaces and trailing comment split out. */
function valueTokens(s: string, valueOf: (value: string) => TokenKind = valueKind): Token[] {
  const hash = commentStart(s);
  const body = hash < 0 ? s : s.slice(0, hash);
  const value = body.trim();
  if (!value) return hash < 0 ? [{ text: s }] : [{ text: body }, { text: s.slice(hash), kind: "comment" }];
  const lead = body.length - body.trimStart().length;
  const out: Token[] = [];
  if (lead) out.push({ text: body.slice(0, lead) });
  out.push({ text: value, kind: valueOf(value) });
  const tail = body.slice(lead + value.length);
  if (tail) out.push({ text: tail });
  if (hash >= 0) out.push({ text: s.slice(hash), kind: "comment" });
  return out;
}

/** Tokens for what a regex finds in line, with what's between them from gap. */
function scan(line: string, re: RegExp, kindOf: (match: string, at: number) => TokenKind | undefined, gap: (text: string) => Token[] = (text) => [{ text }]): Token[] {
  const out: Token[] = [];
  let at = 0;
  for (const m of line.matchAll(re)) {
    if (!m[0]) continue;
    if (m.index > at) out.push(...gap(line.slice(at, m.index)));
    out.push({ text: m[0], kind: kindOf(m[0], m.index) });
    at = m.index + m[0].length;
  }
  if (at < line.length) out.push(...gap(line.slice(at)));
  return out;
}

const JSON_TOKEN = /"(?:[^"\\]|\\.)*"?|'(?:[^'\\]|\\.)*'?|\/\/.*|-?\d+(?:\.\d+)?(?:[eE][-+]?\d+)?|\b(?:true|false|null)\b|[A-Za-z_$][\w$]*(?=\s*:)|[{}[\],:]/g;

function jsonLine(line: string): Token[] {
  return scan(line, JSON_TOKEN, (m, at) => {
    const c = m[0];
    if (m.startsWith("//")) return "comment";
    if ("{}[],:".includes(c)) return "punct";
    if (c === '"' || c === "'") return /^\s*:/.test(line.slice(at + m.length)) ? "key" : "string";
    if (/[\d-]/.test(c)) return "number";
    return m === "true" || m === "false" || m === "null" ? "literal" : "key";
  });
}

const YAML_KEY = /^("(?:[^"\\]|\\.)*"|'[^']*'|[^\s#'"[\]{}&*!|>%@`][^#]*?)(\s*:)(?=\s|$)/;

function yamlValue(value: string): TokenKind {
  if (/^[&*!]/.test(value)) return "literal";
  if (/^[|>][-+\d]*$/.test(value)) return "punct";
  return valueKind(value);
}

function yamlLine(line: string): Token[] {
  const [, indent, dashes, rest] = line.match(/^(\s*)((?:- +|-$)*)(.*)$/s)!;
  const out: Token[] = [];
  if (indent) out.push({ text: indent });
  if (dashes) out.push({ text: dashes, kind: "punct" });
  if (!rest) return out;
  if (/^(---|\.\.\.)(\s|$)/.test(rest) && !indent && !dashes) return [...out, { text: rest, kind: "punct" }];
  const key = rest.match(YAML_KEY);
  if (!key) return [...out, ...valueTokens(rest, yamlValue)];
  out.push({ text: key[1], kind: "key" }, { text: key[2], kind: "punct" });
  return [...out, ...valueTokens(rest.slice(key[0].length), yamlValue)];
}

function iniLine(line: string): Token[] {
  if (/^\s*[;#]/.test(line)) return [{ text: line, kind: "comment" }];
  if (/^\s*\[.*\]\s*$/.test(line)) return [{ text: line, kind: "section" }];
  const m = line.match(/^(\s*)([^\s=:][^=]*?)(\s*=\s*)(.*)$/s) ?? line.match(/^(\s*)([\w.-]+)(\s*:\s+)(.*)$/s);
  if (!m) return [{ text: line }];
  const [, indent, key, separator, value] = m;
  const out: Token[] = [];
  if (indent) out.push({ text: indent });
  out.push({ text: key, kind: "key" }, { text: separator, kind: "punct" });
  return value ? [...out, ...valueTokens(value)] : out;
}

const XML_TOKEN = /<!--.*?(?:-->|$)|<[/?!]?[\w:.-]*|[/?]?>|[\w:.-]+(?==)|"[^"]*"?|'[^']*'?|=/g;

function xmlLine(line: string): Token[] {
  return scan(
    line,
    XML_TOKEN,
    (m) => {
      if (m.startsWith("<!--")) return "comment";
      if (m[0] === "<") return "section";
      if (m === "=" || m.endsWith(">")) return "punct";
      return m[0] === '"' || m[0] === "'" ? "string" : "key";
    },
    // Text between tags is a value.
    (text) => (text.trim() ? [{ text, kind: "string" }] : [{ text }]),
  );
}

export function tokenize(line: string, syntax: Syntax): Token[] {
  if (line.length > MAX_COLOR_LINE) return [{ text: line }];
  switch (syntax) {
    case "json":
      return jsonLine(line);
    case "yaml":
      return yamlLine(line);
    case "xml":
      return xmlLine(line);
    case "ini":
      return iniLine(line);
  }
}
