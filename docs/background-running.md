# Background Running & Fault Tolerance

This document covers the production-readiness features added to AI Dev Conductor for background operation, automatic recovery, and fault tolerance.

## Overview

| Feature | Purpose |
|---------|---------|
| Systemd service | Background operation with auto-restart |
| Health endpoint | Monitoring and liveness checks |
| Dead session cleanup | Automatic removal of sessions with exited shell processes |
| WebSocket reconnect | Frontend auto-reconnects on server restart or network blip |
| HTTP server timeouts | Protection against slow/stalled connections |
| PID file | Process management in non-systemd environments |
| SIGHUP handling | Survive terminal hangup when backgrounded |

## Health Check

```
GET /api/health
```

Public endpoint (no authentication required). Returns:

```json
{"status": "ok"}
```

Use this for load balancer health checks, uptime monitors, or the systemd watchdog.

## Dead Session Auto-Cleanup

When a shell process exits (user types `exit`, process crashes, or gets killed), the system automatically:

1. Detects the exit via `cmd.Wait()` in `session.waitProcess()`
2. Closes the PTY file descriptor, which causes `readPTY()` to exit and close the `done` channel
3. Connected WebSocket clients receive the close signal and disconnect
4. The `OnProcessExit` callback fires, removing the session from the manager's map

No manual cleanup required. Connected clients will see the connection close, and the frontend will attempt to reconnect (which will fail with "session not found" if the session is gone).

## WebSocket Auto-Reconnect

When the WebSocket connection drops unexpectedly (server restart, network issue), the frontend automatically reconnects:

- **Backoff schedule**: 1s, 2s, 4s, 8s, 16s, 30s, 30s, 30s... (exponential, capped at 30s)
- **Max attempts**: 20
- **Status display**: `[Reconnecting (N/20)...]` shown in the terminal in yellow
- **After max attempts**: `[Connection lost. Click to reconnect.]` shown in red, with a click/keypress handler to retry
- **On successful reconnect**: counter resets, terminal size is re-sent

Reconnection is **not** attempted when:
- The user manually disconnects (switches sessions, deletes session, navigates away)
- The user connects to a different session

## HTTP Server Timeouts

```go
ReadTimeout:  15s   // Max time to read the full request (headers + body)
WriteTimeout: 15s   // Max time to write the response
IdleTimeout:  60s   // Max time for keep-alive connections between requests
```

These prevent resource exhaustion from slow or stalled HTTP connections. They do **not** affect WebSocket connections, which are hijacked from the HTTP server after the upgrade handshake.

## PID File

Set the `AI_CONDUCTOR_PID_FILE` environment variable to write a PID file on startup:

```bash
export AI_CONDUCTOR_PID_FILE=/var/run/ai-dev-conductor.pid
```

The PID file is removed on graceful shutdown (SIGINT/SIGTERM). If the process crashes, the stale PID file will remain and should be handled by your process manager.

When not set (the default), no PID file is written.

## SIGHUP Handling

SIGHUP is ignored so the process doesn't crash when the controlling terminal is closed (e.g., SSH disconnect while running in background). Only SIGINT and SIGTERM trigger graceful shutdown.

## Graceful Shutdown

On SIGINT or SIGTERM:

1. All terminal sessions are closed (shell processes killed, PTYs closed, clients notified)
2. HTTP server stops accepting new connections
3. In-flight requests and active WebSocket connections are given **15 seconds** to drain
4. PID file is removed (if configured)
5. Process exits

## Systemd Service

### Installation

1. Build and install the binary:

```bash
go build -o ai-dev-conductor .
sudo cp ai-dev-conductor /usr/local/bin/
```

2. Create the system user and directories:

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin ai-conductor
sudo mkdir -p /var/lib/ai-dev-conductor
sudo chown ai-conductor:ai-conductor /var/lib/ai-dev-conductor
```

3. Create the environment file:

```bash
sudo mkdir -p /etc/ai-dev-conductor
sudo tee /etc/ai-dev-conductor/env <<'EOF'
AI_CONDUCTOR_PASSWORD=your-secure-password
AI_CONDUCTOR_ADDR=0.0.0.0:8080
AI_CONDUCTOR_DATA_DIR=/var/lib/ai-dev-conductor/sessions
AI_CONDUCTOR_SHELL=/bin/bash
EOF
sudo chmod 600 /etc/ai-dev-conductor/env
```

4. Install and start the service:

```bash
sudo cp ai-dev-conductor.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now ai-dev-conductor
```

### Service Configuration

The service file (`ai-dev-conductor.service`) includes:

| Directive | Value | Purpose |
|-----------|-------|---------|
| `Type` | `notify` | Systemd waits for readiness notification |
| `Restart` | `on-failure` | Auto-restart on crash (not on clean exit) |
| `RestartSec` | `3s` | Wait 3 seconds between restart attempts |
| `WatchdogSec` | `30s` | Systemd kills the process if no watchdog ping in 30s |
| `NoNewPrivileges` | `true` | Process cannot gain new privileges |
| `ProtectSystem` | `strict` | Filesystem is read-only except allowed paths |
| `ProtectHome` | `true` | No access to /home |
| `ReadWritePaths` | `/var/lib/ai-dev-conductor` | Only writable path |
| `PrivateTmp` | `true` | Isolated /tmp |

### Managing the Service

```bash
# Check status
sudo systemctl status ai-dev-conductor

# View logs
sudo journalctl -u ai-dev-conductor -f

# Restart
sudo systemctl restart ai-dev-conductor

# Stop
sudo systemctl stop ai-dev-conductor
```

### Verifying Auto-Restart

```bash
# Kill the process hard (simulates crash)
sudo kill -9 $(pgrep ai-dev-conductor)

# Watch it restart within ~3 seconds
sudo systemctl status ai-dev-conductor
```

## Configuration Reference

All configuration via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `AI_CONDUCTOR_PASSWORD` | `admin` | Login password |
| `AI_CONDUCTOR_ADDR` | `0.0.0.0:8080` | Listen address |
| `AI_CONDUCTOR_DATA_DIR` | `./data/sessions` | Session history directory |
| `AI_CONDUCTOR_SHELL` | auto-detected | Shell binary path |
| `AI_CONDUCTOR_PID_FILE` | *(none)* | PID file path |
