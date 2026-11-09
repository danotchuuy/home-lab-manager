import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { api } from "../api";
import { useAdmin } from "../access";
import { AppIcon } from "../components/common";
import { DeployProgress } from "../components/DeployProgress";
import { useHosts } from "../components/HostPicker";
import type { AdoptPlan, DeployEvent, UnmanagedContainer } from "../types";
import { formatPorts } from "../util";

const SOURCES: Record<AdoptPlan["source"], (p: AdoptPlan) => string> = {
  "community-apps": (p) =>
    `Unraid made it from the Community Applications template ${p.template}. hlm uses that template, with the values from Unraid's copy of it, so it stays up to date with the template.`,
  unraid: () =>
    "Unraid made it, from a template the catalog doesn't have. hlm keeps Unraid's copy of the template, with its values, as a custom template.",
  container: () =>
    "hlm makes a custom template from the container's own settings: its ports, folders, volumes, variables, labels and devices, with the rest as extra docker run parameters.",
};

/** Lists the containers hlm doesn't manage on a host, and adopts them as servers. */
export function AdoptPage() {
  const admin = useAdmin();
  const hosts = useHosts();
  const [host, setHost] = useState("local");
  const [list, setList] = useState<UnmanagedContainer[]>();
  const [error, setError] = useState<string>();
  const [open, setOpen] = useState<string>();

  const load = useCallback(() => {
    setError(undefined);
    setList(undefined);
    api.unmanagedContainers(host).then(setList, (e: Error) => setError(e.message));
  }, [host]);
  useEffect(load, [load]);

  if (!admin) return <div className="page">Only admins can adopt containers.</div>;
  return (
    <div className="page adopt-page">
      <div className="page-header">
        <h1>Adopt a container</h1>
        <Link className="btn small ghost" to={`/stacks/adopt?host=${encodeURIComponent(host)}`}>
          Adopt a Compose stack
        </Link>
        {hosts && hosts.length > 1 && (
          <select value={host} onChange={(e) => setHost(e.target.value)} aria-label="Docker host">
            {hosts.map((h) => (
              <option key={h.id} value={h.id} disabled={h.status ? !h.status.ok : false}>
                {h.name}
                {h.local ? "" : ` (${h.id})`}
              </option>
            ))}
          </select>
        )}
      </div>
      <p className="muted">
        Containers on this Docker host that hlm didn&apos;t make. Adopting one makes it one of hlm&apos;s servers, with
        backups, updates, schedules and its settings form. Docker can&apos;t add hlm&apos;s labels to a container, so
        hlm recreates it with the same settings, as an update does; it keeps its name, data and volumes, and the old
        container stays until the new one runs. hlm shows what would change first.
      </p>
      {error && <div className="alert error">{error}</div>}
      {!list && !error && <p className="muted">Loading…</p>}
      {list && list.length === 0 && (
        <p className="muted">
          Every container here is hlm&apos;s. <Link to="/">Back to the servers</Link>
        </p>
      )}
      {list?.map((c) => (
        <ContainerCard
          key={c.id}
          c={c}
          open={open === c.id}
          onOpen={() => setOpen(open === c.id ? undefined : c.id)}
          onClosed={load}
        />
      ))}
    </div>
  );
}

function ContainerCard({
  c,
  open,
  onOpen,
  onClosed,
}: {
  c: UnmanagedContainer;
  open: boolean;
  onOpen: () => void;
  onClosed: () => void;
}) {
  return (
    <section className="card adopt-card">
      <div className="adopt-head">
        <AppIcon src={c.icon} size={40} glyph="📦" />
        <div className="adopt-title">
          <strong>{c.name}</strong> <span className={`badge state-${c.state}`}>{c.state}</span>
          {c.unraid && <span className="badge plain">Unraid</span>}
          {c.compose && <span className="badge plain">Compose: {c.compose}</span>}
          <div className="muted small">
            <code>{c.image}</code>
            {c.ports.length > 0 && <> · ports {formatPorts(c.ports)}</>}
            {c.network && c.network !== "default" && c.network !== "bridge" && <> · network {c.network}</>}
          </div>
          {c.mounts && c.mounts.length > 0 && (
            <div className="muted small adopt-mounts">
              {c.mounts.map((m) => (
                <code key={m}>{m}</code>
              ))}
            </div>
          )}
        </div>
        {c.adoptable ? (
          <button className="btn small" onClick={onOpen}>
            {open ? "Close" : "Adopt…"}
          </button>
        ) : c.compose ? (
          <Link
            className="btn small"
            to={`/stacks/adopt?host=${encodeURIComponent(c.host)}${c.composeFile ? `&path=${encodeURIComponent(c.composeFile)}` : ""}`}
          >
            Adopt the stack…
          </Link>
        ) : null}
      </div>
      {!c.adoptable && c.reason && <p className="muted small no-margin">{c.reason}</p>}
      {open && <AdoptPanel id={c.id} onCancel={onOpen} onFailed={onClosed} />}
    </section>
  );
}

function AdoptPanel({ id, onCancel, onFailed }: { id: string; onCancel: () => void; onFailed: () => void }) {
  const navigate = useNavigate();
  const [plan, setPlan] = useState<AdoptPlan>();
  const [error, setError] = useState<string>();
  const [events, setEvents] = useState<DeployEvent[]>([]);
  const [running, setRunning] = useState(false);
  const [accepted, setAccepted] = useState(false);

  useEffect(() => {
    api.adoptPlan(id).then(setPlan, (e: Error) => setError(e.message));
  }, [id]);

  async function adopt() {
    setRunning(true);
    setEvents([]);
    let done = false;
    try {
      await api.adoptContainer(id, (e) => {
        setEvents((prev) => [...prev, e]);
        if (e.stage === "done") done = true;
      });
    } catch (e) {
      setEvents((prev) => [...prev, { stage: "error", message: (e as Error).message }]);
    } finally {
      setRunning(false);
    }
    if (done) setTimeout(() => navigate(`/servers/${encodeURIComponent(id)}`), 1200);
    else onFailed();
  }

  if (error) return <div className="alert error small">{error}</div>;
  if (!plan) return <p className="muted small">Working out how hlm would adopt it…</p>;
  const changes = plan.changes.length > 0;
  return (
    <div className="adopt-plan">
      <p className="small">{SOURCES[plan.source](plan)}</p>
      {changes ? (
        <div className="alert warn small">
          <strong>The new container would differ from this one:</strong>
          <ul>
            {plan.changes.map((c) => (
              <li key={c}>{c}</li>
            ))}
          </ul>
          You can change its settings after adopting it, or leave it as it is.
        </div>
      ) : (
        <div className="alert success small">
          The new container would have the same settings as this one.
        </div>
      )}
      {plan.notes && plan.notes.length > 0 && (
        <ul className="muted small">
          {plan.notes.map((n) => (
            <li key={n}>{n}</li>
          ))}
        </ul>
      )}
      {plan.container.unraid && (
        <p className="muted small">
          Afterwards, change it in hlm, not on Unraid&apos;s Docker page: Unraid would recreate it without hlm&apos;s
          labels, and it would stop being a server here.
        </p>
      )}
      {events.length > 0 && <DeployProgress events={events} />}
      {changes && (
        <label className="radio small">
          <input type="checkbox" checked={accepted} onChange={(e) => setAccepted(e.target.checked)} disabled={running} />
          Adopt it with these changes
        </label>
      )}
      <div className="btn-row">
        <button className="btn primary" onClick={adopt} disabled={running || (changes && !accepted)}>
          {running ? "Adopting…" : plan.restart ? "Adopt and restart it" : "Adopt it"}
        </button>
        <button className="btn ghost" onClick={onCancel} disabled={running}>
          Cancel
        </button>
      </div>
      {plan.restart && !running && (
        <p className="muted small no-margin">It&apos;s running: it stops, and the new container starts in its place.</p>
      )}
    </div>
  );
}
