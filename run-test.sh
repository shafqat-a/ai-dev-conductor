#!/bin/sh
# Run a TEST instance of AI Dev Conductor on port 5051, served under the
# /terminaltest base path at https://home.cloudlabs.live/terminaltest.
# State is kept separate from the production instance (run.sh) so the two
# never share sessions, DB, or PID files.
# Usage: ./run-test.sh [start|stop|status]

DIR="$(cd "$(dirname "$0")" && pwd)"
PID_FILE="$DIR/ai-dev-conductor-test.pid"
LOG_FILE="$DIR/ai-dev-conductor-test.log"

export AI_CONDUCTOR_ADDR="0.0.0.0:5051"
export AI_CONDUCTOR_PASSWORD="Orion123@"
export AI_CONDUCTOR_BASE_PATH="/terminaltest"
export AI_CONDUCTOR_PUBLIC_URL="https://home.cloudlabs.live/terminaltest"
export AI_CONDUCTOR_DATA_DIR="$DIR/data/sessions-test"
export AI_CONDUCTOR_PID_FILE="$PID_FILE"

case "${1:-start}" in
    start)
        if [ -f "$PID_FILE" ] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
            echo "Already running (PID $(cat "$PID_FILE"))"
            exit 1
        fi

        echo "Building..."
        cd "$DIR" && go build -o ai-dev-conductor . || exit 1

        echo "Starting test instance on port 5051 (base path /terminaltest)..."
        nohup "$DIR/ai-dev-conductor" >> "$LOG_FILE" 2>&1 &

        sleep 1
        if [ -f "$PID_FILE" ] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
            echo "Running (PID $(cat "$PID_FILE"))"
            echo "Logs: $LOG_FILE"
            echo "URL:  https://home.cloudlabs.live/terminaltest/"
        else
            echo "Failed to start. Check $LOG_FILE"
            exit 1
        fi
        ;;

    stop)
        if [ -f "$PID_FILE" ]; then
            PID=$(cat "$PID_FILE")
            echo "Stopping (PID $PID)..."
            kill "$PID" 2>/dev/null
            sleep 2
            if kill -0 "$PID" 2>/dev/null; then
                kill -9 "$PID"
            fi
            rm -f "$PID_FILE"
            echo "Stopped"
        else
            echo "Not running"
        fi
        ;;

    status)
        if [ -f "$PID_FILE" ] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
            echo "Running (PID $(cat "$PID_FILE"))"
        else
            echo "Not running"
            rm -f "$PID_FILE" 2>/dev/null
        fi
        ;;

    *)
        echo "Usage: $0 {start|stop|status}"
        exit 1
        ;;
esac
