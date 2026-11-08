import { useCallback, useEffect, useRef, useState, type KeyboardEvent } from "react";

/** Something on a page to find: a card on one of its tabs. */
export interface FindableSection {
  id: string;
  title: string;
  /** More words it's found by. */
  keywords: string;
  /** Where it is, shown beside it (its tab's name). */
  where?: string;
}

/** Sections whose title or keywords match every word typed. */
export function findSections<T extends FindableSection>(sections: T[], query: string): T[] {
  const words = query.toLowerCase().split(/\s+/).filter(Boolean);
  if (words.length === 0) return [];
  return sections.filter((s) => {
    const text = `${s.title} ${s.keywords} ${s.where ?? ""}`.toLowerCase();
    return words.every((w) => text.includes(w));
  });
}

/**
 * "Find a setting": a search box listing the page's sections that match,
 * across its tabs; picking one calls onPick (which shows its tab and
 * scrolls to it). The System page and a server's page both have one.
 */
export function SectionFinder({
  sections,
  onPick,
  placeholder = "Find a setting…",
}: {
  sections: FindableSection[];
  onPick: (id: string) => void;
  placeholder?: string;
}) {
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(0);
  const found = findSections(sections, query).slice(0, 12);
  useEffect(() => setActive(0), [query]);

  function pick(id: string) {
    setQuery("");
    onPick(id);
  }

  function key(e: KeyboardEvent<HTMLInputElement>) {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setActive((a) => Math.min(a + 1, found.length - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActive((a) => Math.max(a - 1, 0));
    } else if (e.key === "Enter" && found[active]) {
      e.preventDefault();
      pick(found[active].id);
    } else if (e.key === "Escape") {
      setQuery("");
    }
  }

  return (
    <div className="system-find">
      <input
        type="search"
        value={query}
        onChange={(e) => setQuery(e.target.value)}
        onKeyDown={key}
        placeholder={placeholder}
        aria-label={placeholder.replace(/…$/, "")}
      />
      {query.trim() && (
        <ul className="system-find-results" role="listbox">
          {found.length === 0 && <li className="muted small">Nothing matches.</li>}
          {found.map((f, i) => (
            <li key={f.id} role="option" aria-selected={i === active} className={i === active ? "active" : ""}>
              <button type="button" className="link-button" onClick={() => pick(f.id)} onMouseEnter={() => setActive(i)}>
                {f.title}
              </button>{" "}
              {f.where && <span className="muted small">{f.where}</span>}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/**
 * Scrolls to an element by its ID once it's on the page: a tab's card may
 * only appear after the tab is shown and its data has loaded. It's
 * highlighted for a moment, so the eye finds it.
 */
export function useScrollToSection(): (id: string) => void {
  const timer = useRef<number>(undefined);
  useEffect(() => () => window.clearInterval(timer.current), []);
  return useCallback((id: string) => {
    window.clearInterval(timer.current);
    const until = Date.now() + 5000;
    const look = () => {
      const el = document.getElementById(id);
      if (el) {
        window.clearInterval(timer.current);
        el.scrollIntoView({ block: "start", behavior: "smooth" });
        el.classList.add("found-flash");
        window.setTimeout(() => el.classList.remove("found-flash"), 1600);
      } else if (Date.now() > until) {
        window.clearInterval(timer.current);
      }
    };
    timer.current = window.setInterval(look, 100);
    look();
  }, []);
}
