import { parse, stringify } from "yaml";
import type { Probes } from "./types";

/** Probes as YAML, the way a pod spec writes them. */
export function probesToYaml(p?: Probes): string {
  if (!p || (!p.startupProbe && !p.livenessProbe && !p.readinessProbe))
    return "";
  return stringify(p);
}

/** Reads probes from YAML (or JSON). */
export function probesFromYaml(text: string): Probes {
  if (!text.trim()) return {};
  const v = parse(text) as Record<string, unknown> | null;
  if (v === null || typeof v !== "object" || Array.isArray(v))
    throw new Error("Write the probes as a YAML map.");
  const out: Probes = {};
  for (const [k, val] of Object.entries(v)) {
    if (
      k !== "startupProbe" &&
      k !== "livenessProbe" &&
      k !== "readinessProbe"
    ) {
      throw new Error(
        `Unknown key ${k}: use startupProbe, livenessProbe and readinessProbe.`,
      );
    }
    if (val !== null && val !== undefined) out[k] = val as Probes[typeof k];
  }
  return out;
}
