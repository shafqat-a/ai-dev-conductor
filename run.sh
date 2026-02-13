#!/bin/sh
# Run AI Dev Conductor in the background on port 5050
# Usage: ./run.sh [start|stop|status]

DIR="$(cd "$(dirname "$0")" && pwd)"
PID_FILE="$DIR/ai-dev-conductor.pid"
LOG_FILE="$DIR/ai-dev-conductor.log"

export AI_CONDUCTOR_ADDR="0.0.0.0:5050"
export AI_CONDUCTOR_PASSWORD="Orion123@"
export AI_CONDUCTOR_PID_FILE="$PID_FILE"

case "${1:-start}" in
    start)
        if [ -f "$PID_FILE" ] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
            echo "Already running (PID $(cat "$PID_FILE"))"
            exit 1
        fi

        echo "Building..."
        cd "$DIR" && go build -o ai-dev-conductor . || exit 1

        echo "Starting on port 5050..."
        nohup "$DIR/ai-dev-conductor" >> "$LOG_FILE" 2>&1 &

        sleep 1
        if [ -f "$PID_FILE" ] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
            echo "Running (PID $(cat "$PID_FILE"))"
            echo "Logs: $LOG_FILE"
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
