#!/bin/bash
set -e

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

echo "Building Homestack..."

echo "→ Installing frontend dependencies..."
cd "$ROOT/frontend"
npm ci

echo "→ Building frontend..."
npm run build

echo "→ Building Go binary..."
cd "$ROOT"
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
CGO_ENABLED=0 go build -ldflags "-s -w -X nimbus/internal/build.Version=${VERSION}" -o nimbus ./cmd/server/...

echo "✅ Build complete: $ROOT/nimbus"
echo "   Run with: ./nimbus [--port 8080] [--db ./nimbus.db]"
