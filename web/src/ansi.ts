import type { CSSProperties } from "react";

// Parses ANSI escape codes in console output into styled segments. Only
// SGR codes (colors and text styles) are kept; other escape sequences
// (cursor movement, clearing the line, window titles) are dropped.

/** Text style in effect at some point of the output. */
export interface AnsiStyle {
  fg?: number | string; // 0-255 palette index, or a CSS rgb() color
  bg?: number | string;
  bold?: boolean;
  dim?: boolean;
  italic?: boolean;
  underline?: boolean;
  strike?: boolean;
  inverse?: boolean;
}

export interface AnsiSegment {
  text: string;
  style: AnsiStyle;
}

// CSI sequences (ESC [ ... final byte), OSC sequences (ESC ] ... BEL or
// ESC \), and other two-byte escapes.
const ESCAPE = /\x1b\[([0-?]*)[ -/]*([@-~])|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?|\x1b[@-Z\\-_]?/g;
// Control characters other than tab that would show up as junk.
const CONTROL = /[\x00-\x08\x0b-\x1f\x7f]/g;

function rgb(r: number, g: number, b: number): string {
  return `rgb(${r & 255}, ${g & 255}, ${b & 255})`;
}

/** Applies an SGR parameter list (the "1;31" in ESC[1;31m) to a style. */
function applySGR(style: AnsiStyle, params: string): AnsiStyle {
  const codes = params === "" ? [0] : params.split(/[;:]/).map((p) => (p === "" ? 0 : Number(p)));
  let s = { ...style };
  for (let i = 0; i < codes.length; i++) {
    const c = codes[i];
    if (c === 0) s = {};
    else if (c === 1) s.bold = true;
    else if (c === 2) s.dim = true;
    else if (c === 3) s.italic = true;
    else if (c === 4) s.underline = true;
    else if (c === 7) s.inverse = true;
    else if (c === 9) s.strike = true;
    else if (c === 22) s.bold = s.dim = false;
    else if (c === 23) s.italic = false;
    else if (c === 24) s.underline = false;
    else if (c === 27) s.inverse = false;
    else if (c === 29) s.strike = false;
    else if (c >= 30 && c <= 37) s.fg = c - 30;
    else if (c >= 90 && c <= 97) s.fg = c - 90 + 8;
    else if (c === 39) delete s.fg;
    else if (c >= 40 && c <= 47) s.bg = c - 40;
    else if (c >= 100 && c <= 107) s.bg = c - 100 + 8;
    else if (c === 49) delete s.bg;
    else if (c === 38 || c === 48) {
      // Extended colors: 38;5;n (palette) or 38;2;r;g;b (24-bit).
      let color: number | string | undefined;
      if (codes[i + 1] === 5 && i + 2 < codes.length) {
        color = codes[i + 2] & 255;
        i += 2;
      } else if (codes[i + 1] === 2 && i + 4 < codes.length) {
        color = rgb(codes[i + 2], codes[i + 3], codes[i + 4]);
        i += 4;
      } else {
        break; // malformed; ignore the rest
      }
      if (c === 38) s.fg = color;
      else s.bg = color;
    }
  }
  return s;
}

/**
 * Splits one line of output into styled segments, starting from the style
 * the previous line ended with. Returns the segments and the style at the
 * end of the line, for the next one.
 */
export function parseAnsiLine(line: string, start: AnsiStyle): { segments: AnsiSegment[]; end: AnsiStyle } {
  const segments: AnsiSegment[] = [];
  let style = start;
  let last = 0;
  const push = (text: string) => {
    text = text.replace(CONTROL, "");
    if (!text) return;
    const prev = segments[segments.length - 1];
    if (prev && prev.style === style) prev.text += text;
    else segments.push({ text, style });
  };
  ESCAPE.lastIndex = 0;
  for (let m = ESCAPE.exec(line); m; m = ESCAPE.exec(line)) {
    push(line.slice(last, m.index));
    last = ESCAPE.lastIndex;
    if (m[2] === "m") style = applySGR(style, m[1]);
  }
  push(line.slice(last));
  return { segments, end: style };
}

// The 16 standard colors, chosen to read well on the console's dark
// background, then the 6x6x6 color cube and 24 grays of the 256 palette.
const BASE16 = [
  "#5c6370", "#f0716b", "#8fd16b", "#ebcb6b", "#6fa8f7", "#d38bf0", "#5fd4d0", "#d8dee9",
  "#7a8496", "#ff8f8a", "#a9e588", "#ffe08a", "#8cbcff", "#e3a8ff", "#82e6e2", "#ffffff",
];

export function paletteColor(n: number): string {
  if (n < 16) return BASE16[n];
  if (n < 232) {
    const i = n - 16;
    const level = (v: number) => (v === 0 ? 0 : 55 + v * 40);
    return rgb(level(Math.floor(i / 36)), level(Math.floor(i / 6) % 6), level(i % 6));
  }
  const g = 8 + (n - 232) * 10;
  return rgb(g, g, g);
}

const color = (c: number | string) => (typeof c === "number" ? paletteColor(c) : c);

/** Whether a color from paletteColor or rgb() is light enough to need dark text on it. */
function isLight(c: string): boolean {
  let r: number, g: number, b: number;
  const m = c.match(/^rgb\((\d+), (\d+), (\d+)\)$/);
  if (m) [r, g, b] = [Number(m[1]), Number(m[2]), Number(m[3])];
  else if (/^#[0-9a-f]{6}$/i.test(c)) [r, g, b] = [1, 3, 5].map((i) => parseInt(c.slice(i, i + 2), 16));
  else return false;
  return 0.299 * r + 0.587 * g + 0.114 * b > 150;
}

/** CSS for a style, or undefined for the plain default. */
export function ansiCSS(s: AnsiStyle): CSSProperties | undefined {
  let fg = s.fg !== undefined ? color(s.fg) : undefined;
  let bg = s.bg !== undefined ? color(s.bg) : undefined;
  // Bold with a standard color shows its bright variant, as most terminals do.
  if (s.bold && typeof s.fg === "number" && s.fg < 8) fg = paletteColor(s.fg + 8);
  if (s.inverse) [fg, bg] = [bg ?? "var(--log-bg)", fg ?? "var(--log-text)"];
  // The default text color is light, so it would vanish on a light background.
  if (bg && !fg && isLight(bg)) fg = "var(--log-bg)";
  const css: CSSProperties = {};
  if (fg) css.color = fg;
  if (bg) css.backgroundColor = bg;
  if (s.bold) css.fontWeight = 700;
  if (s.dim) css.opacity = 0.65;
  if (s.italic) css.fontStyle = "italic";
  const deco = [s.underline && "underline", s.strike && "line-through"].filter(Boolean).join(" ");
  if (deco) css.textDecoration = deco;
  return Object.keys(css).length ? css : undefined;
}
