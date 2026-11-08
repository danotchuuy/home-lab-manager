import { useEffect, useId, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from "react";

export interface MultiOption {
  value: string;
  label: string;
  /** A few words beside it, such as a server's app. */
  hint?: string;
  /** Chosen some other way (a server through its host's group, say): shown chosen, and can't be removed here. */
  fixed?: string;
}

/**
 * Picking several of a list (servers, groups, hosts): the chosen ones as
 * chips, and a box to type in that lists the rest to add. Easier than a
 * long column of checkboxes, and than a <select multiple> with its
 * Ctrl-clicks.
 */
export function MultiSelect({
  options,
  value,
  onChange,
  placeholder = "Add…",
  empty,
  disabled,
  label,
  chipAction,
}: {
  options: MultiOption[];
  value: string[];
  onChange: (value: string[]) => void;
  placeholder?: string;
  /** What choosing none means, shown when nothing is chosen ("every server", say). */
  empty?: ReactNode;
  disabled?: boolean;
  /** For screen readers. */
  label: string;
  /** A small button on each chosen chip (opening a server's ports, say). */
  chipAction?: { label: string; title?: (o: MultiOption) => string; onClick: (o: MultiOption) => void };
}) {
  const id = useId();
  const [query, setQuery] = useState("");
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const box = useRef<HTMLDivElement>(null);
  const input = useRef<HTMLInputElement>(null);

  const byValue = useMemo(() => new Map(options.map((o) => [o.value, o])), [options]);
  // Chosen: the fixed ones, then the rest in the order chosen.
  const fixed = options.filter((o) => o.fixed);
  const picked = value.filter((v) => !byValue.get(v)?.fixed);
  const isChosen = (v: string) => value.includes(v) || !!byValue.get(v)?.fixed;
  const words = query.toLowerCase().split(/\s+/).filter(Boolean);
  const shown = options.filter((o) => {
    const text = `${o.label} ${o.hint ?? ""}`.toLowerCase();
    return words.every((w) => text.includes(w));
  });

  // Closes when the focus or a click goes elsewhere.
  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent) => {
      if (box.current && !box.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", close);
    return () => document.removeEventListener("mousedown", close);
  }, [open]);
  useEffect(() => setActive(0), [query]);

  function toggle(o: MultiOption) {
    if (o.fixed) return;
    onChange(value.includes(o.value) ? value.filter((v) => v !== o.value) : [...value, o.value]);
  }

  function key(e: KeyboardEvent<HTMLInputElement>) {
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        setOpen(true);
        setActive((a) => Math.min(a + 1, shown.length - 1));
        break;
      case "ArrowUp":
        e.preventDefault();
        setActive((a) => Math.max(a - 1, 0));
        break;
      case "Enter":
        // Never submits the form it's in.
        e.preventDefault();
        if (open && shown[active]) {
          toggle(shown[active]);
          setQuery("");
        } else setOpen(true);
        break;
      case "Escape":
        if (open) {
          e.preventDefault();
          e.stopPropagation();
          setOpen(false);
        }
        break;
      case "Backspace":
        if (!query && picked.length) onChange(value.filter((v) => v !== picked[picked.length - 1]));
        break;
    }
  }

  const chip = (o: MultiOption, removable: boolean) => (
    <span key={o.value} className={`multi-chip${o.fixed ? " fixed" : ""}`} title={o.fixed || o.hint}>
      {o.label}
      {chipAction && (
        <button
          type="button"
          className="multi-chip-action"
          onClick={() => chipAction.onClick(o)}
          title={chipAction.title?.(o)}
          disabled={disabled}
        >
          {chipAction.label}
        </button>
      )}
      {removable && (
        <button type="button" className="multi-chip-remove" aria-label={`Remove ${o.label}`} onClick={() => toggle(o)} disabled={disabled}>
          ×
        </button>
      )}
    </span>
  );

  const listID = `${id}-list`;
  return (
    <div className={`multi-select${disabled ? " disabled" : ""}`} ref={box}>
      <div className="multi-field" onClick={() => !disabled && input.current?.focus()}>
        {fixed.map((o) => chip(o, false))}
        {picked.map((v) => {
          const o = byValue.get(v) ?? { value: v, label: v, hint: "no longer there" };
          return chip(o, true);
        })}
        {value.length === 0 && fixed.length === 0 && empty && !query && <span className="multi-empty">{empty}</span>}
        <input
          ref={input}
          role="combobox"
          aria-label={label}
          aria-expanded={open}
          aria-controls={listID}
          aria-autocomplete="list"
          aria-activedescendant={open && shown[active] ? `${id}-${active}` : undefined}
          value={query}
          placeholder={placeholder}
          disabled={disabled}
          onChange={(e) => {
            setQuery(e.target.value);
            setOpen(true);
          }}
          onFocus={() => setOpen(true)}
          onKeyDown={key}
        />
      </div>
      {open && !disabled && (
        <div className="multi-menu">
          <div className="multi-menu-bar small">
            <button type="button" className="link-button" onClick={() => onChange([...new Set([...value, ...shown.filter((o) => !o.fixed).map((o) => o.value)])])}>
              {words.length ? "All of these" : "All"}
            </button>
            <button type="button" className="link-button" onClick={() => onChange(value.filter((v) => !shown.some((o) => o.value === v)))}>
              None
            </button>
          </div>
          <ul id={listID} role="listbox" aria-multiselectable="true" aria-label={label}>
            {shown.length === 0 && <li className="muted small multi-none">Nothing matches.</li>}
            {shown.map((o, i) => (
              <li
                key={o.value}
                id={`${id}-${i}`}
                role="option"
                aria-selected={isChosen(o.value)}
                aria-disabled={!!o.fixed}
                className={`${i === active ? "active" : ""}${o.fixed ? " fixed" : ""}`}
                onMouseEnter={() => setActive(i)}
                // Keeps the focus in the box, so the menu stays open.
                onMouseDown={(e) => e.preventDefault()}
                onClick={() => toggle(o)}
                title={o.fixed}
              >
                <span className="multi-check" aria-hidden="true">
                  {isChosen(o.value) ? "✓" : ""}
                </span>
                {o.label}
                {o.hint && <span className="muted small"> {o.hint}</span>}
                {o.fixed && <span className="muted small"> ({o.fixed})</span>}
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  );
}
