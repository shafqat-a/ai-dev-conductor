# AI Dev Conductor

A web-based terminal session manager written in Go. Provides password-protected, multi-session shell access through the browser with real-time WebSocket streaming.

## Features

- **Multi-session management** — Create, rename, and delete terminal sessions from a sidebar
- **Multi-server support** — Manage sessions across multiple remote instances from a single UI
- **Real-time streaming** — WebSocket-based terminal I/O with xterm.js
- **Session persistence** — Output history saved to disk and replayed on reconnect
- **Image paste** — Ctrl+V of a clipboard image is delivered to the server's clipboard (or a file) so terminal programs like Claude Code can read it
- **Command palette & shortcuts** — `Ctrl/Cmd+K` palette; `Ctrl+Shift+]`/`[` cycle sessions; `Ctrl+Shift+N` new session
- **Themes & fonts** — Switchable terminal themes (Tokyo Night, Dracula, Solarized, Light) and font size, saved per browser
- **Activity indicators** — Unread-output dots in the sidebar + terminal-bell desktop notifications
- **Mobile-friendly** — On-screen Esc/Tab/Ctrl/arrow keys and a slide-in sidebar on small screens
- **Auto-reconnect** — Exponential backoff reconnection on connection loss
- **Authentication** — Bcrypt password hashing with session tokens (cookie + header)
- **Read-only share links** — Mint a time-boxed public link (`/s/{token}`) that lets anyone watch a session live without controlling it; read-only is enforced server-side, and links can be revoked
- **Production-ready** — Systemd service, health checks, graceful shutdown, dead session cleanup

## Quick Start

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
| `AI_CONDUCTOR_PUBLIC_URL` | *(request origin)* | External base URL (e.g. `https://host`) used to build absolute share-link URLs |
| `AI_CONDUCTOR_SHARE_TTL` | `24h` | Default lifetime of a minted share link (capped at 30 days) |

### Login brute-force protection

Failed logins are rate-limited per client IP (resolved from `X-Forwarded-For`
when behind a trusted proxy, else the connection address). After
`LOGIN_MAX_ATTEMPTS` failures within `LOGIN_WINDOW`, the IP is locked out for an
exponentially increasing duration and `/api/login` returns `429` with a
`Retry-After` header. Each failure logs a fail2ban-friendly line:

```
auth: failed login attempt, ip=<ip>
```

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
