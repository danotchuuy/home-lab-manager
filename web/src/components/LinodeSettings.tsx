import { useCallback, useEffect, useState } from "react";
import { api } from "../api";
import type { FirewallRule, FirewallStatus, FirewallTest } from "../types";
import { Spinner } from "./common";

export const ruleText = (r: FirewallRule) => `${r.protocol} ${r.ports ?? "all ports"}`;

/** One line on whether the managed firewall opens the relayed ports. */
export function FirewallRulesLine({ status }: { status: FirewallStatus }) {
  if (status.error) return <li className="bad">{status.error}</li>;
  return (
    <li className={status.inSync ? "ok" : "bad"}>
      {status.inSync
        ? status.rules.length
          ? `Open for the proxy's relays: ${status.rules.map(ruleText).join("; ")}`
          : "No relays applied, so no ports are open for them"
        : `Out of date: should open ${status.wanted.length ? status.wanted.map(ruleText).join("; ") : "nothing"}${
            status.rules.length ? `, has ${status.rules.map(ruleText).join("; ")}` : ""
          }`}
    </li>
  );
}

/**
 * Connects hlm to Linode with an API token and chooses the Cloud Firewall it
 * keeps open for the proxy's relays. The token never comes back from the
 * server.
 */
export function LinodeSettings() {
  const [status, setStatus] = useState<FirewallStatus>();
  const [editingToken, setEditingToken] = useState(false);
  const [token, setToken] = useState("");
  const [choices, setChoices] = useState<FirewallTest>();
  const [firewallId, setFirewallId] = useState(0);
  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  // Set when removing failed because hlm's rules couldn't be taken off.
  const [offerKeep, setOfferKeep] = useState(false);

  // Lists the firewalls the saved token can manage.
  const loadChoices = useCallback(async (current: number) => {
    try {
      const t = await api.testLinode("");
      setChoices(t);
      setFirewallId(current || t.suggested || 0);
    } catch (e) {
      setError((e as Error).message);
    }
  }, []);

  useEffect(() => {
    api
      .linode()
      .then((st) => {
        setStatus(st);
        if (st.configured) loadChoices(st.firewallId ?? 0);
      })
      .catch((e) => setError((e as Error).message));
  }, [loadChoices]);

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

  const test = () =>
    run("test", async () => {
      const t = await api.testLinode(token);
      const n = t.firewalls.length;
      setNotice(`Connected. The token can manage ${n} Cloud Firewall${n === 1 ? "" : "s"}.`);
    });

  const saveToken = () =>
    run("save", async () => {
      const st = await api.saveLinode(token, status?.firewallId ?? 0);
      setStatus(st);
      setToken("");
      setEditingToken(false);
      setNotice(st.firewallId ? "Saved the new token." : "Connected. Now choose the firewall to manage.");
      await loadChoices(st.firewallId ?? 0);
    });

  const saveFirewall = () =>
    run("firewall", async () => {
      const st = await api.saveLinode("", firewallId);
      setStatus(st);
      if (!st.error)
        setNotice(
          firewallId
            ? "Saved. hlm keeps this firewall open for the proxy's relays."
            : "hlm no longer manages a firewall; its rules were removed.",
        );
    });

  const sync = () =>
    run("sync", async () => {
      setStatus(await api.syncProxyFirewall());
      setNotice("The firewall is up to date.");
    });

  const remove = (keepRules = false) => {
    if (!keepRules && !confirm("Disconnect Linode? hlm removes its rules from the firewall first, closing the relayed ports."))
      return;
    run("remove", async () => {
      try {
        await api.deleteLinode(keepRules);
      } catch (e) {
        setOfferKeep(true);
        throw e;
      }
      setOfferKeep(false);
      setChoices(undefined);
      setFirewallId(0);
      setStatus(await api.linode());
    });
  };

  if (!status) return error ? <div className="alert error">{error}</div> : <Spinner />;

  const messages = (
    <>
      {error && (
        <div className="alert error">
          {error}
          {offerKeep && (
            <div className="btn-row">
              <button className="btn small ghost" onClick={() => remove(true)} disabled={!!busy}>
                Disconnect anyway, leaving the ports open
              </button>
            </div>
          )}
        </div>
      )}
      {notice && <div className="alert success">{notice}</div>}
    </>
  );

  if (!status.configured || editingToken) {
    return (
      <div className="linode config-form">
        <p className="muted small no-margin">
          If the proxy server is a Linode, hlm can keep its Cloud Firewall open for the relayed game ports: it opens them
          when you apply the proxy and closes them when you remove the relays.
        </p>
        <label className="field">
          <span className="field-label">API token</span>
          <input
            type="password"
            value={token}
            onChange={(e) => setToken(e.target.value)}
            placeholder={status.hasToken ? "New token" : ""}
            autoComplete="off"
          />
          <span className="field-help">
            A personal access token (Cloud Manager → My Profile → API Tokens) with <strong>Firewalls: Read/Write</strong> and{" "}
            <strong>Linodes: Read Only</strong>. It&apos;s stored encrypted on this machine and never shown again.
          </span>
        </label>
        {messages}
        <div className="btn-row">
          <button className="btn small" onClick={test} disabled={!!busy || !token.trim()}>
            {busy === "test" ? "Testing…" : "Test connection"}
          </button>
          <button className="btn small primary" onClick={saveToken} disabled={!!busy || !token.trim()}>
            {busy === "save" ? "Saving…" : "Save"}
          </button>
          {editingToken && (
            <button className="btn small ghost" onClick={() => setEditingToken(false)}>
              Cancel
            </button>
          )}
        </div>
      </div>
    );
  }

  const chosen = choices?.firewalls.find((f) => f.id === status.firewallId);
  const changed = firewallId !== (status.firewallId ?? 0);

  return (
    <div className="linode">
      <dl className="facts">
        <dt>Signed in with</dt>
        <dd>API token (stored encrypted)</dd>
        <dt>Managed firewall</dt>
        <dd>
          {status.firewallId ? (
            <>
              <strong>{status.firewallLabel ?? chosen?.label ?? `#${status.firewallId}`}</strong>
              {chosen?.linodes?.length ? <span className="muted"> on {chosen.linodes.join(", ")}</span> : null}
              {status.inboundPolicy && (
                <span className="muted">
                  {" "}
                  · inbound {status.inboundPolicy === "DROP" ? "blocked unless a rule allows it" : "allowed by default"} ·{" "}
                  {status.otherRules} other rule{status.otherRules === 1 ? "" : "s"}
                </span>
              )}
            </>
          ) : (
            <span className="muted">none chosen yet</span>
          )}
        </dd>
      </dl>

      {status.firewallId ? (
        <div className="server-info">
          <ul>
            {status.firewallState && status.firewallState !== "enabled" && (
              <li className="bad">The firewall is {status.firewallState}</li>
            )}
            <FirewallRulesLine status={status} />
          </ul>
        </div>
      ) : null}

      <div className="field firewall-choice">
        <span className="field-label">Firewall to manage</span>
        {!choices ? (
          <Spinner />
        ) : (
          <>
            <div className="firewall-select">
              <select value={firewallId} onChange={(e) => setFirewallId(Number(e.target.value))}>
                <option value={0}>None</option>
                {choices.firewalls.map((f) => (
                  <option key={f.id} value={f.id}>
                    {f.label}
                    {f.linodes?.length ? ` (on ${f.linodes.join(", ")})` : " (not assigned to a Linode)"}
                    {f.id === choices.suggested ? " · the proxy server's" : ""}
                    {f.status !== "enabled" ? ` [${f.status}]` : ""}
                  </option>
                ))}
              </select>
              <button className="btn small primary" onClick={saveFirewall} disabled={!!busy || !changed}>
                {busy === "firewall" ? "Saving…" : "Use this firewall"}
              </button>
              <button className="btn small ghost" onClick={() => loadChoices(firewallId)} disabled={!!busy}>
                Refresh list
              </button>
            </div>
            {choices.note && <span className="field-help">{choices.note}</span>}
            <span className="field-help">
              hlm adds its own inbound rules (labelled <code>hlm-…</code>) after the firewall&apos;s other rules and never
              changes those or the default policies, so keep a rule allowing SSH. Switching firewalls moves hlm&apos;s rules.
            </span>
          </>
        )}
      </div>

      {messages}
      <div className="btn-row">
        {status.firewallId && !status.inSync && !status.error ? (
          <button className="btn small primary" onClick={sync} disabled={!!busy}>
            {busy === "sync" ? "Updating…" : "Update firewall"}
          </button>
        ) : null}
        <button className="btn small" onClick={() => setEditingToken(true)} disabled={!!busy}>
          Change token
        </button>
        <button className="btn small danger" onClick={() => remove()} disabled={!!busy}>
          {busy === "remove" ? "Removing…" : "Disconnect"}
        </button>
      </div>
    </div>
  );
}
