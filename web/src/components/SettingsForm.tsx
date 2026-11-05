import { type FormEvent, useEffect, useMemo, useState } from "react";
import {
  type Field,
  type Place,
  addSetting,
  flipBool,
  humanize,
  parseSettings,
  placesOf,
  sectionsOf,
  setValue,
} from "../settingsform";

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

function groupName(f: Field | Place): string {
  if (f.group) return f.section ? `${f.group} (${f.section})` : f.group;
  return f.section;
}

const NEW_SECTION = "new";

/** Where a new setting can go: each section (its structs first, then the section itself), including empty ones. */
function placesFor(text: string, fields: Field[]): Place[] {
  const structs = placesOf(fields).filter((p) => p.group !== undefined);
  const headers = sectionsOf(text);
  const sections = fields.some((f) => f.section === "") || headers.length === 0 ? ["", ...headers] : headers;
  const out: Place[] = [];
  for (const section of sections) {
    out.push(...structs.filter((p) => p.section === section));
    out.push({ section });
  }
  return out;
}

function placeLabel(p: Place, hasSections: boolean): string {
  if (p.group) return p.section ? `${p.group}, in [${p.section}]` : p.group;
  if (p.section) return `[${p.section}]`;
  return hasSections ? "Before the first section" : "The file";
}

interface Adding {
  /** An index into the places, or NEW_SECTION. */
  where: string;
  section: string;
  key: string;
  value: string;
  error?: string;
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

  const places = useMemo(() => placesFor(text, fields), [text, fields]);
  const hasSections = places.some((p) => p.section !== "");
  const [adding, setAdding] = useState<Adding | null>(null);
  // The setting just added, to show and focus once it's in the form.
  const [added, setAdded] = useState<{ section: string; group?: string; key: string } | null>(null);
  useEffect(() => {
    if (!added) return;
    const f = [...fields].reverse().find((x) => x.section === added.section && x.group === added.group && x.key === added.key);
    setAdded(null);
    const el = f && document.getElementById(`setting-${f.id}`);
    if (el) {
      el.scrollIntoView({ block: "center" });
      el.focus();
    }
  }, [fields, added]);

  const startAdding = (p?: Place) => {
    const i = p ? places.findIndex((x) => x.section === p.section && x.group === p.group) : 0;
    setAdding({ where: String(Math.max(i, 0)), section: "", key: "", value: "" });
  };

  const add = (e: FormEvent) => {
    e.preventDefault();
    if (!adding) return;
    const place: Place = adding.where === NEW_SECTION ? { section: adding.section.trim() } : places[Number(adding.where)];
    try {
      if (adding.where === NEW_SECTION && !place.section) throw new Error("Name the new section.");
      const next = addSetting(text, place, adding.key, adding.value);
      setFilter("");
      setOnlyChanged(false);
      setAdding(null);
      setAdded({ ...place, key: adding.key.trim() });
      onChange(next);
    } catch (err) {
      setAdding({ ...adding, error: err instanceof Error ? err.message : String(err) });
    }
  };

  const addForm = adding && (
    <form className="settings-add" onSubmit={add} aria-label="Add a setting">
      <label className="field">
        <span className="field-label">Where</span>
        <select value={adding.where} onChange={(e) => setAdding({ ...adding, where: e.target.value, error: undefined })}>
          {places.map((p, i) => (
            <option key={i} value={String(i)}>
              {placeLabel(p, hasSections)}
            </option>
          ))}
          {hasSections && <option value={NEW_SECTION}>A new section…</option>}
        </select>
      </label>
      {adding.where === NEW_SECTION && (
        <label className="field">
          <span className="field-label">Section</span>
          <input
            value={adding.section}
            onChange={(e) => setAdding({ ...adding, section: e.target.value, error: undefined })}
            placeholder="ServerSettings"
            spellCheck={false}
            autoComplete="off"
          />
        </label>
      )}
      <label className="field">
        <span className="field-label">Name</span>
        <input
          value={adding.key}
          onChange={(e) => setAdding({ ...adding, key: e.target.value, error: undefined })}
          placeholder="MaxPlayers"
          spellCheck={false}
          autoComplete="off"
          autoFocus
          required
        />
      </label>
      <label className="field">
        <span className="field-label">Value</span>
        <input
          value={adding.value}
          onChange={(e) => setAdding({ ...adding, value: e.target.value, error: undefined })}
          spellCheck={false}
          autoComplete="off"
        />
      </label>
      <div className="settings-add-buttons">
        <button type="submit" className="btn small primary">
          Add
        </button>
        <button type="button" className="btn small ghost" onClick={() => setAdding(null)}>
          Cancel
        </button>
      </div>
      {adding.error && (
        <p className="form-error small" role="alert">
          {adding.error}
        </p>
      )}
      <p className="muted small settings-add-help">
        It&apos;s written like the settings around it; check the game&apos;s documentation for the exact name. Like
        other edits, it&apos;s saved with Save.
      </p>
    </form>
  );

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
        {!readOnly && (
          <button type="button" className="btn small" onClick={() => (adding ? setAdding(null) : startAdding())}>
            Add a setting
          </button>
        )}
      </div>
      {addForm}
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
          {!readOnly && !q && !onlyChanged && (
            <button type="button" className="link-button small settings-add-here" onClick={() => startAdding(g.fields[0])}>
              Add a setting{g.name ? ` to ${g.name}` : ""}
            </button>
          )}
        </section>
      ))}
    </div>
  );
}
