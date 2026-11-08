import { Fragment, useEffect, useState, type FormEvent } from "react";
import { api, ApiError } from "../api";
import type {
  AgentEnrolment,
  AgentStatus,
  AgentUpdate,
  DeployEvent,
  DockerHost,
  DockerHostInput,
  DockerVersion,
  HostInspection,
  HostsList,
  SSHKeyInfo,
} from "../types";
import { Ago, Spinner } from "./common";
import { CopyButton } from "./ConnectInfo";
import { LANNetworkSetup } from "./NetworkSection";

const EMPTY: DockerHostInput = {
  id: "",
  name: "",
  kind: "ssh",
  ssh: { host: "", port: 22, user: "", hostKey: "" },
  socket: "",
  url: "",
  ca: "",
  cert: "",
  key: "",
  appdataRoot: "",
  address: "",
};

type HostKeyPrompt = { key: string; fingerprint: string; type: string };
type Action = "test" | "save" | "check";

/** Host page → Docker hosts: other machines whose servers hlm manages too. */
export function DockerHosts() {
  const [hosts, setHosts] = useState<DockerHost[]>();
  const [sshKey, setSSHKey] = useState<SSHKeyInfo>();
  const [error, setError] = useState<string>();
  // The host being edited; "" for a new one.
  const [editing, setEditing] = useState<string>();
  // The host whose LAN network (br0) is being set up.
  const [lanFor, setLanFor] = useState<string>();
  const [checked, setChecked] = useState<Record<string, string>>({});
  // What agents are kept on (hlm's version), and updates running from here.
  const [agents, setAgents] = useState<Pick<HostsList, "agentVersion" | "agentAutoUpdate" | "agentUpdateBlocked">>({});
  const [updating, setUpdating] = useState<Record<string, string>>({});

  const load = () =>
    api.hosts().then(
      (r) => {
        setHosts(r.hosts);
        setSSHKey(r.sshKey);
        setAgents({ agentVersion: r.agentVersion, agentAutoUpdate: r.agentAutoUpdate, agentUpdateBlocked: r.agentUpdateBlocked });
      },
      (e) => setError((e as Error).message),
    );

  // While an agent is being updated (by hlm on its own, say), follow it.
  const busy = hosts?.some((h) => h.agentUpdate?.state === "updating") ?? false;
  useEffect(() => {
    if (!busy) return;
    const t = setInterval(load, 3000);
    return () => clearInterval(t);
  }, [busy]);

  async function updateAgent(h: DockerHost) {
    if (!confirm(`Update ${h.name}'s agent to ${agents.agentVersion}? Its connection drops for a few seconds; its servers keep running.`))
      return;
    setUpdating((u) => ({ ...u, [h.id]: "Starting…" }));
    let failed: string | undefined;
    try {
      await api.updateAgent(h.id, (e) => {
        if (e.stage === "error") failed = e.message;
        if (e.message) setUpdating((u) => ({ ...u, [h.id]: e.message! }));
      });
    } catch (e) {
      failed = (e as Error).message;
    }
    setUpdating((u) => {
      const next = { ...u };
      delete next[h.id];
      return next;
    });
    if (failed) setError(`Updating ${h.name}'s agent failed: ${failed}`);
    load();
  }

  async function setAuto(on: boolean) {
    try {
      await api.setAgentAutoUpdate(on);
      setAgents((a) => ({ ...a, agentAutoUpdate: on }));
    } catch (e) {
      setError((e as Error).message);
    }
  }
  useEffect(() => {
    load();
  }, []);

  async function check(id: string) {
    setChecked((c) => ({ ...c, [id]: "Checking…" }));
    try {
      const v = await api.checkHost(id);
      setChecked((c) => ({ ...c, [id]: `Docker ${v.version} (${v.os}/${v.arch})` }));
    } catch (e) {
      setChecked((c) => ({ ...c, [id]: (e as Error).message }));
    }
  }

  async function remove(h: DockerHost) {
    if (
      !confirm(
        `Remove ${h.name}? hlm stops managing its servers; they keep running there, with their data. Their backups, tasks and other settings in hlm are kept, for if you add the host again.` +
          (h.kind === "agent"
            ? " Its agent is disconnected and its credential stops working; remove the hlm-agent container there too."
            : ""),
      )
    )
      return;
    try {
      await api.deleteHost(h.id);
      load();
    } catch (e) {
      setError((e as Error).message);
    }
  }

  const others = hosts?.filter((h) => !h.local) ?? [];
  const local = hosts?.find((h) => h.local);
  return (
    <div className="card docker-hosts">
      <h2>Docker hosts</h2>
      <p className="muted small no-margin">
        Other machines hlm runs servers on, such as a NAS or a mini-PC. Their servers are listed with this host&apos;s,
        named <code>name@host</code>, with their console, files, backups and updates. hlm reaches their Docker over SSH
        (as a user in the <code>docker</code> group) or Docker&apos;s API over TCP with TLS, or through hlm&apos;s agent
        on the host, which connects out to hlm, so nothing has to be opened there. Choose a host at the top of this
        page for its CPU, memory, network, disks and disk usage by server.
      </p>
      {error && <div className="alert error small">{error}</div>}
      {hosts?.some((h) => h.kind === "agent") && (
        <label className="radio small">
          <input
            type="checkbox"
            checked={agents.agentAutoUpdate ?? true}
            onChange={(e) => setAuto(e.target.checked)}
            disabled={!!agents.agentUpdateBlocked}
          />
          Keep agents on hlm&apos;s version: update an agent automatically when it connects on another one
          {agents.agentVersion && !agents.agentUpdateBlocked ? ` (${agents.agentVersion})` : ""}
          {agents.agentUpdateBlocked && <span className="muted"> — {agents.agentUpdateBlocked}</span>}
        </label>
      )}
      {!hosts ? (
        <Spinner />
      ) : (
        <div className="table-scroll">
          <table className="data-table">
            <thead>
              <tr>
                <th>Host</th>
                <th>Connection</th>
                <th>Data folder</th>
                <th>Status</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {local && (
                <tr>
                  <td>
                    <strong>{local.name}</strong> <span className="muted small">(local)</span>
                  </td>
                  <td>
                    <code className="small">{local.url}</code>
                  </td>
                  <td>
                    <code className="small">{local.appdataRoot}</code>
                  </td>
                  <td />
                  <td className="row-actions">
                    <button className="btn small ghost" onClick={() => setLanFor(lanFor === local.id ? undefined : local.id)}>
                      LAN network (br0)
                    </button>
                  </td>
                </tr>
              )}
              {local && lanFor === local.id && (
                <tr>
                  <td colSpan={5}>
                    <LANNetworkSetup host={local.id} onClose={() => setLanFor(undefined)} />
                  </td>
                </tr>
              )}
              {others.map((h) => (
                <Fragment key={h.id}>
                <tr>
                  <td>
                    <strong>{h.name}</strong> <span className="muted small">({h.id})</span>
                    {h.address && <div className="muted small">at {h.address}</div>}
                  </td>
                  <td className="small">
                    {h.kind === "agent" ? (
                      <>
                        <AgentState agent={h.agent} />
                        <AgentUpdateState
                          update={h.agentUpdate}
                          running={updating[h.id]}
                          behind={agentBehind(h, agents.agentVersion, agents.agentUpdateBlocked)}
                          version={agents.agentVersion}
                        />
                      </>
                    ) : h.kind === "ssh" ? (
                      <>
                        SSH{" "}
                        <code>
                          {h.ssh?.user}@{h.ssh?.host}
                          {h.ssh?.port !== 22 ? `:${h.ssh?.port}` : ""}
                        </code>
                      </>
                    ) : (
                      <>
                        TLS <code>{h.url}</code>
                      </>
                    )}
                  </td>
                  <td>
                    <code className="small">{h.appdataRoot}</code>
                  </td>
                  <td className="small">
                    {checked[h.id] ??
                      (h.status ? (
                        h.status.ok ? (
                          <span className="host-ok">Reachable</span>
                        ) : (
                          <span className="host-down" title={h.status.error}>
                            Can&apos;t be reached: {h.status.error}
                          </span>
                        )
                      ) : (
                        <span className="muted">Not listed yet</span>
                      ))}
                  </td>
                  <td className="row-actions">
                    <button className="btn small ghost" onClick={() => check(h.id)}>
                      Check
                    </button>
                    <button className="btn small ghost" onClick={() => setEditing(h.id)}>
                      Edit
                    </button>
                    <button
                      className="btn small ghost"
                      onClick={() => setLanFor(lanFor === h.id ? undefined : h.id)}
                      disabled={h.status ? !h.status.ok : false}
                      title="Give servers there their own LAN address, like Unraid's br0"
                    >
                      LAN network (br0)
                    </button>
                    {agentBehind(h, agents.agentVersion, agents.agentUpdateBlocked) && (
                      <button
                        className="btn small"
                        onClick={() => updateAgent(h)}
                        disabled={!!updating[h.id] || h.agentUpdate?.state === "updating"}
                        title={`Its agent runs ${h.agent?.version || "an older version"}; hlm runs ${agents.agentVersion}`}
                      >
                        Update agent to {agents.agentVersion}
                      </button>
                    )}
                    <button className="btn small ghost danger" onClick={() => remove(h)}>
                      Remove
                    </button>
                  </td>
                </tr>
                {lanFor === h.id && (
                  <tr>
                    <td colSpan={5}>
                      <LANNetworkSetup host={h.id} onClose={() => setLanFor(undefined)} />
                    </td>
                  </tr>
                )}
                </Fragment>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {editing === undefined ? (
        <div className="btn-row">
          <button className="btn small" onClick={() => setEditing("")}>
            Add a host
          </button>
        </div>
      ) : (
        <HostForm
          key={editing}
          initial={editing ? hosts?.find((h) => h.id === editing) : undefined}
          sshKey={sshKey}
          onKey={setSSHKey}
          onDone={() => {
            setEditing(undefined);
            load();
          }}
          onCancel={() => setEditing(undefined)}
        />
      )}
    </div>
  );
}

function HostForm({
  initial,
  sshKey,
  onKey,
  onDone,
  onCancel,
}: {
  initial?: DockerHost;
  sshKey?: SSHKeyInfo;
  onKey: (k: SSHKeyInfo) => void;
  onDone: () => void;
  onCancel: () => void;
}) {
  const [form, setForm] = useState<DockerHostInput>(() =>
    initial ? { ...EMPTY, ...initial, ssh: initial.ssh ?? EMPTY.ssh, key: "" } : { ...EMPTY, ssh: { ...EMPTY.ssh! } },
  );
  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<string>();
  const [version, setVersion] = useState<DockerVersion>();
  const [hostKey, setHostKey] = useState<HostKeyPrompt>();
  // What was being tried when the host key had to be confirmed.
  const [pending, setPending] = useState<Action>("test");
  const [inspection, setInspection] = useState<HostInspection>();
  const [setupEvents, setSetupEvents] = useState<DeployEvent[]>([]);
  // For a TCP host: hlm sets TLS up over SSH, rather than being given the certificates.
  const [auto, setAuto] = useState(!initial);
  const [tlsPort, setTLSPort] = useState(2376);
  // The enrolment made for a new host's agent.
  const [enrolment, setEnrolment] = useState<AgentEnrolment>();
  // For a new agent's host: hlm installs the agent over SSH, rather than showing the command.
  const [agentSSH, setAgentSSH] = useState(false);
  // hlm's address as the new agent's host reaches it.
  const [agentServer, setAgentServer] = useState(window.location.origin);

  const set = <K extends keyof DockerHostInput>(k: K, v: DockerHostInput[K]) => {
    setForm((f) => ({ ...f, [k]: v }));
    setVersion(undefined);
    setInspection(undefined);
  };
  const setSSH = (k: "host" | "port" | "user" | "hostKey", v: string | number) => {
    setForm((f) => ({ ...f, ssh: { ...f.ssh!, [k]: v } }));
    setVersion(undefined);
    setInspection(undefined);
  };

  // Only what the chosen connection needs.
  const payload = (f: DockerHostInput): DockerHostInput =>
    f.kind === "ssh"
      ? { ...f, url: undefined, ca: undefined, cert: undefined, key: undefined }
      : f.kind === "agent"
        ? {
            ...f,
            url: undefined,
            ca: undefined,
            cert: undefined,
            key: undefined,
            ssh: undefined,
            socket: undefined,
            agent: undefined,
          }
        : { ...f, ssh: undefined, socket: undefined };

  // The host as hlm signs in to it over SSH (to check it, or set Docker or TLS up).
  const sshPayload = (f: DockerHostInput): DockerHostInput => ({
    ...f,
    kind: "ssh",
    url: undefined,
    ca: undefined,
    cert: undefined,
    key: undefined,
  });

  async function run(what: Action, f = form) {
    setBusy(what);
    setError(undefined);
    try {
      if (what === "test") {
        setVersion(await api.testHost(payload(f)));
        setInspection(undefined);
      } else if (what === "check") setInspection(await api.inspectHost(sshPayload(f)));
      else {
        await (initial ? api.updateHost(payload(f)) : api.createHost(payload(f)));
        onDone();
      }
    } catch (e) {
      const body = e instanceof ApiError ? e.body : undefined;
      const hk = body?.hostKey as string | undefined;
      if (hk) {
        setPending(what);
        setHostKey({
          key: hk,
          fingerprint: body?.hostKeyFingerprint as string,
          type: (body?.hostKeyType as string) || "ed25519",
        });
      } else {
        setError((e as Error).message);
        // Docker may just not be there: look at the host to say what's missing.
        if (what === "test" && f.kind === "ssh") api.inspectHost(payload(f)).then(setInspection, () => {});
      }
    } finally {
      setBusy(undefined);
    }
  }

  function trust() {
    const f = { ...form, ssh: { ...form.ssh!, hostKey: hostKey!.key } };
    setForm(f);
    setHostKey(undefined);
    run(pending === "save" ? "test" : pending, f);
  }

  async function setUpDocker() {
    if (!confirm(`Run these commands as root on ${form.ssh?.host}? They can take a few minutes.`)) return;
    setBusy("setup");
    setError(undefined);
    setSetupEvents([]);
    let ok = false;
    try {
      await api.setupDocker(sshPayload(form), (e) => {
        setSetupEvents((prev) => [...prev, e]);
        if (e.stage === "done") ok = true;
      });
    } catch (e) {
      setSetupEvents((prev) => [...prev, { stage: "error", message: (e as Error).message }]);
    } finally {
      setBusy(undefined);
    }
    if (ok) await run(form.kind === "ssh" ? "test" : "check");
  }

  async function setUpTLS() {
    if (
      !confirm(
        `Set Docker on ${form.ssh?.host} up for TLS on port ${tlsPort}? Docker restarts, and so do its running containers.`,
      )
    )
      return;
    setBusy("tls");
    setError(undefined);
    setSetupEvents([]);
    let ok = false;
    try {
      await api.setupTLS(
        { ...form, kind: "tcp", url: undefined, ca: undefined, cert: undefined, key: undefined, port: tlsPort },
        (e) => {
          setSetupEvents((prev) => [...prev, e]);
          if (e.stage === "done") ok = true;
        },
      );
    } catch (e) {
      setSetupEvents((prev) => [...prev, { stage: "error", message: (e as Error).message }]);
    } finally {
      setBusy(undefined);
    }
    if (ok) setTimeout(onDone, 1500);
  }

  async function installAgent() {
    if (
      !confirm(
        `Install hlm's agent on ${form.ssh?.host}? hlm starts an hlm-agent container there, which connects to ${agentServer}.`,
      )
    )
      return;
    setBusy("install");
    setError(undefined);
    setSetupEvents([]);
    let ok = false;
    try {
      await api.installAgent({ ...sshPayload(form), kind: "agent", server: agentServer }, (e) => {
        setSetupEvents((prev) => [...prev, e]);
        if (e.stage === "done") ok = true;
      });
    } catch (e) {
      setSetupEvents((prev) => [...prev, { stage: "error", message: (e as Error).message }]);
    } finally {
      setBusy(undefined);
    }
    if (ok) setTimeout(onDone, 1500);
  }

  async function makeKey() {
    try {
      onKey(await api.hostsSSHKey());
    } catch (e) {
      setError((e as Error).message);
    }
  }

  async function enrol() {
    setBusy("enrol");
    setError(undefined);
    try {
      setEnrolment(await api.agentEnrolment(payload(form)));
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(undefined);
    }
  }

  function submit(e: FormEvent) {
    e.preventDefault();
    if (agentKind && !initial) {
      if (agentSSH) installAgent();
      else enrol();
    } else run("save");
  }

  const agentKind = form.kind === "agent";
  const ssh = form.kind === "ssh";
  // Installing a new host's agent over SSH.
  const agentOverSSH = agentKind && agentSSH && !initial;
  // Signing in over SSH: to use the host, to set TLS up on it, or to install its agent.
  const viaSSH = ssh || (form.kind === "tcp" && auto) || agentOverSSH;
  const sshFields = (
    <>
      <div className="field-grid">
        <label className="field">
          <span className="field-label">SSH host</span>
          <input
            value={form.ssh?.host ?? ""}
            onChange={(e) => setSSH("host", e.target.value)}
            placeholder="192.168.1.20"
            spellCheck={false}
            required
          />
        </label>
        <label className="field">
          <span className="field-label">Port</span>
          <input
            type="number"
            value={form.ssh?.port ?? 22}
            onChange={(e) => setSSH("port", Number(e.target.value))}
            min={1}
            max={65535}
          />
        </label>
        <label className="field">
          <span className="field-label">User</span>
          <input
            value={form.ssh?.user ?? ""}
            onChange={(e) => setSSH("user", e.target.value)}
            placeholder="hlm"
            spellCheck={false}
            required
          />
          <span className="field-help">
            {ssh ? (
              <>
                A user in the host&apos;s <code>docker</code> group (or root).
              </>
            ) : agentKind ? (
              <>
                A user in the host&apos;s <code>docker</code> group, root, or a user with passwordless{" "}
                <code>sudo</code>. hlm signs in only to install the agent.
              </>
            ) : (
              <>
                Root, or a user with passwordless <code>sudo</code>: setting TLS up changes how Docker starts.
              </>
            )}
          </span>
        </label>
        {ssh && (
          <label className="field">
            <span className="field-label">Docker socket</span>
            <input
              value={form.socket ?? ""}
              onChange={(e) => set("socket", e.target.value)}
              placeholder="/var/run/docker.sock"
              spellCheck={false}
            />
          </label>
        )}
      </div>
      {sshKey ? (
        <div className="field">
          <span className="field-label">hlm&apos;s public key</span>
          <code className="ssh-key">{sshKey.public}</code>
          <span className="field-help">
            Add it to <code>~{form.ssh?.user || "user"}/.ssh/authorized_keys</code> on the host. It&apos;s the same key
            the Proxy page uses.
          </span>
        </div>
      ) : (
        <p className="muted small">
          hlm doesn&apos;t have an SSH key yet.{" "}
          <button type="button" className="link-button" onClick={makeKey}>
            Make one
          </button>
        </p>
      )}
      {hostKey && (
        <div className="alert warn cert-prompt">
          <strong>First connection to {form.ssh?.host}.</strong> Check that this is the host&apos;s{" "}
          {hostKey.type.toUpperCase()} key fingerprint (on the host:{" "}
          <code>ssh-keygen -lf /etc/ssh/ssh_host_{hostKey.type}_key.pub</code>):
          <code className="fingerprint">{hostKey.fingerprint}</code>
          <div className="btn-row">
            <button type="button" className="btn small primary" onClick={trust}>
              It matches, trust it
            </button>
            <button type="button" className="btn small ghost" onClick={() => setHostKey(undefined)}>
              Cancel
            </button>
          </div>
        </div>
      )}
      {form.ssh?.hostKey && !hostKey && (
        <p className="muted small no-margin">
          Trusting the host&apos;s key.{" "}
          <button type="button" className="link-button" onClick={() => setSSH("hostKey", "")}>
            forget it
          </button>
        </p>
      )}
    </>
  );
  return (
    <form className="config-form host-form" onSubmit={submit}>
      <h3>{initial ? `Change ${initial.name}` : "Add a Docker host"}</h3>
      <div className="field-grid">
        <label className="field">
          <span className="field-label">ID</span>
          <input
            value={form.id}
            onChange={(e) => set("id", e.target.value.toLowerCase())}
            disabled={!!initial || !!enrolment}
            placeholder="nas"
            spellCheck={false}
            required
          />
          <span className="field-help">
            Short, lower-case: servers there are named <code>name@{form.id || "nas"}</code>. It can&apos;t change later.
          </span>
        </label>
        <label className="field">
          <span className="field-label">Name</span>
          <input value={form.name} onChange={(e) => set("name", e.target.value)} placeholder="NAS" />
        </label>
        <label className="field">
          <span className="field-label">Connect with</span>
          <select
            value={form.kind}
            onChange={(e) => set("kind", e.target.value as "ssh" | "tcp" | "agent")}
            disabled={initial?.kind === "agent" || !!enrolment}
          >
            <option value="ssh">SSH (recommended)</option>
            <option value="tcp">Docker&apos;s API over TCP with TLS</option>
            {(!initial || initial.kind === "agent") && (
              <option value="agent">hlm&apos;s agent on the host (it connects to hlm)</option>
            )}
          </select>
        </label>
      </div>

      {agentKind ? (
        initial ? (
          <p className="small no-margin">
            Agent: <AgentState agent={initial.agent} />
          </p>
        ) : (
          <>
            <p className="muted small no-margin">
              The host runs hlm&apos;s agent, a small container that connects out to this hlm over HTTPS and passes hlm
              the Docker calls it makes there, and nothing else. Nothing has to be opened on the host or forwarded to
              it, so it works behind NAT or another network.
            </p>
            <div className="field">
              <label className="radio small">
                <input type="radio" checked={!agentSSH} onChange={() => setAgentSSH(false)} disabled={!!enrolment} />
                Make a one-time command to run there
              </label>
              <label className="radio small">
                <input type="radio" checked={agentSSH} onChange={() => setAgentSSH(true)} disabled={!!enrolment} />
                Let hlm install it over SSH
              </label>
            </div>
            {agentSSH && (
              <>
                {sshFields}
                <div className="field-grid">
                  <label className="field">
                    <span className="field-label">hlm&apos;s address, from the host</span>
                    <input
                      value={agentServer}
                      onChange={(e) => setAgentServer(e.target.value)}
                      placeholder="https://hlm.example.com"
                      spellCheck={false}
                      required
                    />
                    <span className="field-help">
                      Where the agent connects to. This page&apos;s address, unless the host reaches hlm another way.
                      {/^https?:\/\/(localhost|127\.|\[::1\])/.test(agentServer) && (
                        <strong> That&apos;s this machine&apos;s own address, which the other host can&apos;t reach.</strong>
                      )}
                    </span>
                  </label>
                </div>
              </>
            )}
          </>
        )
      ) : ssh ? (
        <>{sshFields}</>
      ) : (
        <>
          <div className="field">
            <label className="radio small">
              <input type="radio" checked={auto} onChange={() => setAuto(true)} />
              Let hlm make the certificates and set Docker up for TLS, over SSH
            </label>
            <label className="radio small">
              <input type="radio" checked={!auto} onChange={() => setAuto(false)} />I have the certificates (Docker is
              already set up with <code>--tlsverify</code>)
            </label>
          </div>
          {auto ? (
            <>
              <p className="muted small no-margin">
                hlm signs in once, makes a CA for this host, a certificate for its Docker and one for hlm, and starts
                Docker with its API on a TCP port, accepting only hlm&apos;s certificate (as in Docker&apos;s{" "}
                <a href="https://docs.docker.com/engine/security/protect-access/" target="_blank" rel="noreferrer">
                  Protect the Docker daemon socket
                </a>
                ). After that, it connects over TLS, not SSH.
              </p>
              {sshFields}
              <div className="field-grid">
                <label className="field">
                  <span className="field-label">Docker&apos;s TLS port</span>
                  <input
                    type="number"
                    value={tlsPort}
                    onChange={(e) => setTLSPort(Number(e.target.value))}
                    min={1}
                    max={65535}
                  />
                  <span className="field-help">Open it in the host&apos;s firewall for this machine.</span>
                </label>
              </div>
            </>
          ) : (
            <>
              <div className="field-grid">
                <label className="field">
                  <span className="field-label">Address</span>
                  <input
                    value={form.url ?? ""}
                    onChange={(e) => set("url", e.target.value)}
                    placeholder="tcp://nas.lan:2376"
                    spellCheck={false}
                    required
                  />
                  <span className="field-help">
                    Docker has to be set up for TLS (<code>--tlsverify</code>). hlm always checks the daemon&apos;s
                    certificate, and won&apos;t use plain TCP.
                  </span>
                </label>
              </div>
              <label className="field">
                <span className="field-label">CA certificate (ca.pem)</span>
                <textarea
                  className="code-input"
                  rows={3}
                  value={form.ca ?? ""}
                  onChange={(e) => set("ca", e.target.value)}
                  placeholder="-----BEGIN CERTIFICATE-----"
                  spellCheck={false}
                />
                <span className="field-help">
                  What signed the daemon&apos;s certificate. Empty uses the usual public authorities.
                </span>
              </label>
              <div className="field-grid">
                <label className="field">
                  <span className="field-label">Client certificate (cert.pem)</span>
                  <textarea
                    className="code-input"
                    rows={3}
                    value={form.cert ?? ""}
                    onChange={(e) => set("cert", e.target.value)}
                    placeholder="-----BEGIN CERTIFICATE-----"
                    spellCheck={false}
                  />
                </label>
                <label className="field">
                  <span className="field-label">Client key (key.pem)</span>
                  <textarea
                    className="code-input"
                    rows={3}
                    value={form.key ?? ""}
                    onChange={(e) => set("key", e.target.value)}
                    placeholder={initial?.hasKey ? "Saved: leave empty to keep it" : "-----BEGIN PRIVATE KEY-----"}
                    spellCheck={false}
                  />
                  <span className="field-help">Kept encrypted with hlm&apos;s settings; never shown again.</span>
                </label>
              </div>
            </>
          )}
        </>
      )}

      <div className="field-grid">
        <label className="field">
          <span className="field-label">Data folder on the host</span>
          <input
            value={form.appdataRoot ?? ""}
            onChange={(e) => set("appdataRoot", e.target.value)}
            placeholder="/mnt/user/appdata"
            spellCheck={false}
            required
            disabled={!!enrolment}
          />
          <span className="field-help">Where servers&apos; data folders go there, like HLM_APPDATA_ROOT here.</span>
        </label>
        <label className="field">
          <span className="field-label">LAN address{agentKind && !agentOverSSH ? "" : " (optional)"}</span>
          <input
            value={form.address ?? ""}
            onChange={(e) => set("address", e.target.value)}
            placeholder={viaSSH ? form.ssh?.host : agentKind ? "192.168.1.20" : "the address's host"}
            spellCheck={false}
            required={agentKind && !agentOverSSH}
            disabled={!!enrolment}
          />
          <span className="field-help">
            Where its servers&apos; ports are reached: for Web UI links, status checks, port forwards and tunnels. An IP
            address is best.
          </span>
        </label>
      </div>

      {error && <div className="alert error small">{error}</div>}
      {viaSSH && inspection && (
        <HostCheck inspection={inspection} busy={busy === "setup"} disabled={!!busy} onSetUp={setUpDocker} />
      )}
      {setupEvents.length > 0 && <SetupLog events={setupEvents} />}
      {version && (
        <div className="alert success small">
          Connected: Docker {version.version} (API {version.apiVersion}, {version.os}/{version.arch}).
        </div>
      )}
      {enrolment && <AgentCommand enrolment={enrolment} onConnected={onDone} />}
      <div className="btn-row">
        {agentOverSSH ? (
          <>
            <button type="button" className="btn" onClick={() => run("check")} disabled={!!busy}>
              {busy === "check" ? "Checking…" : "Check for Docker"}
            </button>
            <button
              type="submit"
              className="btn primary"
              disabled={!!busy || !form.ssh?.hostKey || !inspection?.running}
              title={!inspection?.running ? "Check for Docker first: it has to be installed and running" : undefined}
            >
              {busy === "install" ? "Installing…" : "Install the agent and add host"}
            </button>
          </>
        ) : agentKind ? (
          !enrolment && (
            <button type="submit" className="btn primary" disabled={!!busy}>
              {initial ? (busy === "save" ? "Saving…" : "Save") : busy === "enrol" ? "Making it…" : "Make the command"}
            </button>
          )
        ) : (
          <>
            {!(auto && !ssh) && (
              <button type="button" className="btn" onClick={() => run("test")} disabled={!!busy}>
                {busy === "test" ? "Testing…" : "Test connection"}
              </button>
            )}
            {viaSSH && (
              <button type="button" className="btn" onClick={() => run("check")} disabled={!!busy}>
                {busy === "check" ? "Checking…" : "Check for Docker"}
              </button>
            )}
            {auto && !ssh ? (
              <button
                type="button"
                className="btn primary"
                onClick={setUpTLS}
                disabled={!!busy || !form.ssh?.hostKey || !inspection?.ready}
                title={!inspection?.ready ? "Check for Docker first: it has to be installed and running" : undefined}
              >
                {busy === "tls" ? "Setting up…" : initial ? "Set TLS up and save" : "Set TLS up and add host"}
              </button>
            ) : (
              <button type="submit" className="btn primary" disabled={!!busy || (ssh && !form.ssh?.hostKey)}>
                {busy === "save" ? "Saving…" : initial ? "Save" : "Add host"}
              </button>
            )}
          </>
        )}
        <button type="button" className="btn ghost" onClick={onCancel}>
          {enrolment ? "Close" : "Cancel"}
        </button>
      </div>
    </form>
  );
}

const DISTRO_PAGE: Record<string, string> = {
  ubuntu: "ubuntu",
  debian: "debian",
  raspbian: "raspberry-pi-os",
  fedora: "fedora",
  centos: "centos",
  rhel: "rhel",
};

/** What hlm found on the host, and the button to set Docker up there. */
function HostCheck({
  inspection: i,
  busy,
  disabled,
  onSetUp,
}: {
  inspection: HostInspection;
  busy: boolean;
  disabled: boolean;
  onSetUp: () => void;
}) {
  const docker = i.dockerVersion
    ? `Docker ${i.dockerVersion}, usable by ${i.user}`
    : !i.docker
      ? "Docker isn't installed"
      : !i.running
        ? "Docker is installed but not running"
        : `Docker is running, but ${i.user} can't use it (not in the docker group)`;
  const action = !i.docker ? "Install Docker" : !i.running ? "Start Docker" : `Let ${i.user} use Docker`;
  return (
    <div className={`alert ${i.ready ? "success" : "info"} small host-check`}>
      <div>
        <strong>{i.os || "Unknown Linux"}</strong> ({i.arch}), signed in as <code>{i.user}</code>
        {i.root ? " (root)" : i.sudo ? " (passwordless sudo)" : ""}. {docker}.
      </div>
      {!i.ready && i.reason && <div>{i.reason}</div>}
      {i.notes?.map((n) => (
        <div key={n}>{n}</div>
      ))}
      {!i.ready && i.script && (
        <>
          {!i.docker && i.distro && (
            <div>
              hlm installs Docker CE from Docker&apos;s own repository, following{" "}
              <a
                href={`https://docs.docker.com/engine/install/${DISTRO_PAGE[i.distro]}/`}
                target="_blank"
                rel="noreferrer"
              >
                Docker&apos;s install steps for {i.distro === "raspbian" ? "Raspberry Pi OS" : i.os}
              </a>
              , starts it on boot, and adds <code>{i.user}</code> to the <code>docker</code> group.
            </div>
          )}
          <details>
            <summary>The commands{i.canSetUp ? " hlm runs as root" : " to run as root"}</summary>
            <pre className="setup-script">{i.script}</pre>
          </details>
          {i.canSetUp && (
            <div className="btn-row">
              <button type="button" className="btn small primary" onClick={onSetUp} disabled={disabled}>
                {busy ? "Working… (this can take a few minutes)" : action}
              </button>
            </div>
          )}
        </>
      )}
    </div>
  );
}

/** The output of setting Docker up, as it runs. */
function SetupLog({ events }: { events: DeployEvent[] }) {
  const lines = events.filter((e) => e.stage === "log").map((e) => e.message);
  const end = events.find((e) => e.stage === "done" || e.stage === "error");
  return (
    <div className="setup-log">
      <pre
        ref={(el) => {
          if (el) el.scrollTop = el.scrollHeight;
        }}
      >
        {lines.join("\n")}
      </pre>
      {end && <div className={`alert ${end.stage === "done" ? "success" : "error"} small`}>{end.message}</div>}
    </div>
  );
}

/** A host's agent's connection, in a few words. */
function AgentState({ agent }: { agent?: AgentStatus }) {
  if (agent?.connected)
    return (
      <span className="host-ok">
        Agent connected <Ago iso={agent.since} />
        {agent.version && <span className="muted"> (version {agent.version})</span>}
      </span>
    );
  return (
    <span className="host-down">
      Agent not connected
      {agent?.lastSeen ? (
        <>
          {" "}
          (last seen <Ago iso={agent.lastSeen} />)
        </>
      ) : (
        " since hlm started"
      )}
    </span>
  );
}

/** The command to run on a new host, and then waiting for its agent to connect. */
function AgentCommand({ enrolment, onConnected }: { enrolment: AgentEnrolment; onConnected: () => void }) {
  const [connected, setConnected] = useState(false);
  const server = window.location.origin;
  const http = window.location.protocol === "http:";
  const args = `agent --server ${server}${http ? " --allow-http" : ""} --token ${enrolment.token}`;
  const docker = [
    "docker run -d --name hlm-agent --restart unless-stopped \\",
    "  -v /var/run/docker.sock:/var/run/docker.sock -v hlm-agent:/data \\",
    "  -v /proc:/host/proc:ro -v /:/host/rootfs:ro \\",
    `  danotchuuy/home-lab-manager:latest ${args}`,
  ].join("\n");
  const id = enrolment.host.id;
  useEffect(() => {
    if (connected) return;
    const t = setInterval(() => {
      api.hosts().then(
        (r) => {
          if (r.hosts.some((h) => h.id === id && h.agent?.connected)) setConnected(true);
        },
        () => {},
      );
    }, 3000);
    return () => clearInterval(t);
  }, [id, connected]);
  useEffect(() => {
    if (!connected) return;
    const t = setTimeout(onConnected, 2500);
    return () => clearTimeout(t);
  }, [connected, onConnected]);
  return (
    <div className="agent-command">
      <p className="small no-margin">
        Run this on <strong>{enrolment.host.name || id}</strong> (as root, or a user in the <code>docker</code> group):
      </p>
      <div className="agent-command-box">
        <pre>{docker}</pre>
        <CopyButton text={docker} label="Copy the command" />
      </div>
      <p className="muted small no-margin">
        Or, with the <code>hlm</code> binary there: <code>hlm {args.replace(enrolment.token, "…")} --data DIR</code>.
        The token works once, until {new Date(enrolment.expires).toLocaleTimeString([], { timeStyle: "short" })}, and
        isn&apos;t shown again; the agent trades it for its own credential, kept in the <code>hlm-agent</code> volume.
        Running the same command again later (to update the agent) keeps that credential. The read-only{" "}
        <code>/proc</code> and <code>/</code> mounts are for the host&apos;s CPU, memory, network and disk figures;
        without them, only its Docker is used.
      </p>
      {http && (
        <div className="alert warn small">
          hlm is open over plain HTTP here, so the agent&apos;s connection isn&apos;t encrypted: anyone who can see or
          change the traffic between the host and hlm could control its Docker. Use it on a network you trust, or open
          hlm over HTTPS and make the command there.
        </div>
      )}
      {connected ? (
        <div className="alert success small">Connected: {enrolment.host.name || id} is added.</div>
      ) : (
        <p className="small no-margin muted">Waiting for the agent to connect…</p>
      )}
    </div>
  );
}

/** Whether a host's agent is connected on another version than hlm's, and can be updated. */
function agentBehind(h: DockerHost, version?: string, blocked?: string): boolean {
  return h.kind === "agent" && !blocked && !!version && !!h.agent?.connected && !!h.agent.version && h.agent.version !== version;
}

/** An agent's update: running, how the last one ended, or that it's behind hlm. */
function AgentUpdateState({
  update,
  running,
  behind,
  version,
}: {
  update?: AgentUpdate;
  running?: string;
  behind: boolean;
  version?: string;
}) {
  if (running || update?.state === "updating") {
    return (
      <div className="small">
        <Spinner /> Updating the agent{update?.to ? ` to ${update.to}` : ""}: {running ?? update?.message ?? "…"}
      </div>
    );
  }
  if (update?.state === "failed" && update.to === version) {
    return (
      <div className="small host-down">
        {update.auto ? "Automatic update" : "Update"} to {update.to} failed: {update.message}
      </div>
    );
  }
  if (update?.state === "done" && !behind) {
    return (
      <div className="small muted">
        Updated to {update.to} <Ago iso={update.at} />
      </div>
    );
  }
  if (behind) return <div className="small muted">hlm runs {version}</div>;
  return null;
}
