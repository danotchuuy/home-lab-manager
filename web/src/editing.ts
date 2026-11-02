/** Text edits for the file editor, separate from the textarea so they're easy to reason about. */

/** Replace value[from:to] with insert, then select selStart..selEnd. */
export interface Edit {
  from: number;
  to: number;
  insert: string;
  selStart: number;
  selEnd: number;
}

/** A change at the start of a line: by > 0 added, by < 0 removed. */
interface Shift {
  at: number;
  by: number;
}

const DEFAULT_SPACES = 2;

/**
 * What one Tab inserts. Follows the file's existing indentation when it has
 * any. YAML doesn't allow tabs for indentation, so it always gets spaces.
 */
export function detectIndent(text: string, path: string): string {
  const yaml = /\.ya?ml$/i.test(path);
  let tabs = 0;
  let spaced = 0;
  let previous = 0;
  const steps = new Map<number, number>();
  for (const line of text.split("\n")) {
    if (!line.trim()) continue;
    if (line[0] === "\t") {
      tabs++;
      continue;
    }
    const width = line.length - line.trimStart().length;
    if (width > 0) spaced++;
    const step = Math.abs(width - previous);
    // A step of 1 is usually alignment, as in " * " comment lines.
    if (step >= 2 && step <= 8) steps.set(step, (steps.get(step) ?? 0) + 1);
    previous = width;
  }
  if (tabs > spaced && !yaml) return "\t";
  if (spaced > 0 && steps.size > 0) {
    const [step] = [...steps].reduce((a, b) => (b[1] > a[1] ? b : a));
    return " ".repeat(step);
  }
  return yaml || /\.json$/i.test(path) ? " ".repeat(DEFAULT_SPACES) : "\t";
}

/** Where the line containing pos starts. */
function lineStart(value: string, pos: number): number {
  return pos === 0 ? 0 : value.lastIndexOf("\n", pos - 1) + 1;
}

/** The lines a selection touches, as [from, to) without the final newline. */
function selectedLines(value: string, start: number, end: number): { from: number; to: number } {
  // A selection ending at column 0 doesn't touch that line: selecting whole
  // lines with Shift+Down puts the end there.
  const last = end > start && value[end - 1] === "\n" ? end - 1 : end;
  const next = value.indexOf("\n", last);
  return { from: lineStart(value, start), to: next < 0 ? value.length : next };
}

/** Where pos ends up after the shifts. A position inside removed indentation moves to the line's start. */
function shifted(pos: number, shifts: Shift[]): number {
  let out = pos;
  for (const { at, by } of shifts) {
    if (at >= pos) break;
    out += by > 0 ? by : -Math.min(-by, pos - at);
  }
  return out;
}

/** Applies change to each touched line, keeping the same text selected. Undefined if nothing changes. */
function eachLine(value: string, start: number, end: number, change: (line: string) => number | string): Edit | undefined {
  const { from, to } = selectedLines(value, start, end);
  const shifts: Shift[] = [];
  const out: string[] = [];
  let at = from;
  for (const line of value.slice(from, to).split("\n")) {
    const c = change(line);
    if (typeof c === "string") {
      if (c) shifts.push({ at, by: c.length });
      out.push(c + line);
    } else {
      if (c) shifts.push({ at, by: -c });
      out.push(line.slice(c));
    }
    at += line.length + 1;
  }
  if (shifts.length === 0) return undefined;
  return { from, to, insert: out.join("\n"), selStart: shifted(start, shifts), selEnd: shifted(end, shifts) };
}

/**
 * Tab. Indents every selected line, or inserts one indent at the cursor when
 * nothing is selected. A selection on one line is indented too rather than
 * replaced, so Tab can't delete selected text.
 */
export function indent(value: string, start: number, end: number, unit: string): Edit | undefined {
  if (start === end) {
    const caret = start + unit.length;
    return { from: start, to: end, insert: unit, selStart: caret, selEnd: caret };
  }
  return eachLine(value, start, end, (line) => (line ? unit : ""));
}

/** Shift+Tab. Removes one level of indentation from every selected line, or from the cursor's line. */
export function outdent(value: string, start: number, end: number, unit: string): Edit | undefined {
  const width = unit === "\t" ? 4 : unit.length;
  return eachLine(value, start, end, (line) => {
    if (line[0] === "\t") return 1;
    let n = 0;
    while (n < width && line[n] === " ") n++;
    return n;
  });
}

/** Enter. Starts the new line at the current line's indentation. */
export function newline(value: string, start: number, end: number): Edit {
  const from = lineStart(value, start);
  const lead = value.slice(from, start).match(/^[ \t]*/)![0];
  const insert = "\n" + lead;
  const caret = start + insert.length;
  return { from: start, to: end, insert, selStart: caret, selEnd: caret };
}

/** How many characters into value the start of a 1-based line is. */
export function offsetOfLine(value: string, line: number): number {
  let at = 0;
  for (let i = 1; i < line; i++) {
    const next = value.indexOf("\n", at);
    if (next < 0) return at;
    at = next + 1;
  }
  return at;
}

/**
 * What a textarea holds for a file's text: it turns every line ending into
 * LF, so the editor works in LF and fromEditor restores the file's own endings.
 */
export function toEditor(content: string): string {
  return content.replace(/\r\n?/g, "\n");
}

/** Whether a file uses CRLF. A mix counts as whichever is more common, since the editor can't keep it per line. */
export function usesCRLF(content: string): boolean {
  const crlf = content.match(/\r\n/g)?.length ?? 0;
  const all = content.match(/\n/g)?.length ?? 0;
  return crlf > 0 && crlf * 2 >= all;
}

/** The text to save for what's in the editor. */
export function fromEditor(text: string, crlf: boolean): string {
  return crlf ? text.replace(/\n/g, "\r\n") : text;
}
