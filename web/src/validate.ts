/** Syntax checks for the file editor. Files stay raw text; nothing here changes them. */

export type FileFormat = "JSON" | "YAML" | "XML";

export interface Problem {
  message: string;
  /** 1-based; missing when the browser's message doesn't give a position. */
  line?: number;
}

/** The format to check a file against. JSONC and JSON5 aren't JSON.parse-able, so they aren't checked. */
export function formatOf(path: string): FileFormat | undefined {
  if (/\.json$/i.test(path)) return "JSON";
  if (/\.ya?ml$/i.test(path)) return "YAML";
  if (/\.xml$/i.test(path)) return "XML";
  return undefined;
}

function lineAt(text: string, pos: number): number {
  let line = 1;
  for (let i = text.indexOf("\n"); i >= 0 && i < pos; i = text.indexOf("\n", i + 1)) line++;
  return line;
}

function checkJSON(text: string): Problem[] {
  try {
    JSON.parse(text);
    return [];
  } catch (e) {
    const message = (e as Error).message;
    // Each browser words this differently, and Safari gives no position.
    const lineCol = message.match(/line (\d+) column \d+/);
    const pos = message.match(/position (\d+)/);
    const line = lineCol ? Number(lineCol[1]) : pos ? lineAt(text, Number(pos[1])) : undefined;
    return [{ message: message.replace(/^JSON\.parse: /, "").replace(/( in JSON)? at position \d+.*$/, "").replace(/ at line \d+ column \d+.*$/, ""), line }];
  }
}

/** Parses in a worker of its own, so a newer edit can stop a parse that's no longer wanted. */
function checkYAML(text: string, signal: AbortSignal): Promise<Problem[]> {
  return new Promise((resolve, reject) => {
    const worker = new Worker(new URL("./yaml-worker.ts", import.meta.url), { type: "module" });
    const finish = (done: () => void) => {
      worker.terminate();
      done();
    };
    signal.addEventListener("abort", () => finish(() => reject(new Error("cancelled"))));
    worker.onmessage = (e: MessageEvent<Problem[]>) => finish(() => resolve(e.data));
    worker.onerror = () => finish(() => reject(new Error("the YAML checker failed to load")));
    worker.postMessage(text);
  });
}

function checkXML(text: string): Problem[] {
  const doc = new DOMParser().parseFromString(text, "application/xml");
  const error = doc.querySelector("parsererror");
  if (!error) return [];
  const detail = error.textContent ?? "Not well-formed XML";
  // Chrome: "error on line 3 at column 5: ...", Firefox: "Line Number 3, Column 5:".
  const line = detail.match(/line (?:number )?(\d+)/i);
  const reason = detail.match(/column \d+: *(.+)/i) ?? detail.match(/XML Parsing Error: *(.+)/i);
  // Chrome adds a sentence about rendering the page, meant for viewing XML as a page.
  const message = reason?.[1].replace(/Below is a rendering.*$/, "").trim();
  return [{ message: message || "Not well-formed XML", line: line ? Number(line[1]) : undefined }];
}

/** The problems in text, in file order; empty if it's valid. */
export async function validate(format: FileFormat, text: string, signal: AbortSignal): Promise<Problem[]> {
  switch (format) {
    case "JSON":
      return checkJSON(text);
    case "YAML":
      return checkYAML(text, signal);
    case "XML":
      return checkXML(text);
  }
}
