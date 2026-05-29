# AI Dev Conductor

A web-based terminal session manager written in Go. Provides password-protected, multi-session shell access through the browser with real-time WebSocket streaming.

## Features

- **Multi-session management** — Create, rename, and delete terminal sessions from a sidebar
- **Multi-server support** — Manage sessions across multiple remote instances from a single UI
- **Real-time streaming** — WebSocket-based terminal I/O with xterm.js
- **Session survival** — Shells run in a detached tmux server and survive a conductor restart/crash; on boot the server reattaches each session with its full scrollback (tmux is a required dependency)
- **Image paste** — Ctrl+V of a clipboard image is delivered to the server's clipboard (or a file) so terminal programs like Claude Code can read it
- **Command palette & shortcuts** — `Ctrl/Cmd+K` palette; `Ctrl+Shift+]`/`[` cycle sessions; `Ctrl+Shift+N` new session
- **Themes & fonts** — Switchable terminal themes (Tokyo Night, Dracula, Solarized, Light) and font size, saved per browser
- **Activity indicators** — Unread-output dots in the sidebar + terminal-bell desktop notifications
- **Mobile-friendly** — On-screen Esc/Tab/Ctrl/arrow keys and a slide-in sidebar on small screens
- **Auto-reconnect** — Exponential backoff reconnection on connection loss
- **Authentication** — Bcrypt password hashing with session tokens (cookie + header)
- **Read-only share links** — Mint a time-boxed public link (`/s/{token}`) that lets anyone watch a session live without controlling it; read-only is enforced server-side, and links can be revoked
- **File upload / download** — Drag a file onto the terminal to upload it into the session's working directory, or download a file from it via the command palette (path-confined server-side; Linux)
- **Production-ready** — Systemd service, health checks, graceful shutdown, dead session cleanup

## Quick Start

> **Prerequisite:** `tmux` must be installed and on `PATH` — it backs every
> session so shells survive restarts, and the server will not start without it
> (`sudo apt install tmux` / `brew install tmux`).

```bash
# Build
go build -o ai-dev-conductor .

# Run (defaults: port 8080, password "admin")
./ai-dev-conductor

# Or with custom config
AI_CONDUCTOR_PASSWORD=secret AI_CONDUCTOR_ADDR=0.0.0.0:5050 ./ai-dev-conductor
```

Open `http://localhost:8080` in your browser, log in, and create a session.

### Using run.sh

```bash
./run.sh start   # Build and start in background (port 5050)
./run.sh status   # Check if running
./run.sh stop     # Graceful shutdown
```

## Configuration

All settings via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `AI_CONDUCTOR_PASSWORD` | `admin` | Login password |
| `AI_CONDUCTOR_ADDR` | `0.0.0.0:8080` | Listen address |
| `AI_CONDUCTOR_DATA_DIR` | `./data/sessions` | Session history directory |
| `AI_CONDUCTOR_SHELL` | auto-detected | Shell binary path |
| `AI_CONDUCTOR_PID_FILE` | *(none)* | PID file path |
| `AI_CONDUCTOR_SESSION_TIMEOUT` | `24h` | Auth session expiry |
| `AI_CONDUCTOR_LOGIN_MAX_ATTEMPTS` | `5` | Failed logins per IP before lockout (`0` disables throttling) |
| `AI_CONDUCTOR_LOGIN_WINDOW` | `1m` | Window in which failures are counted |
| `AI_CONDUCTOR_LOGIN_LOCKOUT` | `1m` | Base lockout, doubling per repeat offence (capped at 16×) |
| `AI_CONDUCTOR_IDLE_TIMEOUT` | *(off)* | Reap sessions with no clients for this long (e.g. `2h`); `0` disables |
| `AI_CONDUCTOR_MAX_SESSIONS` | *(unlimited)* | Cap on concurrent live sessions; `0` is unlimited |
| `AI_CONDUCTOR_MAX_UPLOAD_BYTES` | `104857600` (100 MiB) | Max size of a single file uploaded to a session's working directory |
| `AI_CONDUCTOR_PUBLIC_URL` | *(request origin)* | External base URL (e.g. `https://host`) used to build absolute share-link URLs |
| `AI_CONDUCTOR_SHARE_TTL` | `24h` | Default lifetime of a minted share link (capped at 30 days) |
| `AI_CONDUCTOR_BASE_PATH` | *(none — host root)* | URL path prefix to serve under when behind a reverse-proxy subpath (e.g. `/terminaltest`). All routes, assets, cookies and WebSockets are scoped to it |

### Login brute-force protection

Failed logins are rate-limited per client IP (resolved from `X-Forwarded-For`
when behind a trusted proxy, else the connection address). After
`LOGIN_MAX_ATTEMPTS` failures within `LOGIN_WINDOW`, the IP is locked out for an
exponentially increasing duration and `/api/login` returns `429` with a
`Retry-After` header. Each failure logs a fail2ban-friendly line:

```
auth: failed login attempt, ip=<ip>
```

### Serving under a subpath

By default the app owns the host root (`/`). To run it under a path prefix
behind a reverse proxy — e.g. a staging instance at
`https://home.cloudlabs.live/terminaltest` — set `AI_CONDUCTOR_BASE_PATH`. Every
route, static asset, session cookie and WebSocket is then scoped to that prefix,
and absolute share-link URLs are built from `AI_CONDUCTOR_PUBLIC_URL`:

```bash
AI_CONDUCTOR_ADDR=0.0.0.0:5051 \
AI_CONDUCTOR_BASE_PATH=/terminaltest \
AI_CONDUCTOR_PUBLIC_URL=https://home.cloudlabs.live/terminaltest \
./ai-dev-conductor
```

Proxy the prefix through nginx **without rewriting the path** (no URI part on
`proxy_pass`), so the app receives the full `/terminaltest/...` URL it expects:

```nginx
location /terminaltest {
    proxy_pass http://127.0.0.1:5051;        # no trailing slash — path passed through
    proxy_http_version 1.1;
    proxy_set_header Host       $host;
    proxy_set_header Upgrade    $http_upgrade;
    proxy_set_header Connection $connection_upgrade;
    proxy_read_timeout 86400s;
    proxy_buffering    off;
    client_max_body_size 100m;               # match AI_CONDUCTOR_MAX_UPLOAD_BYTES
}
```

`./run-test.sh {start|stop|status}` wraps exactly this for a port-5051 test
instance with its own data dir and PID file, isolated from the production
instance managed by `run.sh`.

### Session survival across restarts

**tmux is a required dependency** — the server refuses to start if `tmux` is not
on `PATH`. Each session's shell runs inside a **detached tmux session** on a
private server socket under the data dir (`<data-dir>/tmux.sock`). Because that
tmux server is independent of the conductor process, shells keep running when the
conductor is restarted or crashes. On boot the conductor enumerates surviving
tmux sessions, reattaches each one, and seeds reconnecting viewers with the
pane's scrollback (via `tmux capture-pane`). Deleting a session kills its tmux
session; a graceful shutdown only detaches.

Under **systemd**, set `KillMode=process` (already in the shipped unit) so a
`systemctl restart` signals only the conductor and leaves the tmux server alive
for reattach — the default `control-group` kill would otherwise tear it down.

### File upload / download

Files transfer to and from a session's **current working directory** (resolved
via `/proc/<pid>/cwd`, so it tracks `cd` — **Linux only**):

- **Upload:** drag a file onto the terminal, or run *Upload file to session…*
  from the command palette (`Ctrl/Cmd+K`). The uploaded filename is reduced to
  its base name, so a malicious `../` filename cannot escape the directory.
- **Download:** run *Download file from session…* and enter a path relative to
  the working directory. The path is confined server-side — `../` traversal and
  absolute paths outside the CWD are rejected with `403`.

Cap upload size with `AI_CONDUCTOR_MAX_UPLOAD_BYTES`; when behind a proxy, raise
nginx `client_max_body_size` to match (see the proxy block above).

## Architecture

```
main.go                    Entry point, HTTP server, routing (chi)
├── config/config.go       Environment-based configuration
├── api/handlers.go        REST API (health, login, sessions CRUD)
├── internal/
│   ├── auth/
│   │   ├── auth.go        Bcrypt password service, token generation
│   │   └── middleware.go   Session store, RequireAuth middleware
│   ├── session/
│   │   ├── session.go     PTY shell session (creack/pty), client broadcasting
│   │   ├── manager.go     Session lifecycle (create/get/list/delete/closeAll)
│   │   └── history.go     Session output history files
│   └── ws/
│       ├── handler.go     WebSocket upgrade, read/write pumps
│       └── protocol.go    JSON message protocol (input/output/resize)
└── web/
    ├── templates/         login.html, terminal.html (embedded)
    └── static/
        ├── css/style.css  Tokyonight dark theme
        └── js/app.js      TerminalManager class, multi-server, xterm.js
```

## API Endpoints

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `GET` | `/api/health` | No | Health check (`{"status":"ok"}`) |
| `POST` | `/api/login` | No | Authenticate, returns session token |
| `GET` | `/api/sessions` | Yes | List all sessions |
| `POST` | `/api/sessions` | Yes | Create new session |
| `PUT` | `/api/sessions/{id}` | Yes | Rename session |
| `DELETE` | `/api/sessions/{id}` | Yes | Delete session |
| `POST` | `/api/sessions/{id}/share` | Yes | Mint a read-only share link (raw token returned once); optional body `{"ttlSeconds": N}` |
| `GET` | `/api/sessions/{id}/shares` | Yes | List a session's share links (metadata only) |
| `DELETE` | `/api/shares/{id}` | Yes | Revoke a share link by its public id |
| `POST` | `/api/sessions/{id}/upload` | Yes | Upload a file (multipart `file` field) into the session's working directory |
| `GET` | `/api/sessions/{id}/download?path=…` | Yes | Download a file from within the session's working directory (path-confined) |
| `GET` | `/ws/{id}` | Yes | WebSocket terminal connection |
| `GET` | `/s/{token}` | No | Public read-only viewer page for a share link |
| `GET` | `/ws/share/{token}` | No | Public read-only WebSocket attach (input dropped server-side) |

## WebSocket Protocol

Messages are JSON over text frames:

```json
{"type": "input",       "data": "ls -la\n"}
{"type": "output",      "data": "total 42\n..."}
{"type": "resize",      "cols": 120, "rows": 40}
{"type": "paste-image", "mime": "image/png", "data": "<base64>"}
```

### Image paste

A native terminal pastes only text; an image has no text representation, so
xterm.js sends nothing for it. To support image paste, the frontend intercepts
the browser `paste` event, base64-encodes the image, and sends a `paste-image`
message. The server then:

1. **Primary:** writes the image to the system clipboard (`wl-copy` on Wayland,
   `xclip` on X11) and sends `Ctrl+V` (`0x16`) to the PTY, so a clipboard-aware
   program such as **Claude Code** reads the image from the clipboard — exactly
   as it would locally.
2. **Fallback** (headless server with no `$DISPLAY`/`$WAYLAND_DISPLAY` or no
   clipboard tool): saves the image under `<data-dir>/<session>/pastes/` and
   types its absolute path into the terminal so the program can read it from
   disk.

For the primary path on a headless box, run the server under a virtual display
(e.g. `xvfb-run`) with `xclip` installed.

## Multi-Server

The frontend can manage sessions across multiple AI Dev Conductor instances:

1. Click **+ Server** in the sidebar
2. Enter name, URL (`http://host:port`), and password
3. Sessions from all servers appear grouped in the sidebar

Server credentials are stored in `localStorage`. Authentication uses the `X-Session-Token` header for cross-origin requests.

## Production Deployment

See [docs/background-running.md](docs/background-running.md) for systemd service setup, security hardening, and fault tolerance features.

## Dependencies

| Package | Purpose |
|---------|---------|
| [chi](https://github.com/go-chi/chi) | HTTP router |
| [creack/pty](https://github.com/creack/pty) | PTY allocation |
| [gorilla/websocket](https://github.com/gorilla/websocket) | WebSocket server |
| [google/uuid](https://github.com/google/uuid) | Session IDs |
| [golang.org/x/crypto](https://pkg.go.dev/golang.org/x/crypto) | Bcrypt password hashing |
| [xterm.js](https://xtermjs.org/) | Frontend terminal (CDN) |
