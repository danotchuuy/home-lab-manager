// Reads key = value settings files (INI, .properties, .cfg, .env…) into
// fields that can be edited one by one. Each field records where its value
// sits in the text, so editing it changes only those characters: comments,
// order, spacing and everything else stay as they were.
//
// Unreal Engine's struct values, as Palworld keeps all its settings in one
// line like OptionSettings=(Difficulty=None,DayTimeSpeedRate=1.000000,
// ServerName="My server",…), are split into a field each.

export type ValueKind = "bool" | "number" | "text";

export interface Field {
  /** Stable while the file is edited: section, group, key and which occurrence. */
  id: string;
  key: string;
  /** The INI section, or "" before any. */
  section: string;
  /** The key of the struct this field is inside, e.g. OptionSettings. */
  group?: string;
  /** The value, without quotes. */
  value: string;
  quoted: boolean;
  /** Where the value is in the text (with its quotes, if quoted). */
  start: number;
  end: number;
  kind: ValueKind;
  /** Looks like a password or token. */
  secret: boolean;
}

/** Files the form can be used for. */
export const FORM_FILE = /\.(ini|cfg|conf|cnf|properties|env|txt)$/i;

const KEY = /^[+\-!.]?[A-Za-z_][\w.\-/[\]]*$/;
const BOOL = /^(true|false)$/i;
const NUMBER = /^[-+]?(\d+\.?\d*|\.\d+)$/;
const SECRET = /pass(word|wd)?|secret|token|api_?key|private/i;

function kindOf(value: string): ValueKind {
  if (BOOL.test(value)) return "bool";
  if (NUMBER.test(value)) return "number";
  return "text";
}

function unescape(s: string): string {
  return s.replace(/\\(["\\])/g, "$1");
}

/** Reads a struct body like Difficulty=None,ServerName="x",List=(a,b), or undefined if it isn't one. */
function parseStruct(text: string, from: number, to: number): Omit<Field, "id" | "section" | "group">[] | undefined {
  const out: Omit<Field, "id" | "section" | "group">[] = [];
  let i = from;
  const skip = () => {
    while (i < to && (text[i] === " " || text[i] === "\t")) i++;
  };
  while (i < to) {
    skip();
    const keyStart = i;
    while (i < to && text[i] !== "=" && text[i] !== "," && text[i] !== ")") i++;
    const key = text.slice(keyStart, i).trim();
    if (text[i] !== "=" || !KEY.test(key)) return undefined;
    i++; // =
    skip();
    const start = i;
    let quoted = false;
    if (text[i] === '"') {
      quoted = true;
      i++;
      while (i < to && text[i] !== '"') i += text[i] === "\\" ? 2 : 1;
      if (i >= to) return undefined; // unclosed quote
      i++;
    } else {
      let depth = 0;
      let inQuote = false;
      for (; i < to; i++) {
        const c = text[i];
        if (inQuote) {
          if (c === "\\") i++;
          else if (c === '"') inQuote = false;
        } else if (c === '"') inQuote = true;
        else if (c === "(") depth++;
        else if (c === ")") {
          if (depth === 0) return undefined;
          depth--;
        } else if (c === "," && depth === 0) break;
      }
      if (depth !== 0) return undefined;
    }
    let end = i;
    while (end > start && (text[end - 1] === " " || text[end - 1] === "\t")) end--;
    const raw = text.slice(start, end);
    const value = quoted ? unescape(raw.slice(1, -1)) : raw;
    out.push({ key, value, quoted, start, end, kind: quoted ? "text" : kindOf(value), secret: SECRET.test(key) });
    skip();
    if (i < to) {
      if (text[i] !== ",") return undefined;
      i++;
    }
  }
  return out;
}

/**
 * Reads the settings in a file. It returns undefined when the file doesn't
 * look like key = value settings (too few lines are).
 */
export function parseSettings(text: string): Field[] | undefined {
  const fields: Field[] = [];
  const seen = new Map<string, number>();
  let section = "";
  let settingLines = 0;
  let otherLines = 0;
  let pos = 0;
  const add = (f: Omit<Field, "id">) => {
    const base = `${f.section}\u0000${f.group ?? ""}\u0000${f.key}`;
    const n = seen.get(base) ?? 0;
    seen.set(base, n + 1);
    fields.push({ ...f, id: `${base}\u0000${n}` });
  };
  for (const line of text.split("\n")) {
    const lineStart = pos;
    pos += line.length + 1;
    const trimmed = line.trim();
    if (trimmed === "" || /^(;|#|\/\/)/.test(trimmed)) continue;
    const sec = /^\[([^\]]*)\]\s*$/.exec(trimmed);
    if (sec) {
      section = sec[1];
      continue;
    }
    // key = value, or key: value as .properties allow.
    const keyBefore = (at: number) => (at > 0 ? line.slice(0, at).trim().replace(/^export\s+/, "") : "");
    const eq = line.indexOf("=");
    const colon = line.indexOf(":");
    let sep = -1;
    if (KEY.test(keyBefore(eq))) sep = eq;
    else if (KEY.test(keyBefore(colon))) sep = colon;
    const key = keyBefore(sep);
    if (sep < 0) {
      otherLines++;
      continue;
    }
    settingLines++;
    let start = sep + 1;
    while (start < line.length && (line[start] === " " || line[start] === "\t")) start++;
    let end = line.length;
    while (end > start && /\s/.test(line[end - 1])) end--;
    const raw = line.slice(start, end);
    // A struct: one field per member.
    if (raw.startsWith("(") && raw.endsWith(")") && raw.length > 2) {
      const members = parseStruct(text, lineStart + start + 1, lineStart + end - 1);
      if (members && members.length > 0) {
        for (const m of members) add({ ...m, section, group: key });
        continue;
      }
    }
    const quoted = raw.length >= 2 && raw.startsWith('"') && raw.endsWith('"');
    const value = quoted ? unescape(raw.slice(1, -1)) : raw;
    add({
      key,
      section,
      value,
      quoted,
      start: lineStart + start,
      end: lineStart + end,
      kind: quoted ? "text" : kindOf(value),
      secret: SECRET.test(key),
    });
  }
  if (fields.length === 0 || settingLines < otherLines) return undefined;
  return fields;
}

/** Returns text with a field's value changed, writing it the way the file did (quoted or not). */
export function setValue(text: string, f: Field, value: string): string {
  value = value.replace(/[\r\n]/g, " ");
  let raw: string;
  // Inside a struct, a value with a separator in it has to be quoted.
  const needsQuotes = f.quoted || (f.group !== undefined && /[,()"]/.test(value) && !/^\(.*\)$/.test(value));
  if (needsQuotes) raw = `"${value.replace(/(["\\])/g, "\\$1")}"`;
  else raw = value;
  return text.slice(0, f.start) + raw + text.slice(f.end);
}

/** The other boolean, written the way the original was (True, true or TRUE). */
export function flipBool(value: string): string {
  const next = value.toLowerCase() === "true" ? "false" : "true";
  if (value === value.toUpperCase()) return next.toUpperCase();
  if (value[0] === value[0].toUpperCase()) return next[0].toUpperCase() + next.slice(1);
  return next;
}

/** A key as words: bEnablePlayerToPlayerDamage → "Enable player to player damage", RCONPort → "RCON port". */
export function humanize(key: string): string {
  let k = key.replace(/^[+\-!.]/, "");
  k = k.replace(/^b(?=[A-Z])/, ""); // Unreal's boolean prefix
  const words = k
    .replace(/([a-z\d])([A-Z])/g, "$1 $2")
    .replace(/([A-Z]+)([A-Z][a-z])/g, "$1 $2")
    .split(/[\s_.\-/]+/)
    .filter(Boolean)
    .map((w, i) => {
      if (w.length > 1 && w === w.toUpperCase()) return w; // an acronym
      const lower = w.toLowerCase();
      return i === 0 ? lower[0].toUpperCase() + lower.slice(1) : lower;
    });
  return words.join(" ") || key;
}
