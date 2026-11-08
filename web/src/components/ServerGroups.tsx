import { useEffect, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { api } from "../api";
import type { Account, APIKeyInfo, DockerHost, GroupRole, GrantKind, Server, ServerGroup } from "../types";
import { Spinner } from "./common";

const KINDS: { kind: GrantKind; label: string }[] = [
  { kind: "local", label: "Local account" },
  { kind: "oidc-user", label: "Single sign-on user" },
  { kind: "oidc-group", label: "Single sign-on group" },
  { kind: "key", label: "API key" },
];

const ROLES: { role: GroupRole; label: string }[] = [
  { role: "viewer", label: "Viewer: status only" },
  { role: "operator", label: "Operator: run, logs, read files, back up" },
  { role: "manager", label: "Manager: also settings, files, ports, restore" },
];

interface Lists {
  servers: Server[];
  hosts: DockerHost[];
  accounts: Account[];
  keys: APIKeyInfo[];
}

/** Groups of servers, and who may use the servers in each. Only admins get here. */
export function ServerGroups() {
  const [groups, setGroups] = useState<ServerGroup[]>();
  const [lists, setLists] = useState<Lists>();
  const [signInRequired, setSignInRequired] = useState(true);
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  async function load() {
    const [g, servers, accounts, keys, status, hosts] = await Promise.all([
      api.groups(),
      api.servers(),
      api.accounts(),
      api.apiKeys(),
      api.authStatus(),
      api.hosts().then(
        (r) => r.hosts,
        () => [] as DockerHost[],
      ),
    ]);
    setGroups(g.map((x) => ({ ...x, hosts: x.hosts ?? [] })));
    setLists({ servers, accounts, keys, hosts });
    setSignInRequired(status.required || status.token);
  }

  useEffect(() => {
    load().catch((e) => setError((e as Error).message));
  }, []);

  async function run(f: () => Promise<unknown>) {
    setBusy(true);
    setError(undefined);
    try {
      await f();
      await load();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  function add(e: FormEvent) {
    e.preventDefault();
    run(async () => {
      await api.createGroup({ name: name.trim(), servers: [], grants: [] });
      setName("");
    });
  }

  if (!groups || !lists) return error ? <div className="alert error">{error}</div> : <Spinner />;

  return (
    <div className="server-groups">
      <p className="muted small no-margin">
        Put servers in a group and choose who may use them. Admins can use every server; everyone else sees only the
        servers their groups give them.
      </p>
      {!signInRequired && (
        <div className="alert error">
          Sign-in isn&apos;t required, so anyone who can reach Home Lab Manager is an admin and these groups limit nobody.
          Require sign-in under Sign-in below.
        </div>
      )}
      {error && <div className="alert error">{error}</div>}
      {groups.map((g) => (
        <GroupCard key={`${g.id}:${JSON.stringify(g)}`} group={g} lists={lists} busy={busy} run={run} />
      ))}
      <form className="inline-form" onSubmit={add}>
        <label className="field">
          <span className="field-label">New group</span>
          <input value={name} onChange={(e) => setName(e.target.value)} placeholder="e.g. Friends' game servers" maxLength={64} required />
        </label>
        <button className="btn small" type="submit" disabled={busy || !name.trim()}>
          Add group
        </button>
      </form>
    </div>
  );
}

function GroupCard({ group, lists, busy, run }: { group: ServerGroup; lists: Lists; busy: boolean; run: (f: () => Promise<unknown>) => Promise<void> }) {
  const [draft, setDraft] = useState(group);
  const dirty = JSON.stringify(draft) !== JSON.stringify(group);
  const incomplete = !draft.name.trim() || draft.grants.some((g) => !g.subject.name.trim());

  const draftHosts = draft.hosts ?? [];
  // The host a server is on: after its @, or this one.
  const hostOf = (server: string) => (server.includes("@") ? server.slice(server.lastIndexOf("@") + 1) : "local");

  function toggleHost(id: string, on: boolean) {
    setDraft((d) => ({ ...d, hosts: on ? [...(d.hosts ?? []), id] : (d.hosts ?? []).filter((h) => h !== id) }));
  }

  function toggleServer(name: string, on: boolean) {
    setDraft((d) => ({ ...d, servers: on ? [...d.servers, name] : d.servers.filter((s) => s !== name) }));
  }

  function setGrant(i: number, change: (g: ServerGroup["grants"][number]) => ServerGroup["grants"][number]) {
    setDraft((d) => ({ ...d, grants: d.grants.map((g, n) => (n === i ? change(g) : g)) }));
  }

  function remove() {
    if (!confirm(`Remove the group ${group.name}? Its servers stay, but anyone it gave access loses it.`)) return;
    run(() => api.deleteGroup(group.id));
  }

  // Servers in the group that are gone from Docker can still be taken out.
  const names = [...new Set([...lists.servers.map((s) => s.name), ...group.servers])].sort();

  return (
    <fieldset className="config-section" disabled={busy}>
      <legend>{group.name}</legend>
      <label className="field">
        <span className="field-label">Name</span>
        <input value={draft.name} onChange={(e) => setDraft({ ...draft, name: e.target.value })} maxLength={64} required />
      </label>

      {lists.hosts.length > 1 && (
        <>
          <span className="field-label">Whole Docker hosts</span>
          <p className="muted small no-margin">Every server on a host, including ones added there later.</p>
          <div className="group-servers">
            {lists.hosts.map((h) => (
              <label key={h.id} className="small">
                <input type="checkbox" checked={draftHosts.includes(h.id)} onChange={(e) => toggleHost(h.id, e.target.checked)} />{" "}
                {h.name}
                {h.local ? "" : ` (${h.id})`}
              </label>
            ))}
          </div>
        </>
      )}

      <span className="field-label">Servers</span>
      {names.length === 0 && <p className="muted small no-margin">No servers yet.</p>}
      <div className="group-servers">
        {names.map((n) => {
          const byHost = draftHosts.includes(hostOf(n));
          return (
            <label key={n} className="small" title={byHost ? "In the group with its whole host" : undefined}>
              <input
                type="checkbox"
                checked={byHost || draft.servers.includes(n)}
                disabled={byHost}
                onChange={(e) => toggleServer(n, e.target.checked)}
              />{" "}
              {n}
            </label>
          );
        })}
      </div>

      <span className="field-label">Who has access</span>
      {draft.grants.map((g, i) => (
        <div key={i} className="group-grant">
          <select
            aria-label="Kind"
            value={g.subject.kind}
            onChange={(e) => setGrant(i, (x) => ({ ...x, subject: { kind: e.target.value as GrantKind, name: "" } }))}
          >
            {KINDS.map((k) => (
              <option key={k.kind} value={k.kind}>
                {k.label}
              </option>
            ))}
          </select>
          <SubjectName grantKind={g.subject.kind} value={g.subject.name} lists={lists} onChange={(name) => setGrant(i, (x) => ({ ...x, subject: { ...x.subject, name } }))} />
          <select aria-label="Role" value={g.role} onChange={(e) => setGrant(i, (x) => ({ ...x, role: e.target.value as GroupRole }))}>
            {ROLES.map((r) => (
              <option key={r.role} value={r.role}>
                {r.label}
              </option>
            ))}
          </select>
          <button
            type="button"
            className="btn small ghost danger"
            onClick={() => setDraft((d) => ({ ...d, grants: d.grants.filter((_, n) => n !== i) }))}
          >
            Remove
          </button>
        </div>
      ))}
      <div className="btn-row">
        <button
          type="button"
          className="btn small ghost"
          onClick={() => setDraft((d) => ({ ...d, grants: [...d.grants, { subject: { kind: "local", name: "" }, role: "viewer" }] }))}
        >
          Add access
        </button>
        <div className="spacer" />
        <button type="button" className="btn small ghost danger" onClick={remove}>
          Remove group
        </button>
        <button type="button" className="btn small primary" disabled={!dirty || incomplete} onClick={() => run(() => api.updateGroup(group.id, draft))}>
          Save
        </button>
      </div>
    </fieldset>
  );
}

function SubjectName({ grantKind, value, lists, onChange }: { grantKind: GrantKind; value: string; lists: Lists; onChange: (name: string) => void }) {
  const options =
    grantKind === "local"
      ? lists.accounts.filter((a) => a.role !== "admin").map((a) => ({ value: a.username, label: a.username }))
      : grantKind === "key"
        ? lists.keys.filter((k) => k.role !== "admin").map((k) => ({ value: k.id, label: k.name }))
        : undefined;
  if (!options) {
    return (
      <input
        aria-label="Name"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={grantKind === "oidc-user" ? "Username or email" : "Group name"}
        spellCheck={false}
      />
    );
  }
  return (
    <select aria-label="Who" value={value} onChange={(e) => onChange(e.target.value)}>
      <option value="">Choose…</option>
      {options.map((o) => (
        <option key={o.value} value={o.value}>
          {o.label}
        </option>
      ))}
      {value && !options.some((o) => o.value === value) && <option value={value}>{value}</option>}
    </select>
  );
}

/** Picks the groups a server is in, on its Settings tab. */
export function ServerGroupPicker({ server, onChanged }: { server: Server; onChanged: () => void }) {
  const [groups, setGroups] = useState<ServerGroup[]>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const load = () => api.groups().then(setGroups, (e) => setError((e as Error).message));
  useEffect(() => {
    load();
  }, []);

  // The host it's on: after its @, or this one, as groups name hosts.
  const serverHost = server.name.includes("@") ? server.name.slice(server.name.lastIndexOf("@") + 1) : "local";

  async function toggle(id: string, on: boolean) {
    const current = groups!.filter((g) => g.servers.includes(server.name)).map((g) => g.id);
    setBusy(true);
    setError(undefined);
    try {
      await api.setServerGroups(server.name, on ? [...current, id] : current.filter((g) => g !== id));
      await load();
      onChanged();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <h2>Server groups</h2>
      <p className="muted small">
        Who may use this server besides admins. Groups are managed under <Link to="/system#server-groups">System → Access</Link>.
      </p>
      {error && <div className="alert error">{error}</div>}
      {!groups && !error && <Spinner />}
      {groups?.length === 0 && <p className="muted small">There are no groups yet.</p>}
      <div className="group-servers">
        {groups?.map((g) => {
          // In the group with every server on its host: set under System → Server groups.
          const viaHost = (g.hosts ?? []).includes(serverHost);
          return (
            <label key={g.id} className="small" title={viaHost ? "Every server on its host is in this group (System → Access → Server groups)" : undefined}>
              <input
                type="checkbox"
                checked={viaHost || g.servers.includes(server.name)}
                disabled={busy || viaHost}
                onChange={(e) => toggle(g.id, e.target.checked)}
              />{" "}
              {g.name}
              {viaHost && <span className="muted"> (through its host)</span>}
            </label>
          );
        })}
      </div>
    </>
  );
}
