/**
 * Checks YAML away from the page: a large file takes seconds to parse, which
 * would freeze typing. Receives the text and answers with its problems.
 */
import { parseAllDocuments } from "yaml";
import type { Problem } from "./validate";

self.onmessage = (e: MessageEvent<string>) => {
  const docs = parseAllDocuments(e.data, { logLevel: "silent" });
  // Unknown tags like Home Assistant's !secret are warnings, so they pass.
  const problems: Problem[] = [...docs].flatMap((d) =>
    d.errors.map((err) => ({
      message: err.message.split("\n")[0].replace(/ at line \d+, column \d+:$/, ""),
      line: err.linePos?.[0].line,
    })),
  );
  self.postMessage(problems);
};
