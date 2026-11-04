# 16 — UI improvements

**Priority:** Low (each item is small; do them opportunistically) · **Depends on:** nothing

The UI is React + Vite (`web/src`), with styles in `web/src/styles.css`, which already follows the system light/dark
setting through `prefers-color-scheme`.

## 1. Theme toggle

- Header menu: **System / Light / Dark**, saved in `localStorage`.
- Implementation: move the dark palette from the two `@media (prefers-color-scheme: dark)` blocks to
  `:root[data-theme="dark"]` (and keep the media query for `data-theme="system"`), set `data-theme` on `<html>` before
  first paint (inline script in `index.html`) to avoid a flash.
- Code editor and charts follow the same attribute.

## 2. Server list: search, filter, sort, tags, favourites

- Search box (name, app, image, hostname).
- Filters: state (running/stopped/crash loop), update available, group, tag.
- Sort: name, state, CPU, memory, players (05).
- **Tags**: free-text labels per server (stored per server name, Manager can edit), shown as chips.
- **Favourites**: per user, pinned to the top (stored server-side per account so it follows the user).
- Remember filters in the URL query so views can be bookmarked.

## 3. Bulk actions

- Select servers (checkboxes, or "all in group/tag") → Start / Stop / Restart / Update / Back up now.
- Runs through the existing per-server endpoints; one progress panel; each action checked against the user's role per
  server (skip and report the ones not allowed).
- Optional API: `POST /api/servers/bulk {ids, action}` returning per-server results, to keep it one request.

## 4. Mobile layout

- Server list as single-column cards; actions in an overflow menu.
- Server page tabs become a scrollable tab bar; console uses full width with a sticky command box (02).
- Tables (Host, backups, audit) collapse into stacked rows below 640 px.
- Touch targets ≥ 44 px. Test on iOS Safari and Android Chrome.
- PWA manifest (`web/public/manifest.webmanifest`) so it can be added to the home screen; icons already exist.

## 5. Keyboard and navigation

- `/` focuses search; `g s` servers, `g h` host, `g c` catalog; `?` shows shortcuts.
- Command palette (Ctrl/⌘-K): jump to server, run an action on it.

## 6. Small polish

- Relative times with exact time on hover everywhere (backups, last update check).
- Toasts for background job completion (install/update/backup finished) while on another page — the jobs poll already
  exists (`/api/jobs`).
- Empty states with a next step ("No servers yet — browse the catalog").
- Copy buttons for connection info (`host:port`, hostname) on server cards — the most common thing players ask for.
- "Connect info" card per server: address, port, password hint, combined into a shareable text.

## 7. Accessibility

- Audit with axe: labels on icon buttons, focus outlines, colour contrast in both themes, `aria-live` for job progress.

## Testing

- Component tests for filter/sort logic (if a test runner is added to `web/` — currently none; add Vitest).
- Manual check list for mobile breakpoints.

## Tasks

- [ ] Theme toggle
- [ ] Search/filter/sort + URL state
- [ ] Tags (API + UI) and per-user favourites
- [ ] Bulk actions (+ optional bulk endpoint)
- [ ] Mobile layout pass + PWA manifest
- [ ] Keyboard shortcuts + command palette
- [ ] Polish items
- [ ] Accessibility pass
- [ ] Add Vitest to `web/`
