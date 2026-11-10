import { useEffect, useState, type ChangeEvent } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api } from "../api";
import { Spinner } from "../components/common";
import { DeployProgress } from "../components/DeployProgress";
import { HostPicker } from "../components/HostPicker";
import { StackPlanCard } from "../components/StackPlanCard";
import type { DeployEvent, Stack, StackPlan } from "../types";

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
  // A new stack's Docker host; an existing one stays where it is.
  const [host, setHost] = useState("local");
  const [stackHost, setStackHost] = useState<string>();
  // The Compose file on the host it was adopted from, if it was.
  const [source, setSource] = useState<string>();
  // Services whose images follow latest.
  const [track, setTrack] = useState<Stack["track"]>();

  useEffect(() => {
    if (!existing) return;
    api.stack(existing).then(
      (r) => {
        setYaml(r.yaml ?? "");
        setEnv(r.env ?? "");
        setStackHost(r.stack.host);
        setSource(r.stack.source);
        setTrack(r.stack.track);
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
      setPlan(await api.previewStack(name.trim(), yaml, env, !!existing, host));
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
      const onEvent = (e: DeployEvent) => {
        setEvents((prev) => [...prev, e]);
        if (e.stage === "done") ok = true;
      };
      if (existing) await api.updateStack(name.trim(), yaml, env, onEvent);
      else await api.createStack(name.trim(), yaml, env, onEvent, host);
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
        {!existing && (
          <HostPicker
            value={host}
            onChange={edit(setHost)}
            help="The machine the stack runs on: its services, networks and data folders are there. It can't move later."
          />
        )}
        {existing && stackHost && stackHost !== "local" && (
          <p className="muted small no-margin">
            On Docker host <strong>{stackHost}</strong>.
          </p>
        )}
        {existing && source && (
          <p className="muted small no-margin">
            Adopted from <code>{source}</code>
            {stackHost && stackHost !== "local" ? ` on ${stackHost}` : ""}. Its relative paths (<code>./data</code>) are
            still relative to that folder. hlm runs it from its own copy of the file below: changes to the file on the host
            aren&apos;t picked up, and <code>docker compose</code> shouldn&apos;t be used on it any more.
          </p>
        )}
        {existing && track && Object.keys(track).length > 0 && (
          <p className="muted small no-margin">
            Following a tag:{" "}
            {Object.keys(track)
              .sort()
              .map((svc) => `${svc} (${track[svc].tag})`)
              .join(", ")}
            . Their images were tagged latest (or stable), so hlm set them to the versions they were. The update check says
            when the tag moves on to a new version, and updating the stack changes the file to it. Change one&apos;s image
            tag yourself and it stops following.
          </p>
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

      {plan && <StackPlanCard plan={plan} />}
      {events.length > 0 && (
        <div className="card">
          <DeployProgress events={events} />
        </div>
      )}
    </div>
  );
}
