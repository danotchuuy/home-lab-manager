# 16 — UI improvements

**Priority:** Low (each item is small; do them opportunistically) · **Depends on:** nothing

> **Status: done** (branch `feature/16-ui-improvements`). See the README's [Using the web UI](../README.md#using-the-web-ui).
> Where the result differs from this plan:
>
> - **Tests:** the web already runs `node --test` on `*.test.ts` (Node strips the types), so the list logic
>   (`src/serverlist.ts`) is tested that way instead of adding Vitest.
> - **Filters:** "crash loop" is part of **With a problem** (also failing health checks and dead containers), and
>   there's a Docker host filter (feature 14). The sort by players uses the game query's counts.
> - **Favourites** are per principal: account, single sign-on user, or API key; one shared set when sign-in isn't
>   required. Tags and favourites are in the secrets store (`server-tags`, `favourites`), so configuration backups
>   include them; a deleted server's are forgotten.
> - **Bulk actions** stream through `POST /api/servers/bulk` (one request) and reuse the multi-server update progress.
>   An update of a server that's already up to date is reported as such, not run.
> - **Tables** don't turn into stacked rows on phones: they scroll sideways (as before); cards, tabs, the header and
>   touch targets were reworked instead.
> - **Connect info** has the addresses and ports but no password hint: hlm doesn't know which setting is a game's
>   join password.
> - **Accessibility:** checked with axe-core (WCAG 2 A/AA) on the Servers, server, Host, System and Catalog pages in
>   both themes. The fixes included a darker light-theme accent and success/warning/danger colours (contrast), labels
>   on selects, `role="img"` on the storage bars, and the activity log's rows opening with a real button.

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

- [x] Theme toggle
- [x] Search/filter/sort + URL state
- [x] Tags (API + UI) and per-user favourites
- [x] Bulk actions (+ optional bulk endpoint)
- [x] Mobile layout pass + PWA manifest
- [x] Keyboard shortcuts + command palette
- [x] Polish items
- [x] Accessibility pass
- [x] Tests for the list logic (with the existing `node --test`, not Vitest)
