import { useEffect, useMemo, useState, type FormEvent } from "react";
import { api } from "../api";
import type { App, ConfigItem, ConfigType, DeployRequest, ExtraConfig, PortConflict } from "../types";
import { DockerOptions, ExtraItems } from "./AdditionalSettings";
import { NetworkSection } from "./NetworkSection";
import { suggestName } from "../util";

interface Props {
  app: App;
  /** Existing settings when editing a server; omitted for a new install. */
  initial?: DeployRequest;
  /** initial is the starting point for a new server (an import or a clone), not one being edited. */
  isNew?: boolean;
  submitLabel: string;
  busy?: boolean;
  /** Only ports and variables can be changed: the rest takes an admin. */
  restricted?: boolean;
  /** Nothing can be changed. */
  readOnly?: boolean;
  onSubmit: (req: DeployRequest) => void;
}

const sections: { type: ConfigType; title: string }[] = [
  { type: "Port", title: "Ports" },
  { type: "Path", title: "Paths" },
  { type: "Variable", title: "Variables" },
  { type: "Device", title: "Devices" },
  { type: "Label", title: "Labels" },
];

const NAME_PATTERN = /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$/;

export function ConfigForm({ app, initial, isNew, submitLabel, busy, restricted, readOnly, onSubmit }: Props) {
  const editing = !!initial && !isNew;
  const [name, setName] = useState(initial?.name ?? suggestName(app.name));
  const [tag, setTag] = useState(initial?.tag ?? "");
  const [values, setValues] = useState<Record<string, string>>(() => {
    const v: Record<string, string> = {};
    for (const c of app.config) v[c.id] = initial?.values?.[c.id] ?? c.value;
    return v;
  });
  const [disabled, setDisabled] = useState<Set<string>>(() => new Set(initial?.disabled ?? []));
  const [showAdvanced, setShowAdvanced] = useState(false);
  const [network, setNetwork] = useState(initial?.network ?? "");
  const [ipAddress, setIPAddress] = useState(initial?.ipAddress ?? "");
  const [extra, setExtra] = useState<ExtraConfig[]>(initial?.extra ?? []);
  const [extraParams, setExtraParams] = useState(initial?.extraParams ?? app.extraParams ?? "");
  const [postArgs, setPostArgs] = useState(initial?.postArgs ?? app.postArgs ?? "");
  const [privileged, setPrivileged] = useState(initial?.privileged ?? app.privileged);

  const advancedCount = app.config.filter((c) => c.display === "advanced").length;

  const missing = useMemo(
    () =>
      app.config.filter((c) => c.required && !disabled.has(c.id) && !(values[c.id] ?? "").trim()).map((c) => c.name),
    [app.config, values, disabled],
  );
  const nameValid = NAME_PATTERN.test(name);

  const request = (): DeployRequest => ({
    templateId: app.id,
    name,
    tag: tag || undefined,
    values,
    disabled: [...disabled],
    network: network || undefined,
    ipAddress: ipAddress.trim() || undefined,
    extra: extra.length > 0 ? extra : undefined,
    // Only store overrides, so template updates still reach servers that
    // didn't change these.
    extraParams: extraParams !== (app.extraParams ?? "") ? extraParams : undefined,
    postArgs: postArgs !== (app.postArgs ?? "") ? postArgs : undefined,
    privileged: privileged !== app.privileged ? privileged : undefined,
    // Kept as it was: start on boot is switched on the server list.
    restartPolicy: initial?.restartPolicy || undefined,
  });

  // Host ports already used by another container or program, checked a
  // moment after the settings change. Saving would be refused anyway.
  const [conflicts, setConflicts] = useState<PortConflict[]>([]);
  const checkKey = JSON.stringify(request());
  useEffect(() => {
    if (readOnly) return;
    let live = true;
    const t = setTimeout(() => {
      api.checkPorts(JSON.parse(checkKey), editing ? initial?.name : undefined).then(
        (r) => live && setConflicts(r.conflicts ?? []),
        () => live && setConflicts([]),
      );
    }, 400);
    return () => {
      live = false;
      clearTimeout(t);
    };
  }, [checkKey, editing, initial?.name, readOnly]);
  const portItems = app.config.filter((c) => c.type === "Port" && !disabled.has(c.id));
  const conflictsFor = (item: ConfigItem) => conflicts.filter((c) => portItemMaps(item, c.container));
  // Conflicts from ports not shown as a field, e.g. extra ports.
  const otherConflicts = conflicts.filter((c) => !portItems.some((i) => portItemMaps(i, c.container)));

  function submit(e: FormEvent) {
    e.preventDefault();
    onSubmit(request());
  }

  function toggle(id: string, enabled: boolean) {
    setDisabled((prev) => {
      const next = new Set(prev);
      if (enabled) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  return (
    <form className="config-form" onSubmit={submit}>
      <fieldset className="plain-fieldset" disabled={readOnly}>
      <div className="field-grid">
        <label className="field">
          <span className="field-label">Server name</span>
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            disabled={editing}
            aria-invalid={!nameValid}
            spellCheck={false}
          />
          <span className="field-help">
            {editing
              ? "The container name can't be changed."
              : "Used as the Docker container name: letters, digits, '_', '.' and '-'."}
          </span>
        </label>

        {app.branches && app.branches.length > 0 && (
          <label className="field">
            <span className="field-label">Version</span>
            <select value={tag} onChange={(e) => setTag(e.target.value)}>
              <option value="">{app.defaultTagDescription || "Default"} ({app.repository})</option>
              {app.branches.map((b) => (
                <option key={b.tag} value={b.tag}>
                  {b.description || b.tag} ({b.repository || b.tag})
                </option>
              ))}
            </select>
          </label>
        )}
      </div>

      {restricted ? (
        <p className="muted small">
          {readOnly
            ? "You can see these settings; changing them takes a manager."
            : "You can change ports and variables. Paths, devices, the network and Docker options take an admin."}
        </p>
      ) : (
        <NetworkSection
          serverName={editing ? initial?.name : undefined}
          templateNetwork={app.network}
          network={network}
          ipAddress={ipAddress}
          onNetwork={setNetwork}
          onIPAddress={setIPAddress}
        />
      )}

      {sections.map(({ type, title }) => {
        const items = app.config.filter((c) => c.type === type && (showAdvanced || c.display !== "advanced"));
        if (items.length === 0) return null;
        return (
          <fieldset key={type} className="config-section" disabled={restricted && type !== "Port" && type !== "Variable"}>
            <legend>{title}</legend>
            <div className="field-grid">
              {items.map((c) => (
                <ConfigField
                  key={c.id}
                  item={c}
                  value={values[c.id] ?? ""}
                  enabled={!disabled.has(c.id)}
                  conflicts={c.type === "Port" && !disabled.has(c.id) ? conflictsFor(c) : []}
                  onChange={(v) => setValues((prev) => ({ ...prev, [c.id]: v }))}
                  onToggle={(on) => toggle(c.id, on)}
                  lockToggle={restricted}
                />
              ))}
            </div>
          </fieldset>
        );
      })}

      {!restricted && <ExtraItems items={extra} onChange={setExtra} />}
      {otherConflicts.map((c) => (
        <div key={`${c.port}/${c.proto}/${c.usedBy ?? ""}`} className="alert error">
          {c.message}
        </div>
      ))}

      {!restricted && (
        <DockerOptions
          extraParams={extraParams}
          postArgs={postArgs}
          privileged={privileged}
          template={{ extraParams: app.extraParams, postArgs: app.postArgs, privileged: app.privileged }}
          onExtraParams={setExtraParams}
          onPostArgs={setPostArgs}
          onPrivileged={setPrivileged}
        />
      )}
      </fieldset>

      <div className="form-footer">
        {advancedCount > 0 && (
          <button type="button" className="btn ghost" onClick={() => setShowAdvanced((s) => !s)}>
            {showAdvanced ? "Hide" : "Show"} advanced settings ({advancedCount})
          </button>
        )}
        <div className="spacer" />
        {missing.length > 0 && <span className="form-error">Required: {missing.join(", ")}</span>}
        {conflicts.length > 0 && (
          <span className="form-error">
            {conflicts.length === 1 ? "A host port is" : `${conflicts.length} host ports are`} already in use; choose others.
          </span>
        )}
        {!readOnly && (
          <button type="submit" className="btn primary" disabled={busy || !nameValid || missing.length > 0 || conflicts.length > 0}>
            {submitLabel}
          </button>
        )}
      </div>
    </form>
  );
}

/** Whether a port field maps a container port ("2456/udp"), which may be inside its range. */
function portItemMaps(item: ConfigItem, container: string): boolean {
  const [n, proto = "tcp"] = container.split("/");
  const mode = (item.mode || "tcp").toLowerCase();
  if (!mode.includes(proto)) return false;
  const m = /^\s*(\d+)\s*(?:[-:]\s*(\d+))?\s*$/.exec(item.target);
  if (!m) return false;
  const port = Number(n);
  return port >= Number(m[1]) && port <= Number(m[2] ?? m[1]);
}

interface FieldProps {
  item: ConfigItem;
  value: string;
  enabled: boolean;
  /** Host ports this port field would use that are already taken. */
  conflicts: PortConflict[];
  onChange: (v: string) => void;
  onToggle: (enabled: boolean) => void;
  /** Switching the item on or off isn't allowed. */
  lockToggle?: boolean;
}

function ConfigField({ item, value, enabled, conflicts, onChange, onToggle, lockToggle }: FieldProps) {
  const [reveal, setReveal] = useState(false);
  // Ports, paths and devices can be switched off when optional; an empty
  // variable is already "off", so they don't get a checkbox.
  const canDisable = !item.required && item.type !== "Variable" && item.type !== "Label";

  let detail = "";
  // A container port range like "2456-2458" (some templates write "2456:2458").
  const portRange = item.type === "Port" ? /^\s*(\d+)\s*[-:]\s*(\d+)\s*$/.exec(item.target) : null;
  const rangeSize = portRange ? Number(portRange[2]) - Number(portRange[1]) + 1 : 1;
  if (item.type === "Port") detail = `container ${item.target}/${item.mode}`;
  else if (item.type === "Path") detail = `→ ${item.target}${item.mode && item.mode !== "rw" ? ` (${item.mode})` : ""}`;
  else if (item.type === "Variable") detail = item.target;

  let input;
  if (item.options && item.options.length > 0) {
    input = (
      <select value={value} onChange={(e) => onChange(e.target.value)} disabled={!enabled}>
        {item.options.map((o) => (
          <option key={o} value={o}>
            {o}
          </option>
        ))}
      </select>
    );
  } else {
    input = (
      <div className="input-row">
        <input
          type={item.mask && !reveal ? "password" : "text"}
          inputMode={item.type === "Port" && !portRange ? "numeric" : undefined}
          value={value}
          placeholder={item.default}
          onChange={(e) => onChange(e.target.value)}
          disabled={!enabled}
          aria-invalid={conflicts.length > 0 || undefined}
          spellCheck={false}
          autoComplete={item.mask ? "new-password" : "off"}
        />
        {item.mask && (
          <button type="button" className="btn ghost small" onClick={() => setReveal((r) => !r)}>
            {reveal ? "Hide" : "Show"}
          </button>
        )}
      </div>
    );
  }

  return (
    <div className={`field${enabled ? "" : " field-disabled"}`}>
      <span className="field-label">
        {canDisable && (
          <input
            type="checkbox"
            checked={enabled}
            onChange={(e) => onToggle(e.target.checked)}
            disabled={lockToggle}
            title={enabled ? "Disable" : "Enable"}
          />
        )}
        {item.name}
        {item.required && <span className="required">*</span>}
        {detail && <code className="field-target">{detail}</code>}
      </span>
      {input}
      {portRange && (
        <span className="field-help">
          Range of {rangeSize} ports. Enter a host range of the same size, or just its first port.
        </span>
      )}
      {conflicts.map((c) => (
        <span key={`${c.port}/${c.proto}/${c.usedBy ?? ""}`} className="form-error small">
          {c.message}
        </span>
      ))}
      {item.description && <span className="field-help">{item.description}</span>}
    </div>
  );
}
