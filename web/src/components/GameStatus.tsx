import { useState } from "react";
import { api } from "../api";
import type { GameStatus, GameStatusKind, GameSummary, QueryProtocol, QueryResult, QuerySettings } from "../types";
import { usePoll } from "../usePoll";
import { Ago } from "./common";

const STATUS_LABEL: Record<GameStatusKind, string> = {
  online: "Online",
  starting: "Starting",
  not_responding: "Not responding",
  unknown: "Status unknown",
  off: "Not checked",
  stopped: "Stopped",
};

const STATUS_HELP: Record<GameStatusKind, string> = {
  online: "The server answers its game's status query.",
  starting: "The container is running, but the game isn't answering yet (it may still be downloading or loading).",
  not_responding: "The server answered before but has stopped, or hasn't answered for a long time.",
  unknown: "hlm hasn't found a way to ask this server how it's doing; choose one in the query settings.",
  off: "Status queries are turned off for this server.",
  stopped: "",
};

const PROTOCOL_LABEL: Record<QueryProtocol, string> = {
  "": "Find a way automatically",
  a2s: "Steam server query (A2S, UDP)",
  minecraft: "Minecraft Java server list ping (TCP)",
  bedrock: "Minecraft Bedrock ping (UDP)",
  "http-status": "HTTP status page (Nightingale's /status, TCP)",
  rcon: "Player list through the console (RCON, Palworld's REST API or exec)",
  tcp: "Only check a TCP port is open",
  off: "Don't check",
};

/** A small badge for a server's game status, with players when known. */
export function GameStatusBadge({ s }: { s?: GameSummary }) {
  if (!s || s.status === "stopped" || s.status === "off") return null;
  const players = s.status === "online" && s.hasPlayers ? ` · ${s.players}${s.maxPlayers ? `/${s.maxPlayers}` : ""}` : "";
  return (
    <span className={`badge game-${s.status}`} title={STATUS_HELP[s.status]}>
      <span className="game-dot" aria-hidden="true" />
      {STATUS_LABEL[s.status]}
      {players && (
        <>
          {players}
          <span className="visually-hidden"> players</span>
        </>
      )}
    </span>
  );
}

/**
 * The most players in each 5-minute step of the last day, as a sparkline:
 * a 2px line in the muted ink, the latest point in the accent, and a
 * crosshair readout on hover or keyboard focus.
 */
function PlayerSparkline({ points, max }: { points: { t: string; players: number }[]; max?: number }) {
  const [hover, setHover] = useState<number>();
  const W = 240;
  const H = 40;
  const pad = 3;
  if (points.length < 2) return null;
  const t0 = new Date(points[0].t).getTime();
  const t1 = new Date(points[points.length - 1].t).getTime();
  const top = Math.max(1, max ?? 0, ...points.map((p) => p.players));
  const x = (t: string) => pad + ((new Date(t).getTime() - t0) / Math.max(1, t1 - t0)) * (W - 2 * pad);
  const y = (n: number) => H - pad - (n / top) * (H - 2 * pad);
  const line = points.map((p, i) => `${i ? "L" : "M"}${x(p.t).toFixed(1)},${y(p.players).toFixed(1)}`).join(" ");
  const area = `${line} L${x(points[points.length - 1].t).toFixed(1)},${H - pad} L${x(points[0].t).toFixed(1)},${H - pad} Z`;
  const last = points[points.length - 1];
  const peak = Math.max(...points.map((p) => p.players));
  const shown = hover !== undefined ? points[hover] : undefined;

  const nearest = (clientX: number, el: SVGSVGElement) => {
    const r = el.getBoundingClientRect();
    const px = ((clientX - r.left) / r.width) * W;
    let best = 0;
    points.forEach((p, i) => {
      if (Math.abs(x(p.t) - px) < Math.abs(x(points[best].t) - px)) best = i;
    });
    return best;
  };
  const fmt = (t: string) => new Date(t).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });

  return (
    <div className="sparkline">
      <svg
        viewBox={`0 0 ${W} ${H}`}
        width={W}
        height={H}
        role="img"
        tabIndex={0}
        aria-label={`Players over the last ${Math.round((t1 - t0) / 3600000) || 1} hours: peak ${peak}, now ${last.players}`}
        onPointerMove={(e) => setHover(nearest(e.clientX, e.currentTarget))}
        onPointerLeave={() => setHover(undefined)}
        onFocus={() => setHover(points.length - 1)}
        onBlur={() => setHover(undefined)}
        onKeyDown={(e) => {
          if (e.key === "ArrowLeft") setHover((h) => Math.max(0, (h ?? points.length - 1) - 1));
          if (e.key === "ArrowRight") setHover((h) => Math.min(points.length - 1, (h ?? 0) + 1));
        }}
      >
        <line className="spark-base" x1={pad} x2={W - pad} y1={H - pad} y2={H - pad} />
        <path className="spark-area" d={area} />
        <path className="spark-line" d={line} />
        {shown && <line className="spark-crosshair" x1={x(shown.t)} x2={x(shown.t)} y1={pad} y2={H - pad} />}
        <circle className="spark-now" cx={x(last.t)} cy={y(last.players)} r={3.5} />
        {shown && <circle className="spark-hover" cx={x(shown.t)} cy={y(shown.players)} r={3.5} />}
      </svg>
      <span className="sparkline-readout small">
        {shown ? (
          <>
            <strong>{shown.players}</strong> <span className="muted">at {fmt(shown.t)}</span>
          </>
        ) : (
          <span className="muted">
            Peak <strong>{peak}</strong> since {fmt(points[0].t)}
          </span>
        )}
      </span>
    </div>
  );
}

function describeResult(r: QueryResult): string {
  return [r.name, r.map && r.map !== r.name ? r.map : "", r.version ? `v${r.version.replace(/^v/, "")}` : ""]
    .filter(Boolean)
    .join(" · ");
}

/** A server's game status card: online or not, players, and (for managers) how it's queried. */
export function GameStatusCard({ server, running, canManage }: { server: string; running: boolean; canManage: boolean }) {
  const [st, setSt] = useState<GameStatus>();
  const [editing, setEditing] = useState(false);
  usePoll(
    async () => {
      setSt(await api.gameStatus(server));
    },
    15000,
    [server, running],
  );
  if (!st || !running) return null;
  const r = st.result;

  return (
    <div className="card game-status">
      <div className="game-status-head">
        <div className="game-status-main">
          <GameStatusBadge s={{ status: st.status, players: r?.players ?? 0, maxPlayers: r?.maxPlayers, hasPlayers: false }} />
          {st.status === "online" && r?.hasPlayers && (
            <span className="players-now">
              <strong>{r.players}</strong>
              {r.maxPlayers ? <span className="muted"> / {r.maxPlayers}</span> : null} players
            </span>
          )}
          {st.status === "online" && r && <span className="muted small">{describeResult(r)}</span>}
          {r?.password && <span className="muted small">· password protected</span>}
        </div>
        {st.history && st.history.length > 1 && <PlayerSparkline points={st.history} max={r?.maxPlayers} />}
      </div>
      {st.status === "online" && r?.playerNames && r.playerNames.length > 0 && (
        <div className="player-names small">
          {r.playerNames.map((n) => (
            <span key={n} className="chip static">
              {n}
            </span>
          ))}
        </div>
      )}
      <div className="muted small">
        {st.status !== "online" && STATUS_HELP[st.status]}
        {st.method && ` Checked with: ${PROTOCOL_LABEL[st.method.protocol]}${st.method.port ? `, port ${st.method.port}` : ""}`}
        {st.checked && (
          <>
            {st.method ? ", " : " Checked "}
            <Ago iso={st.checked} />.
          </>
        )}
        {canManage && (
          <>
            {" "}
            <button className="link-button" onClick={() => setEditing(!editing)}>
              Query settings
            </button>
          </>
        )}
      </div>
      {st.error && st.status !== "online" && <div className="small form-error">{st.error}</div>}
      {editing && canManage && (
        <QuerySettingsForm
          server={server}
          initial={st.settings}
          onSaved={(n) => {
            setSt(n);
            setEditing(false);
          }}
        />
      )}
    </div>
  );
}

function QuerySettingsForm({
  server,
  initial,
  onSaved,
}: {
  server: string;
  initial: QuerySettings;
  onSaved: (st: GameStatus) => void;
}) {
  const [set, setSet] = useState<QuerySettings>(initial);
  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const needsPort = !["", "rcon", "off"].includes(set.protocol);

  async function run(label: string, fn: () => Promise<void>) {
    setBusy(label);
    setError(undefined);
    setNotice(undefined);
    try {
      await fn();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(undefined);
    }
  }

  const value = (): QuerySettings => (needsPort ? set : { protocol: set.protocol });

  return (
    <div className="config-form query-settings">
      <div className="field-grid">
        <label className="field">
          <span className="field-label">How to ask</span>
          <select value={set.protocol} onChange={(e) => setSet({ ...set, protocol: e.target.value as QueryProtocol })}>
            {(Object.keys(PROTOCOL_LABEL) as QueryProtocol[]).map((p) => (
              <option key={p} value={p}>
                {PROTOCOL_LABEL[p]}
              </option>
            ))}
          </select>
          {set.protocol === "rcon" && (
            <span className="field-help">
              Uses the console&apos;s setup (RCON, Palworld&apos;s REST API, or a command run in the container); works for
              Palworld, Minecraft and ARK. Through Palworld&apos;s REST API it also has the player limit, server name and
              version.
            </span>
          )}
        </label>
        {needsPort && (
          <label className="field">
            <span className="field-label">Port (in the container)</span>
            <input
              type="number"
              min={1}
              max={65535}
              value={set.port ?? ""}
              onChange={(e) => setSet({ ...set, port: Number(e.target.value) || undefined })}
              required
            />
            <span className="field-help">For Steam games, the query port (often the game port + 1, or 27015).</span>
          </label>
        )}
      </div>
      {error && <div className="alert error small">{error}</div>}
      {notice && <div className="alert success small">{notice}</div>}
      <div className="btn-row">
        <button
          className="btn small primary"
          disabled={!!busy}
          onClick={() => run("save", async () => onSaved(await api.setQuerySettings(server, value())))}
        >
          {busy === "save" ? "Saving…" : "Save"}
        </button>
        {set.protocol !== "off" && (
          <button
            className="btn small"
            disabled={!!busy}
            onClick={() =>
              run("test", async () => {
                const t = await api.testQuery(server, value());
                const how = `${PROTOCOL_LABEL[t.settings.protocol]}${t.settings.port ? ` on port ${t.settings.port}` : ""}`;
                const players = t.result.hasPlayers ? `, ${t.result.players} players online` : "";
                setNotice(`It answers: ${how}${players}.`);
              })
            }
          >
            {busy === "test" ? "Testing…" : set.protocol === "" ? "Find a way" : "Test"}
          </button>
        )}
      </div>
    </div>
  );
}

