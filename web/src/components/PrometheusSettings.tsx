import { useEffect, useState } from "react";
import { api } from "../api";
import type { MetricsSettings } from "../types";
import { Spinner } from "./common";

/** System → Prometheus: how to scrape /metrics, and who may without a key. */
export function PrometheusSettings() {
  const [saved, setSaved] = useState<MetricsSettings>();
  const [allowFrom, setAllowFrom] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();

  useEffect(() => {
    api.metricsSettings().then(
      (s) => {
        setSaved(s);
        setAllowFrom(s.allowFrom.join(", "));
      },
      (e) => setError((e as Error).message),
    );
  }, []);

  const list = allowFrom
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean);
  const changed = saved !== undefined && list.join(",") !== saved.allowFrom.join(",");

  async function save() {
    setBusy(true);
    setError(undefined);
    setNotice(undefined);
    try {
      const s = await api.setMetricsSettings({ allowFrom: list });
      setSaved(s);
      setAllowFrom(s.allowFrom.join(", "));
      setNotice("Saved.");
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const url = `${window.location.origin}/metrics`;
  return (
    <div className="prometheus">
      <p className="muted small no-margin">
        hlm serves its servers&apos; state, resource use, players, backups and the host&apos;s figures at{" "}
        <code>{url}</code> in Prometheus&apos; format, for Grafana and alerting. Make an{" "}
        <a href="#api-keys">API key</a> with the <strong>Metrics</strong> role for Prometheus: it can read{" "}
        <code>/metrics</code> and nothing else.
      </p>
      <pre className="op-output">{`scrape_configs:
  - job_name: hlm
    metrics_path: /metrics
    scheme: ${window.location.protocol.replace(":", "")}
    authorization:
      credentials: hlm_…   # the metrics key
    static_configs:
      - targets: ["${window.location.host}"]`}</pre>
      {!saved && !error ? (
        <Spinner />
      ) : (
        <>
          <label className="field">
            <span className="field-label">Allow without a key from (optional)</span>
            <input value={allowFrom} onChange={(e) => setAllowFrom(e.target.value)} placeholder="e.g. 192.168.11.20, 10.0.0.0/24" />
            <span className="field-help">
              Addresses or ranges that may read <code>/metrics</code> without a key, such as your Prometheus server.
              Leave empty to always need one. Server names and images are in the metrics, so keep the list short.
            </span>
          </label>
          {error && <div className="alert error small">{error}</div>}
          {notice && <div className="alert success small">{notice}</div>}
          <div className="btn-row">
            <button className="btn small primary" onClick={save} disabled={busy || !changed}>
              {busy ? "Saving…" : "Save"}
            </button>
          </div>
        </>
      )}
    </div>
  );
}
