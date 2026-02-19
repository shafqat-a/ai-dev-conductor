# AI Dev Conductor

A web-based terminal session manager written in Go. Provides password-protected, multi-session shell access through the browser with real-time WebSocket streaming.

## Features

- **Multi-session management** — Create, rename, and delete terminal sessions from a sidebar
- **Multi-server support** — Manage sessions across multiple remote instances from a single UI
- **Real-time streaming** — WebSocket-based terminal I/O with xterm.js
- **Session persistence** — Output history saved to disk and replayed on reconnect
- **Binary data support** — Full binary passthrough for clipboard paste (images, non-UTF8 data)
- **Auto-reconnect** — Exponential backoff reconnection on connection loss
- **Authentication** — Bcrypt password hashing with session tokens (cookie + header)
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
| `GET` | `/ws/{id}` | Yes | WebSocket terminal connection |

## WebSocket Protocol

Messages are JSON over text frames:

```json
{"type": "input",  "data": "ls -la\n"}
{"type": "output", "data": "total 42\n..."}
{"type": "resize", "cols": 120, "rows": 40}
```

Binary WebSocket frames are written directly to the PTY — this supports pasting images and other binary clipboard content into programs running in the terminal (e.g. Claude Code).

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
