#!/bin/sh
# Deploy companion binaries and systemd units to the Rock 2F.
# Usage: ./deploy/deploy.sh [user@host]
set -e

HOST="${1:-root@192.168.88.220}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"

echo "=== Building arm64 binaries ==="
cd "$REPO_DIR/server"

# Always rebuild web assets so the embedded server never serves a stale
# bundle. `cp -r dist/. dest/` (not `cp -r dist dest`) replaces dest's
# contents in place instead of nesting a dist/ subdirectory inside it.
echo "  rebuilding web assets..."
cd "$REPO_DIR/web" && npm run build
rm -rf "$REPO_DIR/server/web/dist"
mkdir -p "$REPO_DIR/server/web/dist"
cp -r dist/. "$REPO_DIR/server/web/dist/"
cd "$REPO_DIR/server"

GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o "$SCRIPT_DIR/bin/companion-api" ./cmd/companion-api
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o "$SCRIPT_DIR/bin/companion-net" ./cmd/companion-net
echo "  companion-api: $(du -h "$SCRIPT_DIR/bin/companion-api" | cut -f1)"
echo "  companion-net: $(du -h "$SCRIPT_DIR/bin/companion-net" | cut -f1)"

echo "=== Uploading to $HOST ==="
# Stop services first: scp open()+truncate()s the destination, which fails
# with ETXTBSY while the binary is the running image of a live process.
ssh "$HOST" "systemctl stop companion-api companion-net" || true
scp "$SCRIPT_DIR/bin/companion-api" "$HOST:/usr/bin/companion-api"
scp "$SCRIPT_DIR/bin/companion-net" "$HOST:/usr/bin/companion-net"

# Upload unit files
scp "$SCRIPT_DIR/systemd/mediamtx.service"      "$HOST:/etc/systemd/system/mediamtx.service"
scp "$SCRIPT_DIR/systemd/companion-api.service" "$HOST:/etc/systemd/system/companion-api.service"
scp "$SCRIPT_DIR/systemd/companion-net.service" "$HOST:/etc/systemd/system/companion-net.service"
scp "$SCRIPT_DIR/systemd/rfkill-unblock.service" "$HOST:/etc/systemd/system/rfkill-unblock.service"
scp "$SCRIPT_DIR/mediamtx.yml"                  "$HOST:/etc/mediamtx.yml"

echo "=== Activating on device ==="
ssh "$HOST" "
    mkdir -p /data/captures
    systemctl daemon-reload
    systemctl enable bluetooth mediamtx companion-api companion-net
    systemctl restart mediamtx companion-api companion-net
    echo 'Services started:'
    systemctl is-active mediamtx companion-api companion-net
"

echo "=== Done! ==="
echo "Open http://$(echo "$HOST" | sed 's/.*@//'):8000 in your browser."
