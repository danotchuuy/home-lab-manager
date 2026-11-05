import { useEffect, useState, type ChangeEvent } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api } from "../api";
import { Spinner } from "../components/common";
import { DeployProgress } from "../components/DeployProgress";
import type { DeployEvent, StackPlan } from "../types";

const EXAMPLE = `services:
  app:
    image: ghcr.io/example/app:latest
    ports:
      - "8080:80"
    volumes:
      - ./data:/data
    environment:
      DB_HOST: db
      DB_PASSWORD: \${DB_PASSWORD}
    depends_on:
      db:
        condition: service_healthy
    restart: unless-stopped
  db:
    image: postgres:16
    environment:
      POSTGRES_PASSWORD: \${DB_PASSWORD}
    volumes:
      - db:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres"]
      interval: 10s
    restart: unless-stopped
volumes:
  db:
`;

const CHANGE: Record<string, string> = {
  create: "created",
  update: "recreated",
  unchanged: "unchanged",
  remove: "removed (its data is kept)",
};

/** Catalog → Add a Compose stack, and a stack's Edit. */
export function StackEditorPage() {
  const { name: existing } = useParams();
  const navigate = useNavigate();
  const [name, setName] = useState(existing ?? "");
  const [yaml, setYaml] = useState(existing ? "" : EXAMPLE);
  const [env, setEnv] = useState("");
  const [loaded, setLoaded] = useState(!existing);
  const [plan, setPlan] = useState<StackPlan>();
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState<string>();
  const [events, setEvents] = useState<DeployEvent[]>([]);

  useEffect(() => {
    if (!existing) return;
    api.stack(existing).then(
      (r) => {
        setYaml(r.yaml ?? "");
        setEnv(r.env ?? "");
        setLoaded(true);
      },
      (e) => setError((e as Error).message),
    );
  }, [existing]);

  const edit = (f: (v: string) => void) => (v: string) => {
    f(v);
    setPlan(undefined);
  };

  async function upload(e: ChangeEvent<HTMLInputElement>, into: (v: string) => void) {
    const f = e.target.files?.[0];
    e.target.value = "";
    if (f) edit(into)(await f.text());
  }

  async function preview() {
    setBusy("preview");
    setError(undefined);
    try {
      setPlan(await api.previewStack(name.trim(), yaml, env, !!existing));
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(undefined);
    }
  }

  async function apply() {
    setBusy("apply");
    setEvents([]);
    let ok = false;
    try {
      const op = existing ? api.updateStack : api.createStack;
      await op(name.trim(), yaml, env, (e) => {
        setEvents((prev) => [...prev, e]);
        if (e.stage === "done") ok = true;
      });
    } catch (e) {
      setEvents((prev) => [...prev, { stage: "error", message: (e as Error).message }]);
    } finally {
      setBusy(undefined);
    }
    if (ok) setTimeout(() => navigate("/"), 1500);
  }

  async function remove() {
    if (
      !confirm(
        `Delete the stack ${existing}? Its containers and networks are removed; their data folders and volumes are kept.`,
      )
    )
      return;
    setBusy("delete");
    try {
      await api.deleteStack(existing!);
      navigate("/");
    } catch (e) {
      setError((e as Error).message);
      setBusy(undefined);
    }
  }

  if (!loaded) return <div className="page">{error ? <div className="alert error">{error}</div> : <Spinner />}</div>;
  const changes = plan?.services.filter((s) => s.change !== "unchanged").length ?? 0;

  return (
    <div className="page stack-editor">
      <Link to={existing ? "/" : "/catalog"} className="back">
        ← {existing ? "Servers" : "Catalog"}
      </Link>
      <h1>{existing ? `Stack ${existing}` : "Add a Compose stack"}</h1>
      <p className="muted">
        Several containers that belong together, from a <code>docker-compose.yml</code>. Each service runs as one of
        hlm&apos;s servers (with its console, files, backups and ports), named{" "}
        <code>&lt;stack&gt;-&lt;service&gt;</code>, on the stack&apos;s own network, where services reach each other by
        name. Relative paths (<code>./data</code>) go under <code>&lt;appdata&gt;/&lt;stack&gt;/</code>.{" "}
        <code>build:</code>, <code>extends</code>, <code>profiles</code> and <code>configs</code>/<code>secrets</code>{" "}
        aren&apos;t supported.
      </p>
      <div className="card config-form">
        {!existing && (
          <label className="field">
            <span className="field-label">Stack name</span>
            <input
              value={name}
              onChange={(e) => edit(setName)(e.target.value)}
              placeholder="nextcloud"
              spellCheck={false}
            />
            <span className="field-help">Lower-case letters, digits, dashes and underscores.</span>
          </label>
        )}
        <label className="field">
          <span className="field-label">docker-compose.yml</span>
          <textarea
            className="code-input"
            rows={22}
            value={yaml}
            onChange={(e) => edit(setYaml)(e.target.value)}
            spellCheck={false}
          />
        </label>
        <label className="btn small ghost upload">
          Load a file…
          <input type="file" accept=".yml,.yaml,text/yaml" hidden onChange={(e) => upload(e, setYaml)} />
        </label>
        <label className="field">
          <span className="field-label">.env (optional)</span>
          <textarea
            className="code-input"
            rows={4}
            value={env}
            onChange={(e) => edit(setEnv)(e.target.value)}
            placeholder={"DB_PASSWORD=change-me"}
            spellCheck={false}
          />
          <span className="field-help">
            Variables for <code>${"{VAR}"}</code> in the file (and for <code>env_file: .env</code>). hlm&apos;s own
            environment isn&apos;t used. Kept encrypted with hlm&apos;s other settings.
          </span>
        </label>
        {error && <div className="alert error">{error}</div>}
        <div className="btn-row">
          <button className="btn" onClick={preview} disabled={!!busy || !name.trim() || !yaml.trim()}>
            {busy === "preview" ? "Checking…" : "Check"}
          </button>
          {plan && (
            <button className="btn primary" onClick={apply} disabled={!!busy || (!!existing && changes === 0)}>
              {busy === "apply"
                ? "Working…"
                : existing
                  ? `Apply ${changes} change${changes === 1 ? "" : "s"}`
                  : "Create the stack"}
            </button>
          )}
          {existing && (
            <button className="btn danger" onClick={remove} disabled={!!busy}>
              Delete stack
            </button>
          )}
        </div>
      </div>

      {plan && (
        <div className="card">
          <h2>What happens</h2>
          {plan.warnings.length > 0 && (
            <ul className="warnings transfer-warnings">
              {plan.warnings.map((w, i) => (
                <li key={i}>⚠ {w}</li>
              ))}
            </ul>
          )}
          <table className="data-table">
            <thead>
              <tr>
                <th>Service</th>
                <th>Image</th>
                <th>Ports</th>
                <th>Data</th>
                <th>Change</th>
              </tr>
            </thead>
            <tbody>
              {plan.services.map((s) => (
                <tr key={s.service}>
                  <td>
                    <strong>{s.service}</strong>
                    <div className="muted small">{s.container}</div>
                    {s.dependsOn?.length ? <div className="muted small">after {s.dependsOn.join(", ")}</div> : null}
                  </td>
                  <td>
                    <code className="small">{s.image}</code>
                  </td>
                  <td className="small">{s.ports.join(", ") || "—"}</td>
                  <td className="small">
                    {s.mounts.map((m) => (
                      <div key={m}>
                        <code>{m}</code>
                      </div>
                    ))}
                  </td>
                  <td className={`small change-${s.change}`}>{CHANGE[s.change]}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {plan.networks.length > 0 && <p className="muted small">Networks: {plan.networks.join(", ")}</p>}
        </div>
      )}
      {events.length > 0 && (
        <div className="card">
          <DeployProgress events={events} />
        </div>
      )}
    </div>
  );
}
