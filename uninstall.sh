#!/usr/bin/env bash
set -euo pipefail

purge=false

for arg in "$@"; do
  case "$arg" in
    --purge|-p) purge=true ;;
    *) echo "Unknown option: $arg"; echo "Usage: $0 [--purge|-p]"; exit 1 ;;
  esac
done

if [[ $EUID -ne 0 ]]; then
  echo "Run as root: sudo ./uninstall.sh"
  exit 1
fi

echo "Stopping and disabling dellipmifanctl..."
systemctl stop dellipmifanctl 2>/dev/null || true
systemctl disable dellipmifanctl 2>/dev/null || true

rm -f /etc/systemd/system/dellipmifanctl.service
systemctl daemon-reload

rm -f /usr/local/bin/dellipmifanctl

if $purge; then
  rm -rf /etc/dellipmifanctl
  echo ""
  echo "dellipmifanctl removed (config purged)."
else
  echo ""
  echo "dellipmifanctl removed. Config left in place: /etc/dellipmifanctl/"
  echo "To also remove config, run with --purge."
fi
