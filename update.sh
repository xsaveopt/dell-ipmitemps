#!/usr/bin/env bash
set -euo pipefail

BIN_DIR="/usr/local/bin"
SCRIPT_DIR="$(cd "$(dirname "$(readlink -f "$0")")" && pwd)"

if [[ $EUID -ne 0 ]]; then
  echo "Run as root: sudo ./update.sh"
  exit 1
fi

new_version="$(grep '^VERSION=' "$SCRIPT_DIR/dellipmifanctl.sh" | cut -d'"' -f2)"
echo "Installing version: ${new_version}"

install -m 755 "$SCRIPT_DIR/dellipmifanctl.sh" "$BIN_DIR/dellipmifanctl"

if systemctl is-active --quiet dellipmifanctl; then
  systemctl restart dellipmifanctl
  echo "Service restarted."
fi

echo "Done."
