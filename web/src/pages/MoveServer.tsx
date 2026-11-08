import { useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api } from "../api";
import { Spinner } from "../components/common";
import { ConfigForm } from "../components/ConfigForm";
import { DeployProgress } from "../components/DeployProgress";
import { useHosts } from "../components/HostPicker";
import { NetworkSection } from "../components/NetworkSection";
import type { DeployEvent, DeployRequest, MigratePreview, NetworkInfo } from "../types";
import { bareName, hostOf } from "../util";

/**
 * Moves a server to another Docker host: its data is copied there, it's
 * started there if it was running, its settings, port forwards and web
 * addresses follow it, and the original is removed (its data folders stay).
 * If its network or fixed address doesn't carry over, a dialog asks for
 * them on the new host first.
 */
export function MoveServerPage() {
  const { name = "" } = useParams();
  const navigate = useNavigate();
  const hosts = useHosts();
  const from = hostOf(name);
  const targets = hosts?.filter((h) => h.id !== from) ?? [];
  const [host, setHost] = useState("");
  const [preview, setPreview] = useState<MigratePreview>();
  const [request, setRequest] = useState<DeployRequest>();
  const [previewing, setPreviewing] = useState(false);
  const [error, setError] = useState<string>();
  // The network dialog, and whether its choice was made.
  const [dialog, setDialog] = useState(false);
  const [chosen, setChosen] = useState(false);
  const [events, setEvents] = useState<DeployEvent[]>([]);
  const [running, setRunning] = useState(false);

  // The first reachable other host.
  useEffect(() => {
    if (!host && targets.length > 0) setHost((targets.find((h) => !h.status || h.status.ok) ?? targets[0]).id);
  }, [host, targets]);

  async function load() {
    setPreviewing(true);
    setError(undefined);
    setPreview(undefined);
    setRequest(undefined);
    setChosen(false);
    try {
      const p = await api.previewMigrate(name, host);
      setPreview(p);
      // The form takes the name without its host.
      setRequest({ ...p.request, name: bareName(p.request.name) });
      if (p.network) setDialog(true);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setPreviewing(false);
    }
  }

  async function move(req: DeployRequest) {
    setRunning(true);
    setEvents([]);
    let moved: string | undefined;
    try {
      await api.migrateServer(name, { host, request: req }, (e) => {
        setEvents((prev) => [...prev, e]);
        if (e.stage === "done" && e.server) moved = e.server.name;
      });
    } catch (e) {
      setEvents((prev) => [...prev, { stage: "error", message: (e as Error).message }]);
    } finally {
      setRunning(false);
    }
    if (moved) setTimeout(() => navigate(`/servers/${encodeURIComponent(moved!)}`), 1500);
  }

  const hostName = (id: string) => hosts?.find((h) => h.id === id)?.name ?? id;
  const needsChoice = !!preview?.network && !chosen;

  return (
    <div className="page">
      <Link to={`/servers/${encodeURIComponent(name)}`} className="back">
        ← {name}
      </Link>
      <h1>Move {bareName(name)} to another host</h1>
      <p className="muted">
        hlm makes {bareName(name)} on the other host with the same settings, stops it here while its data folders are
        copied across, and starts it there if it was running. Its server groups, schedules, backup plan and history,
        players and other settings go with it, and its port forwards, web addresses and proxy relay are pointed at the
        new host. Then it&apos;s removed here; its data folders stay on this host, listed on the Host page, to delete
        once you&apos;re happy with the move.
      </p>

      <div className="card">
        <h2>Where to</h2>
        {!hosts ? (
          <Spinner />
        ) : targets.length === 0 ? (
          <p className="muted">There&apos;s no other Docker host to move it to. Add one on the Host page.</p>
        ) : (
          <div className="config-form">
            <label className="field">
              <span className="field-label">Docker host</span>
              <select
                value={host}
                onChange={(e) => {
                  setHost(e.target.value);
                  setPreview(undefined);
                }}
                disabled={running}
              >
                {targets.map((h) => (
                  <option key={h.id} value={h.id} disabled={h.status ? !h.status.ok : false}>
                    {h.name}
                    {h.local ? "" : ` (${h.id})`}
                    {h.status && !h.status.ok ? " — can't be reached" : ""}
                  </option>
                ))}
              </select>
              <span className="field-help">From {hostName(from)}.</span>
            </label>
            {!preview && (
              <div className="btn-row">
                <button className="btn primary" onClick={load} disabled={previewing || !host}>
                  {previewing ? "Working it out…" : "Next: check the move"}
                </button>
              </div>
            )}
          </div>
        )}
        {error && <div className="alert error">{error}</div>}
      </div>

      {preview && request && (
        <div className="card">
          <h2>
            {preview.name} on {hostName(host)}
          </h2>
          {preview.warnings.length > 0 && (
            <div className="alert warn small">
              <ul className="no-margin">
                {preview.warnings.map((w) => (
                  <li key={w}>{w}</li>
                ))}
              </ul>
            </div>
          )}
          {preview.network && (
            <div className={`alert ${chosen ? "info" : "warn"} small`}>
              {preview.network.reason}{" "}
              {chosen ? (
                <>
                  It&apos;ll be on <strong>{request.network || "its template's network"}</strong>
                  {request.ipAddress ? (
                    <>
                      {" "}
                      at <strong>{request.ipAddress}</strong>
                    </>
                  ) : (
                    " with an automatic address"
                  )}
                  .{" "}
                </>
              ) : null}
              <button type="button" className="link-button" onClick={() => setDialog(true)} disabled={running}>
                {chosen ? "Change" : "Choose its network and address"}
              </button>
            </div>
          )}
          <div className="small">
            <strong>{preview.running ? "Stopped here, then copied" : "Copied"}:</strong>
            {preview.copies && preview.copies.length > 0 ? (
              <ul className="copies">
                {preview.copies.map((c) => (
                  <li key={c.target}>
                    <code>{c.target}</code>: <code>{c.from}</code> → <code>{c.to}</code>
                  </li>
                ))}
              </ul>
            ) : (
              <span className="muted"> no data folders.</span>
            )}
            <strong>Follows it:</strong>
            <ul className="copies">
              {preview.follows.map((f) => (
                <li key={f}>{f}</li>
              ))}
            </ul>
            <p className="muted">
              {preview.running
                ? `It's started on ${hostName(host)} once its data is there.`
                : `It's stopped now, so it stays stopped on ${hostName(host)}.`}
            </p>
          </div>
          {events.length > 0 && <DeployProgress events={events} />}
          <ConfigForm
            key={`${host}|${request.network ?? ""}|${request.ipAddress ?? ""}`}
            app={preview.app}
            initial={request}
            isNew
            keepName
            host={host}
            submitLabel={running ? "Moving…" : `Move to ${hostName(host)}`}
            busy={running || needsChoice}
            onSubmit={move}
          />
          {needsChoice && <p className="muted small">Choose its network and address on {hostName(host)} first.</p>}
        </div>
      )}

      {dialog && preview?.network && request && (
        <NetworkDialog
          host={host}
          hostName={hostName(host)}
          choice={preview.network}
          request={request}
          onCancel={() => setDialog(false)}
          onChoose={(network, ipAddress) => {
            setRequest({ ...request, network, ipAddress });
            setChosen(true);
            setDialog(false);
          }}
        />
      )}
    </div>
  );
}

/**
 * Asks for a moved server's network and address on its new host, when its
 * own don't carry over (another subnet, the network missing, the address
 * taken there). It starts on the network of the same name, or else a LAN
 * network there, and fills in the next free address on a LAN network.
 */
function NetworkDialog({
  host,
  hostName,
  choice,
  request,
  onChoose,
  onCancel,
}: {
  host: string;
  hostName: string;
  choice: NonNullable<MigratePreview["network"]>;
  request: DeployRequest;
  onChoose: (network: string, ipAddress: string) => void;
  onCancel: () => void;
}) {
  const [networks, setNetworks] = useState<NetworkInfo[]>();
  const [network, setNetwork] = useState("");
  const [ipAddress, setIPAddress] = useState("");

  useEffect(() => {
    api.networks(host).then(
      (list) => {
        setNetworks(list);
        const same = list.find((n) => n.name === choice.network);
        const lan = list.find((n) => n.directIp);
        setNetwork(same?.name ?? lan?.name ?? "bridge");
      },
      () => setNetworks([]),
    );
  }, [host, choice.network]);

  return (
    <div className="modal-backdrop" role="dialog" aria-modal="true" aria-labelledby="move-network-title">
      <div className="card modal wide">
        <h2 id="move-network-title">Its network on {hostName}</h2>
        <p className="small no-margin">{choice.reason}</p>
        <p className="muted small no-margin">
          Here it&apos;s on <strong>{choice.network}</strong>
          {choice.subnets.length > 0 && <> ({choice.subnets.join(", ")})</>}
          {choice.ipAddress && (
            <>
              {" "}
              at <strong>{choice.ipAddress}</strong>
            </>
          )}
          . Players, port forwards and web addresses use its new address once it&apos;s moved.
        </p>
        {!networks ? (
          <Spinner />
        ) : (
          <NetworkSection
            host={host}
            templateNetwork={request.network || choice.network}
            network={network}
            ipAddress={ipAddress}
            onNetwork={setNetwork}
            onIPAddress={setIPAddress}
          />
        )}
        <div className="btn-row">
          <button className="btn primary" onClick={() => onChoose(network, ipAddress.trim())} disabled={!networks}>
            Use this
          </button>
          <button className="btn ghost" onClick={onCancel}>
            Cancel
          </button>
        </div>
      </div>
    </div>
  );
}
