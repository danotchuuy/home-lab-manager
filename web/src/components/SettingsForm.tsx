import { useMemo, useState } from "react";
import { type Field, flipBool, humanize, parseSettings, setValue } from "../settingsform";

const NUMBER = /^[-+]?(\d+\.?\d*|\.\d+)$/;

/** Known values of some settings, offered as suggestions (anything can still be typed). */
const OPTIONS: Record<string, string[]> = {
  DeathPenalty: ["None", "Item", "ItemAndEquipment", "All"],
  Difficulty: ["None", "Casual", "Normal", "Hard"],
  RandomizerType: ["None", "Region", "All"],
  LogFormatType: ["Text", "Json"],
  gamemode: ["survival", "creative", "adventure", "spectator"],
  difficulty: ["peaceful", "easy", "normal", "hard"],
  "level-type": ["minecraft:normal", "minecraft:flat", "minecraft:large_biomes", "minecraft:amplified"],
};

function groupName(f: Field): string {
  if (f.group) return f.section ? `${f.group} (${f.section})` : f.group;
  return f.section;
}

/**
 * Edits a key = value settings file as a form: a field per setting, each
 * change written straight into the text (only that value changes), so
 * saving, reverting and conflicts work as in the text editor.
 */
export function SettingsForm({
  text,
  original,
  readOnly,
  onChange,
}: {
  text: string;
  original: string;
  readOnly: boolean;
  onChange: (text: string) => void;
}) {
  const [filter, setFilter] = useState("");
  const [onlyChanged, setOnlyChanged] = useState(false);
  const [shown, setShown] = useState<Record<string, boolean>>({});
  const fields = useMemo(() => parseSettings(text) ?? [], [text]);
  // The values and kinds as the file was opened: a number setting stays one
  // while it's being typed, even when it briefly isn't a number.
  const opened = useMemo(() => parseSettings(original) ?? [], [original]);
  const before = useMemo(() => new Map(opened.map((f) => [f.id, f.value])), [opened]);
  const kinds = useMemo(() => new Map(opened.map((f) => [f.id, f.kind])), [opened]);
  const changed = fields.filter((f) => before.get(f.id) !== f.value).length;

  const q = filter.trim().toLowerCase();
  const visible = fields.filter(
    (f) =>
      (!q || f.key.toLowerCase().includes(q) || humanize(f.key).toLowerCase().includes(q) || f.value.toLowerCase().includes(q)) &&
      (!onlyChanged || before.get(f.id) !== f.value),
  );
  const groups: { name: string; fields: Field[] }[] = [];
  for (const f of visible) {
    const name = groupName(f);
    const g = groups[groups.length - 1];
    if (g && g.name === name) g.fields.push(f);
    else groups.push({ name, fields: [f] });
  }

  const set = (f: Field, value: string) => onChange(setValue(text, f, value));

  return (
    <div className="settings-form">
      <div className="settings-form-bar">
        <input
          type="search"
          placeholder={`Filter ${fields.length} settings`}
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          aria-label="Filter settings"
        />
        <label className="radio small">
          <input type="checkbox" checked={onlyChanged} onChange={(e) => setOnlyChanged(e.target.checked)} disabled={!changed && !onlyChanged} />
          Only changed{changed ? ` (${changed})` : ""}
        </label>
      </div>
      {visible.length === 0 && <p className="muted small">No settings match.</p>}
      {groups.map((g, gi) => (
        <section key={gi} className="settings-group">
          {g.name && <h3>{g.name}</h3>}
          <div className="settings-grid">
            {g.fields.map((f) => {
              const was = before.get(f.id);
              const isChanged = was !== undefined && was !== f.value;
              const isNew = was === undefined;
              const label = humanize(f.key);
              const inputId = `setting-${f.id}`;
              const options = OPTIONS[f.key];
              const kind = kinds.get(f.id) ?? f.kind;
              const notNumber = kind === "number" && !NUMBER.test(f.value);
              return (
                <div key={f.id} className={`setting-row${isChanged ? " changed" : ""}`}>
                  <label htmlFor={inputId} className="setting-label">
                    <span>{label}</span>
                    <code className="muted small">{f.key}</code>
                  </label>
                  <div className="setting-input">
                    {kind === "bool" && f.kind === "bool" ? (
                      <label className="radio small">
                        <input
                          id={inputId}
                          type="checkbox"
                          checked={f.value.toLowerCase() === "true"}
                          onChange={() => set(f, flipBool(f.value))}
                          disabled={readOnly}
                        />
                        {f.value}
                      </label>
                    ) : (
                      <span className="inline-input">
                        <input
                          id={inputId}
                          type={f.secret && !shown[f.id] ? "password" : "text"}
                          inputMode={kind === "number" ? "decimal" : undefined}
                          className={notNumber ? "invalid" : undefined}
                          aria-invalid={notNumber || undefined}
                          value={f.value}
                          onChange={(e) => set(f, e.target.value)}
                          readOnly={readOnly}
                          list={options ? `${inputId}-options` : undefined}
                          spellCheck={false}
                          autoComplete="off"
                        />
                        {options && (
                          <datalist id={`${inputId}-options`}>
                            {options.map((o) => (
                              <option key={o} value={o} />
                            ))}
                          </datalist>
                        )}
                        {f.secret && (
                          <button
                            type="button"
                            className="btn small ghost"
                            onClick={() => setShown((s) => ({ ...s, [f.id]: !s[f.id] }))}
                          >
                            {shown[f.id] ? "Hide" : "Show"}
                          </button>
                        )}
                      </span>
                    )}
                    {notNumber && <span className="form-error small">This setting is a number.</span>}
                    {isChanged && (
                      <span className="muted small setting-was">
                        was {f.secret && !shown[f.id] ? "••••" : was === "" ? "empty" : was}{" "}
                        {!readOnly && (
                          <button type="button" className="link-button" onClick={() => set(f, was)}>
                            undo
                          </button>
                        )}
                      </span>
                    )}
                    {isNew && <span className="muted small setting-was">new</span>}
                  </div>
                </div>
              );
            })}
          </div>
        </section>
      ))}
    </div>
  );
}
