import type { ExtraConfig, ExtraConfigType } from "../types";

interface TypeInfo {
  target?: { label: string; placeholder: string };
  value: { label: string; placeholder: string };
  modes?: string[];
}

// Field labels for each kind of extra item, following Unraid's editor.
const TYPES: Record<ExtraConfigType, TypeInfo> = {
  Path: {
    target: { label: "Container path", placeholder: "/data/mods" },
    value: { label: "Host path", placeholder: "/srv/hlm/appdata/mods" },
    modes: ["rw", "ro", "rw,slave", "rw,shared"],
  },
  Port: {
    target: { label: "Container port", placeholder: "27015 or 27015-27020" },
    value: { label: "Host port", placeholder: "27015" },
    modes: ["tcp", "udp"],
  },
  Variable: {
    target: { label: "Key", placeholder: "TZ" },
    value: { label: "Value", placeholder: "Europe/Oslo" },
  },
  Label: {
    target: { label: "Key", placeholder: "com.example.owner" },
    value: { label: "Value", placeholder: "" },
  },
  Device: {
    value: { label: "Host device", placeholder: "/dev/dri" },
  },
};

const MODE_LABELS: Record<string, string> = {
  rw: "Read/Write",
  ro: "Read only",
  "rw,slave": "Read/Write - Slave",
  "rw,shared": "Read/Write - Shared",
  tcp: "TCP",
  udp: "UDP",
};

/** User-added paths, ports, variables, labels and devices. */
export function ExtraItems({ items, onChange }: { items: ExtraConfig[]; onChange: (items: ExtraConfig[]) => void }) {
  function update(i: number, patch: Partial<ExtraConfig>) {
    onChange(items.map((item, j) => (j === i ? { ...item, ...patch } : item)));
  }

  function setType(i: number, type: ExtraConfigType) {
    // Reset the mode, which means something different for each type.
    update(i, { type, mode: TYPES[type].modes?.[0] });
  }

  return (
    <fieldset className="config-section">
      <legend>Additional settings</legend>
      {items.length === 0 && (
        <p className="muted small no-margin">Add paths, ports, variables, labels or devices the template doesn't include.</p>
      )}
      <div className="extra-list">
        {items.map((item, i) => {
          const info = TYPES[item.type];
          return (
            <div key={i} className="extra-row">
              <label className="field">
                <span className="field-label">Type</span>
                <select value={item.type} onChange={(e) => setType(i, e.target.value as ExtraConfigType)}>
                  {Object.keys(TYPES).map((t) => (
                    <option key={t}>{t}</option>
                  ))}
                </select>
              </label>
              <label className="field">
                <span className="field-label">Name</span>
                <input
                  value={item.name ?? ""}
                  placeholder="Optional"
                  onChange={(e) => update(i, { name: e.target.value })}
                />
              </label>
              {info.target ? (
                <label className="field">
                  <span className="field-label">{info.target.label}</span>
                  <input
                    value={item.target}
                    placeholder={info.target.placeholder}
                    onChange={(e) => update(i, { target: e.target.value })}
                    spellCheck={false}
                  />
                </label>
              ) : (
                <div className="extra-empty" />
              )}
              <label className="field">
                <span className="field-label">{info.value.label}</span>
                <input
                  value={item.value}
                  placeholder={info.value.placeholder}
                  onChange={(e) => update(i, { value: e.target.value })}
                  spellCheck={false}
                />
              </label>
              {info.modes ? (
                <label className="field">
                  <span className="field-label">{item.type === "Port" ? "Protocol" : "Access"}</span>
                  <select value={item.mode ?? info.modes[0]} onChange={(e) => update(i, { mode: e.target.value })}>
                    {info.modes.map((m) => (
                      <option key={m} value={m}>
                        {MODE_LABELS[m] ?? m}
                      </option>
                    ))}
                  </select>
                </label>
              ) : (
                <div className="extra-empty" />
              )}
              <button
                type="button"
                className="btn ghost small danger extra-remove"
                onClick={() => onChange(items.filter((_, j) => j !== i))}
                aria-label="Remove"
              >
                Remove
              </button>
            </div>
          );
        })}
      </div>
      <button
        type="button"
        className="btn small"
        onClick={() => onChange([...items, { type: "Path", target: "", value: "", mode: "rw" }])}
      >
        + Add another Path, Port, Variable, Label or Device
      </button>
    </fieldset>
  );
}

interface DockerOptionsProps {
  extraParams: string;
  postArgs: string;
  privileged: boolean;
  template: { extraParams?: string; postArgs?: string; privileged: boolean };
  onExtraParams: (v: string) => void;
  onPostArgs: (v: string) => void;
  onPrivileged: (v: boolean) => void;
}

/** Raw `docker run` flags, the container command, and privileged mode. */
export function DockerOptions(props: DockerOptionsProps) {
  const { extraParams, postArgs, privileged, template } = props;
  const reset = (current: string, original: string | undefined, set: (v: string) => void) =>
    current !== (original ?? "") && (
      <button type="button" className="link-button" onClick={() => set(original ?? "")}>
        Reset to template
      </button>
    );

  return (
    <fieldset className="config-section">
      <legend>Docker options</legend>
      <div className="field-grid wide">
        <label className="field">
          <span className="field-label">
            Extra parameters {reset(extraParams, template.extraParams, props.onExtraParams)}
          </span>
          <input
            value={extraParams}
            onChange={(e) => props.onExtraParams(e.target.value)}
            placeholder="--memory=8g --cpus=4 -e TZ=Europe/Oslo -v /srv/backups:/backups"
            spellCheck={false}
          />
          <span className="field-help">
            Extra <code>docker run</code> flags, such as <code>-e</code>, <code>-v</code>, <code>-p</code>,{" "}
            <code>--memory</code>, <code>--cpus</code>, <code>--ulimit</code>, <code>--cap-add</code>,{" "}
            <code>--device</code>, <code>--gpus</code>, <code>--restart</code>. Unsupported flags are skipped with a
            warning.
          </span>
        </label>
        <label className="field">
          <span className="field-label">
            Post arguments {reset(postArgs, template.postArgs, props.onPostArgs)}
          </span>
          <input
            value={postArgs}
            onChange={(e) => props.onPostArgs(e.target.value)}
            placeholder="Arguments passed to the container's command"
            spellCheck={false}
          />
          <span className="field-help">Appended after the image name, like the command in <code>docker run</code>.</span>
        </label>
        <label className="field checkbox-field">
          <span className="field-label">
            <input type="checkbox" checked={privileged} onChange={(e) => props.onPrivileged(e.target.checked)} />
            Privileged
            {privileged !== template.privileged && <span className="muted small">(template: {String(template.privileged)})</span>}
          </span>
          <span className="field-help">Gives the container full access to the host. Only enable it if the server needs it.</span>
        </label>
      </div>
    </fieldset>
  );
}
