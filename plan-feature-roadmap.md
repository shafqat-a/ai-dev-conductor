# Feature Roadmap — Reliability, Security & UX

**Status:** Approved — execution staged. No feature code written yet.
**Date:** 2026-05-29.
**Scope:** 10 approved features (#1, 3, 5, 7, 9, 11, 13, 14, 16, 20 from the
suggestion list). Sibling plan: `plan-feature-multiuser.md` (passkeys + accounts) —
several items here share its store/auth layer; coordination notes inline.

### Decisions locked (2026-05-29)

| Decision | Choice |
|----------|--------|
| Sequence | **Roadmap first**, retrofit multiuser/users later. |
| First deliverable | **#7 login brute-force protection** (M3a). |
| #1 survival approach | **tmux-backed** (option A); its own sub-plan before coding M2a. |
| Delivery workflow | **Feature branch + PR per milestone.** First branch: `feat/login-throttling`. |

---

## 0. The features and how they cluster

| # | Feature | Layer | Depends on |
|---|---------|-------|------------|
| 5 | Persistent session metadata | backend store | — (foundation) |
| 1 | Session survival across restart | backend (PTY model) | 5 |
| 3 | Idle / orphan session reaping | backend | 5 |
| 7 | Login brute-force protection | backend (auth) | — |
| 9 | Read-only share links | backend + frontend | 5, store |
| 16 | Activity indicators + bell | backend + frontend | 5 |
| 11 | Command palette + shortcuts | frontend | — |
| 14 | Terminal theming & fonts | frontend | — |
| 20 | Mobile-friendly terminal UX | frontend | — |
| 13 | File upload/download to CWD | backend + frontend | 1 (CWD) |

Five milestones, ordered by dependency and risk:

- **M1 — Persistence foundation** (#5) — unblocks 1, 3, 9, 16.
- **M2 — Session lifecycle** (#1 survival, #3 reaping) — the heavy backend work.
- **M3 — Security** (#7 throttling, #9 share links) — pressing now that it's internet-facing.
- **M4 — Frontend UX batch** (#11, #14, #16, #20) — shared frontend touch points.
- **M5 — File transfer** (#13) — needs M2's CWD resolution.

---

## 1. Architecture decisions

### 1.1 Storage (shared with the multiuser plan)
Today there is **no persistence** beyond per-session history files in `DataDir`.
Three features here need durable state (#5 metadata, #9 share tokens, #7 rate-limit
counters, plus audit). **Reuse the `modernc.org/sqlite` store** proposed in
`plan-feature-multiuser.md` (`<DataDir>/state.db`, pure-Go, no cgo). If the
multiuser work hasn't landed, M1 introduces the store and the multiuser plan
extends it. Avoid a second persistence mechanism.

Rate-limit counters can stay **in-memory** (they're ephemeral); everything else is SQLite.

### 1.2 Session survival (#1) — the load-bearing decision
The current `session.NewSession` spawns a PTY **owned by the server process**, so a
restart kills every shell. Three ways to make sessions outlive the process:

- **(A) tmux-backed (recommended).** Shells run inside a detached tmux server; ai-dev-
  conductor becomes a tmux **control-mode** (`tmux -CC`) client. On boot it reattaches
  via `tmux list-sessions`. Proven (this is exactly what `../terminal-hub` does) and
  gives free scrollback, resize, and naming. **Cost:** real refactor of
  `session.Manager`/`Session`; `WriteInput`/`Resize`/`readPTY`/`PasteImage` all route
  through tmux instead of a raw fd; adds a `tmux` runtime dependency.
- **(B) Standalone PTY supervisor daemon.** A small long-lived process owns the
  `HashMap<id, pty>` and speaks a unix-socket protocol; the web server is a client.
  Full control, no tmux dep, but it's a from-scratch reimplementation of what tmux
  already does well.
- **(C) fd hand-off on graceful restart** (`SCM_RIGHTS`). Survives *planned* restarts
  only, not crashes. Fragile. Rejected.

**Recommendation: (A) tmux.** It is the smallest path to *crash-and-restart* survival
and aligns with terminal-hub, easing a future merge of the two projects. Because it is
the largest single item, **M2 may warrant its own detailed sub-plan** before coding.

### 1.3 CWD resolution (#13)
Per-session working directory comes from `readlink /proc/<pid>/cwd` (Linux) — needs the
child PID, which `Session` already has via `waitProcess`. Document the Linux-only caveat.

### 1.4 Reverse-proxy reality
Already behind nginx/TLS at `home.cloudlabs.live`. Upload limits (#13) must be raised in
**both** nginx (`client_max_body_size`) and the Go handler. Share links (#9) must be
absolute `https://home.cloudlabs.live/...` URLs (derive from `AI_CONDUCTOR_PUBLIC_URL`).

---

## 2. Milestone M1 — Persistent session metadata (#5)

**Goal:** session list (id, name, created_at, last_activity_at, last_client_at, rows/cols)
survives restart and is the source of truth for the sidebar.

- **Tables** (`sessions_meta`): `id PK, name, created_at, last_activity_at,
  last_client_disconnect_at, cols, rows, status`. (Distinct from the auth `sessions`
  cookie table.)
- `session.Manager`: on `Create`/`Rename`/`Delete` write-through to the store; on
  `NewManager` load existing rows. `List()` reads merged live+stored state.
- Track `last_activity_at` (any PTY output) and `last_client_disconnect_at` (in
  `RemoveClient` when client count hits 0) — these feed M2 #3 and M4 #16.
- **Tests:** metadata survives a `Manager` reopen; rename/delete propagate; timestamps update.

**Note:** with M2 option (A), tmux holds the live sessions and this table becomes the
*annotation* layer (names, activity) over `tmux list-sessions`. Design the store API so it
works either way.

---

## 3. Milestone M2 — Session lifecycle

### M2a — Survival across restart (#1) — *largest item; consider a dedicated sub-plan*
- Decide A vs B (§1.2; recommend A).
- For (A): add a `tmux-client` layer (control-mode decoder/driver), refactor `Session`
  to wrap a tmux pane, reattach on boot, map paste/image (`Ctrl+V` to pane or
  `tmux load-buffer` + `paste-buffer`), resize via `tmux resize-window`, history via
  `capture-pane -p` on reattach (replaces the current ring buffer / `ReadHistory`).
- **Tests:** create session → kill+restart server → session still listed and attachable
  with scrollback; integration test gated on `tmux` present (skip otherwise).

### M2b — Idle / orphan reaping (#3)
- Config: `AI_CONDUCTOR_IDLE_TIMEOUT` (default off), `AI_CONDUCTOR_MAX_SESSIONS`.
- Background sweeper (like the existing `SessionStore.cleanup` ticker): close sessions
  whose `last_client_disconnect_at` exceeds the timeout. Push a "closing in N min"
  warning to any reconnecting client first (new WS control message).
- Audit each auto-close.
- **Tests:** session with no clients past timeout is reaped; active session is never reaped.

---

## 4. Milestone M3 — Security

### M3a — Login brute-force protection (#7) — *quick, high priority*
- In-memory per-IP (and per-username once multiuser lands) sliding-window limiter in
  front of `HandleLogin`: e.g. 5 failures → exponential backoff/lockout, 429 with
  `Retry-After`. Use `X-Forwarded-For` (trusting our nginx).
- Structured failure logs (fail2ban-friendly) + audit entries.
- Constant-time-ish responses to avoid timing oracle.
- **Tests:** N failures → 429; success resets counter; lockout expires.

### M3b — Read-only share links (#9)
- `share_links` table: `token_hash PK (sha256), session_id, mode('read'), created_by,
  expires_at, revoked`. Raw token shown once.
- Endpoints: `POST /api/sessions/{id}/share` (mint), `GET /api/sessions` list shares,
  `DELETE /api/shares/{token}` revoke. Public attach route `GET /s/{token}` →
  read-only WS attach.
- **Read-only enforcement:** the shared WS client never calls `WriteInput`/`Resize`;
  enforce server-side (ignore input frames for read-only clients), not just UI. Reuse
  the existing multi-client fan-out in `Session.AddClient`.
- Show a "view-only" banner in the UI.
- **Tests:** token attaches read-only; input frames are dropped server-side; expired/
  revoked token rejected; single-use vs reusable per config.

---

## 5. Milestone M4 — Frontend UX batch

All in `web/static/js/app.js` + `web/static/css/style.css` + `terminal.html`; persist
preferences in `localStorage` next to the existing server list (`loadServers`/`saveServers`).

### M4a — Command palette + shortcuts (#11)
- `Ctrl/Cmd+K` palette: switch session/server, new/rename/kill session, toggle theme.
- Global keys: `Ctrl+Shift+]`/`[` next/prev session, `Ctrl+Shift+N` new. Ensure they
  don't collide with xterm passthrough (scope to non-terminal focus or use a modifier
  xterm ignores).
- **Test:** Playwright — palette opens, fuzzy filter, action dispatch.

### M4b — Terminal theming & fonts (#14)
- xterm.js theme presets (dark/light/solarized…), font family/size, cursor style;
  live-apply via `term.options`. Persisted per browser.
- **Test:** Playwright — change theme/font, reload, setting sticks.

### M4c — Activity indicators + bell (#16)
- Sidebar: unread-output dot per session (from M1 `last_activity_at` vs last-viewed),
  reconnect/disconnect status dot. Terminal bell (`\a`) → optional `Notification` +
  favicon badge.
- Needs lightweight live updates: either poll `/api/sessions` or a small status WS.
- **Test:** Playwright — background session output flips the dot; viewing clears it.

### M4d — Mobile-friendly UX (#20)
- On-screen modifier bar (Ctrl/Esc/Tab/arrows) injecting the right byte sequences;
  pinch-zoom → font scaling; collapsible/auto-hiding sidebar; viewport meta + touch CSS.
- **Test:** Playwright mobile emulation — modifier keys send correct bytes; sidebar toggles.

---

## 6. Milestone M5 — File upload/download (#13)

- Resolve session CWD via `/proc/<pid>/cwd` (Linux; document caveat).
- `POST /api/sessions/{id}/upload` (multipart) → writes into CWD (size-limited; reject
  traversal); `GET /api/sessions/{id}/download?path=…` → serves a file under CWD only
  (path-confined). Raise nginx `client_max_body_size`.
- Frontend: drag-drop onto the terminal uploads to CWD with a toast; right-click a
  selected path → download. Coexists with the existing image-paste handler (`handlePaste`).
- **Tests:** upload lands in CWD; path traversal blocked; download confined to CWD;
  oversize rejected.

---

## 7. Suggested execution order

1. **M3a (login throttling)** — small, security-critical, no dependencies. Do first.
2. **M1 (persistent metadata)** — foundation for the rest.
3. **M4 (UX batch)** — high user-visible value, low risk, parallelizable with backend.
4. **M2b (reaping)** — needs M1.
5. **M3b (share links)** — needs store; pairs naturally with multiuser auth.
6. **M5 (file transfer)** — needs M2 CWD work (or ship a simpler "upload to DataDir" interim).
7. **M2a (session survival)** — largest; schedule its own sub-plan and a dedicated branch.

Each milestone is its own branch + PR; every task ends green
(`go build ./... && go vet ./... && go test ./...`, plus Playwright for frontend items).

---

## 8. Risks & notes

- **#1 is a genuine refactor**, not a feature toggle. tmux dependency + paste/resize/
  history rework. Budget it as a project, and write the sub-plan before coding.
- **Coordinate the store** with `plan-feature-multiuser.md` to avoid two DBs / schema drift.
- **Read-only must be server-enforced** (#9) — UI-only read-only is a security hole.
- **Linux assumptions** (#13 `/proc`, #1 tmux) — document; degrade gracefully on macOS.
- **localStorage prefs are per-browser** — fine until multiuser lands, then consider
  moving theme/prefs onto the user row.

## 9. Open questions

**Resolved 2026-05-29** (see "Decisions locked" above):
- #1 approach → **tmux-backed**.
- Sequence vs. multiuser → **roadmap first, retrofit later**.

**Still open (decide when the milestone is reached):**
1. **Share links (#9):** single-use or reusable-until-expiry? Allow `write` mode later, or read-only forever?
2. **Reaping (#3):** default idle timeout value, and should it ever close sessions that *have* a live client (hard cap on total runtime)?
