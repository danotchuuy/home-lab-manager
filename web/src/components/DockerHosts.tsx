import { useEffect, useState, type FormEvent } from "react";
import { api, ApiError } from "../api";
import type { DeployEvent, DockerHost, DockerHostInput, DockerVersion, HostInspection, SSHKeyInfo } from "../types";
import { Spinner } from "./common";

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
  const [checked, setChecked] = useState<Record<string, string>>({});

  const load = () =>
    api.hosts().then(
      (r) => {
        setHosts(r.hosts);
        setSSHKey(r.sshKey);
      },
      (e) => setError((e as Error).message),
    );
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
        `Remove ${h.name}? hlm stops managing its servers; they keep running there, with their data. Their backups, tasks and other settings in hlm are kept, for if you add the host again.`,
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
        (as a user in the <code>docker</code> group) or Docker&apos;s API over TCP with TLS. The CPU, memory and disks
        on this page are this host&apos;s only.
      </p>
      {error && <div className="alert error small">{error}</div>}
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
                  <td />
                </tr>
              )}
              {others.map((h) => (
                <tr key={h.id}>
                  <td>
                    <strong>{h.name}</strong> <span className="muted small">({h.id})</span>
                    {h.address && <div className="muted small">at {h.address}</div>}
                  </td>
                  <td className="small">
                    {h.kind === "ssh" ? (
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
                    <button className="btn small ghost danger" onClick={() => remove(h)}>
                      Remove
                    </button>
                  </td>
                </tr>
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
      : { ...f, ssh: undefined, socket: undefined };

  async function run(what: Action, f = form) {
    setBusy(what);
    setError(undefined);
    try {
      if (what === "test") {
        setVersion(await api.testHost(payload(f)));
        setInspection(undefined);
      } else if (what === "check") setInspection(await api.inspectHost(payload(f)));
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
      await api.setupDocker(payload(form), (e) => {
        setSetupEvents((prev) => [...prev, e]);
        if (e.stage === "done") ok = true;
      });
    } catch (e) {
      setSetupEvents((prev) => [...prev, { stage: "error", message: (e as Error).message }]);
    } finally {
      setBusy(undefined);
    }
    if (ok) await run("test");
  }

  async function makeKey() {
    try {
      onKey(await api.hostsSSHKey());
    } catch (e) {
      setError((e as Error).message);
    }
  }

  function submit(e: FormEvent) {
    e.preventDefault();
    run("save");
  }

  const ssh = form.kind === "ssh";
  return (
    <form className="config-form host-form" onSubmit={submit}>
      <h3>{initial ? `Change ${initial.name}` : "Add a Docker host"}</h3>
      <div className="field-grid">
        <label className="field">
          <span className="field-label">ID</span>
          <input
            value={form.id}
            onChange={(e) => set("id", e.target.value.toLowerCase())}
            disabled={!!initial}
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
          <select value={form.kind} onChange={(e) => set("kind", e.target.value as "ssh" | "tcp")}>
            <option value="ssh">SSH (recommended)</option>
            <option value="tcp">Docker&apos;s API over TCP with TLS</option>
          </select>
        </label>
      </div>

      {ssh ? (
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
                A user in the host&apos;s <code>docker</code> group (or root).
              </span>
            </label>
            <label className="field">
              <span className="field-label">Docker socket</span>
              <input
                value={form.socket ?? ""}
                onChange={(e) => set("socket", e.target.value)}
                placeholder="/var/run/docker.sock"
                spellCheck={false}
              />
            </label>
          </div>
          {sshKey ? (
            <div className="field">
              <span className="field-label">hlm&apos;s public key</span>
              <code className="ssh-key">{sshKey.public}</code>
              <span className="field-help">
                Add it to <code>~{form.ssh?.user || "user"}/.ssh/authorized_keys</code> on the host. It&apos;s the same
                key the Proxy page uses.
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

      <div className="field-grid">
        <label className="field">
          <span className="field-label">Data folder on the host</span>
          <input
            value={form.appdataRoot ?? ""}
            onChange={(e) => set("appdataRoot", e.target.value)}
            placeholder="/mnt/user/appdata"
            spellCheck={false}
            required
          />
          <span className="field-help">Where servers&apos; data folders go there, like HLM_APPDATA_ROOT here.</span>
        </label>
        <label className="field">
          <span className="field-label">LAN address (optional)</span>
          <input
            value={form.address ?? ""}
            onChange={(e) => set("address", e.target.value)}
            placeholder={ssh ? form.ssh?.host : "the address's host"}
            spellCheck={false}
          />
          <span className="field-help">
            Where its servers&apos; ports are reached: for Web UI links, status checks, port forwards and tunnels. An IP
            address is best.
          </span>
        </label>
      </div>

      {error && <div className="alert error small">{error}</div>}
      {ssh && inspection && (
        <HostCheck inspection={inspection} busy={busy === "setup"} disabled={!!busy} onSetUp={setUpDocker} />
      )}
      {setupEvents.length > 0 && <SetupLog events={setupEvents} />}
      {version && (
        <div className="alert success small">
          Connected: Docker {version.version} (API {version.apiVersion}, {version.os}/{version.arch}).
        </div>
      )}
      <div className="btn-row">
        <button type="button" className="btn" onClick={() => run("test")} disabled={!!busy}>
          {busy === "test" ? "Testing…" : "Test connection"}
        </button>
        {ssh && (
          <button type="button" className="btn" onClick={() => run("check")} disabled={!!busy}>
            {busy === "check" ? "Checking…" : "Check for Docker"}
          </button>
        )}
        <button type="submit" className="btn primary" disabled={!!busy || (ssh && !form.ssh?.hostKey)}>
          {busy === "save" ? "Saving…" : initial ? "Save" : "Add host"}
        </button>
        <button type="button" className="btn ghost" onClick={onCancel}>
          Cancel
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
