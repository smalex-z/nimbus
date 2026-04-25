#!/bin/bash
# Development mode: runs backend and frontend dev server concurrently.

set -e

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

echo "Starting Homestack in development mode..."
echo "  Backend:  http://localhost:8080"
echo "  Frontend: http://localhost:5173"
echo ""

(cd "$ROOT" && go run ./cmd/server/...) &
BACKEND_PID=$!

(cd "$ROOT/frontend" && npm run dev) &
FRONTEND_PID=$!

cleanup() {
    echo ""
    echo "Shutting down..."
    kill "$BACKEND_PID" "$FRONTEND_PID" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

wait
