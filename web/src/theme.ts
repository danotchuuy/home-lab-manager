/** The colour theme: the system's setting, or light or dark whatever it says. */
export type Theme = "system" | "light" | "dark";

const KEY = "hlm-theme";

export function getTheme(): Theme {
  try {
    const t = localStorage.getItem(KEY);
    return t === "light" || t === "dark" ? t : "system";
  } catch {
    return "system";
  }
}

/** Saves the theme and applies it (index.html applies it again on the next load, before the first paint). */
export function setTheme(t: Theme) {
  try {
    if (t === "system") localStorage.removeItem(KEY);
    else localStorage.setItem(KEY, t);
  } catch {
    // Not saved; it still applies until the page is reloaded.
  }
  if (t === "system") delete document.documentElement.dataset.theme;
  else document.documentElement.dataset.theme = t;
}
