#!/usr/bin/env bash
set -euo pipefail

BIN_DIR="/usr/local/bin"
CONF_DIR="/etc/dellipmifanctl"
SERVICE_FILE="/etc/systemd/system/dellipmifanctl.service"

SCRIPT_DIR="$(cd "$(dirname "$(readlink -f "$0")")" && pwd)"

if [[ $EUID -ne 0 ]]; then
  echo "Run as root: sudo ./install.sh"
  exit 1
fi

echo "Installing dellipmifanctl..."

install -d "$CONF_DIR"

install -m 755 "$SCRIPT_DIR/dellipmifanctl.sh" "$BIN_DIR/dellipmifanctl"

# Only install config if one doesn't already exist
if [[ -f "$CONF_DIR/config.conf" ]]; then
  echo "  Existing config preserved: $CONF_DIR/config.conf"
else
  install -m 640 "$SCRIPT_DIR/config.conf.example" "$CONF_DIR/config.conf"
  echo "  Config installed: $CONF_DIR/config.conf"
  echo ""
  echo "  >>> Edit $CONF_DIR/config.conf before starting the service. <<<"
  echo ""
fi

install -m 644 "$SCRIPT_DIR/dellipmifanctl.service" "$SERVICE_FILE"
systemctl daemon-reload

echo ""
echo "Installation complete."
echo ""
echo "Next steps:"
echo "  1. Edit $CONF_DIR/config.conf"
echo "  2. sudo systemctl enable --now dellipmifanctl"
echo "  3. sudo journalctl -fu dellipmifanctl   # follow logs"
