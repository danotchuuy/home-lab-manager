import { useState } from "react";
import { api } from "../api";
import type { Probe, ProbeKind, Probes, ProbeStatus } from "../types";
import { Ago } from "./common";
import { probesFromYaml } from "../probesyaml";

export { probesFromYaml, probesToYaml } from "../probesyaml";

const KINDS: { kind: ProbeKind; key: keyof Probes; label: string }[] = [
  { kind: "startup", key: "startupProbe", label: "Startup" },
  { kind: "liveness", key: "livenessProbe", label: "Liveness" },
  { kind: "readiness", key: "readinessProbe", label: "Readiness" },
];

const EXAMPLE = `startupProbe:
  tcpSocket:
    port: 8080
  periodSeconds: 10
  failureThreshold: 30
livenessProbe:
  httpGet:
    path: /healthz
    port: 8080
  periodSeconds: 30
  timeoutSeconds: 5
readinessProbe:
  httpGet:
    path: /ready
    port: 8080
`;

function describe(p: Probe): string {
  if (p.httpGet)
    return `${p.httpGet.scheme ?? "HTTP"} GET ${p.httpGet.host ?? ""}:${p.httpGet.port}${p.httpGet.path ?? "/"}`;
  if (p.tcpSocket) return `TCP ${p.tcpSocket.host ?? ""}:${p.tcpSocket.port}`;
  if (p.exec) return p.exec.command.join(" ");
  return "?";
}

/** The probes' editor and state, on a server's health card. */
export function ProbesEditor({
  server,
  text,
  onChange,
  error,
  status,
  running,
  canManage,
}: {
  server: string;
  text: string;
  onChange: (text: string) => void;
  error?: string;
  status?: ProbeStatus;
  running: boolean;
  canManage: boolean;
}) {
  const [tests, setTests] =
    useState<
      Partial<Record<ProbeKind, { success: boolean; message: string }>>
    >();
  const [testError, setTestError] = useState<string>();
  const [testing, setTesting] = useState(false);

  let probes: Probes = {};
  try {
    probes = probesFromYaml(text);
  } catch {
    // shown by the card
  }
  const any = KINDS.some((k) => probes[k.key]);

  async function test() {
    setTesting(true);
    setTests(undefined);
    setTestError(undefined);
    const out: Partial<
      Record<ProbeKind, { success: boolean; message: string }>
    > = {};
    try {
      for (const k of KINDS) {
        const p = probes[k.key];
        if (p) out[k.kind] = await api.testServerProbe(server, p);
      }
      setTests(out);
    } catch (e) {
      setTestError(`${(e as Error).message}`);
    } finally {
      setTesting(false);
    }
  }

  return (
    <div className="field">
      <span className="field-label">Kubernetes-style probes</span>
      {status && (
        <>
          <p className="small no-margin">
            {status.restarting ? (
              <strong className="run-failed">Restarting: a probe failed</strong>
            ) : !status.started ? (
              <strong>Starting: waiting for the startup probe</strong>
            ) : status.ready ? (
              <strong className="run-ok">Ready</strong>
            ) : (
              <strong className="run-failed">Not ready</strong>
            )}
            {status.readySince && (
              <span className="muted">
                {" "}
                since <Ago iso={status.readySince} />
              </span>
            )}
          </p>
          <div className="table-scroll">
            <table className="data-table small">
              <thead>
                <tr>
                  <th>Probe</th>
                  <th>Last result</th>
                  <th>In a row</th>
                  <th>Checked</th>
                </tr>
              </thead>
              <tbody>
                {KINDS.filter((k) => status.probes[k.kind]).map((k) => {
                  const r = status.probes[k.kind]!;
                  return (
                    <tr key={k.kind}>
                      <td>{k.label}</td>
                      <td>
                        {r.waiting ? (
                          <span className="muted">waiting for startup</span>
                        ) : !r.result ? (
                          <span className="muted">not run yet</span>
                        ) : (
                          <span
                            className={
                              r.result === "success" ? "run-ok" : "run-failed"
                            }
                          >
                            {r.result}
                          </span>
                        )}
                        {r.message && <div className="muted">{r.message}</div>}
                      </td>
                      <td>
                        {r.result === "success"
                          ? `${r.successes} ok`
                          : r.result
                            ? `${r.failures} failed`
                            : ""}
                      </td>
                      <td>{r.checked ? <Ago iso={r.checked} /> : ""}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </>
      )}
      <textarea
        className="code-input"
        rows={Math.max(6, Math.min(24, text.split("\n").length + 1))}
        value={text}
        onChange={(e) => {
          onChange(e.target.value);
          setTests(undefined);
        }}
        placeholder={EXAMPLE}
        spellCheck={false}
        disabled={!canManage}
      />
      {error && <div className="alert error small">{error}</div>}
      <span className="field-help">
        <code>startupProbe</code>, <code>livenessProbe</code> and{" "}
        <code>readinessProbe</code>, as in a Kubernetes pod spec, each with one
        of <code>httpGet</code> (200 to 399 succeeds; redirects aren&apos;t
        followed), <code>tcpSocket</code> (connecting succeeds) or{" "}
        <code>exec</code> (a command in the container, not in a shell:{" "}
        <code>[sh, -c, &quot;…&quot;]</code>; exit code 0 succeeds), and{" "}
        <code>initialDelaySeconds</code>, <code>periodSeconds</code> (10),{" "}
        <code>timeoutSeconds</code> (1), <code>successThreshold</code> (1) and{" "}
        <code>failureThreshold</code> (3). Ports are the container&apos;s, by
        number; hlm reaches them as it reaches the server for its status query,
        or at <code>host</code> if one is given. hlm runs the probes itself, so
        they work straight away and need nothing in the image. Until the startup
        probe succeeds the others wait, and failing{" "}
        <code>failureThreshold</code> times restarts the server; a liveness
        probe failing that many times in a row restarts it too (not during a
        backup or update, and each restart counts towards the crash-loop limit);
        a readiness probe failing marks it not ready and notifies you (Health
        check failing).
      </span>
      {canManage && running && any && (
        <div className="btn-row">
          <button
            type="button"
            className="btn small"
            disabled={testing || !!error}
            onClick={test}
          >
            {testing ? "Testing…" : "Test the probes now"}
          </button>
        </div>
      )}
      {testError && <div className="alert error small">{testError}</div>}
      {tests && (
        <ul className="small no-margin">
          {KINDS.filter((k) => tests[k.kind]).map((k) => (
            <li key={k.kind}>
              {k.label} ({describe(probes[k.key]!)}):{" "}
              <span
                className={tests[k.kind]!.success ? "run-ok" : "run-failed"}
              >
                {tests[k.kind]!.success ? "succeeds" : "fails"}
              </span>{" "}
              <span className="muted">{tests[k.kind]!.message}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
