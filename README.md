# dellipmifanctl

Replaces Dell BMC automatic fan control with a temperature-driven loop. Reads temperatures from Prometheus or Grafana and sets fan speed via `ipmitool`.

## Contents

- [Requirements](#requirements)
- [Installation](#installation)
- [Updating](#updating)
- [Uninstall](#uninstall)
- [Configuration](#configuration)

## Requirements

- Dell server with iDRAC
- `ipmitool`, `curl`, `jq`, `bc`
- Prometheus or Grafana datasource exposing temperature metrics (e.g. via `node_exporter`)

## Installation

**1. Download and extract the latest release.**

**2. Run the installer:**

```bash
sudo ./install.sh
```

This copies the script to `/usr/local/bin/dellipmifanctl` and installs a systemd unit.

**3. Edit the config:**

```bash
sudo nano /etc/dellipmifanctl/config.conf
```

Set your datasource URL, IPMI connection details, temperature sensors (as PromQL queries), and fan curve. See `config.conf.example` for all options.

**4. Enable and start the service:**

```bash
sudo systemctl enable --now dellipmifanctl
sudo journalctl -fu dellipmifanctl
```

## Updating

Download and extract the latest release, then run:

```bash
sudo ./update.sh
```

This replaces the installed binary and restarts the service if running. Your config is always preserved.

## Uninstall

```bash
sudo ./uninstall.sh           # leaves config in /etc/dellipmifanctl/
sudo ./uninstall.sh --purge   # also removes config
```

## Configuration

Key settings in `config.conf`:

| Setting                         | Description                                           |
| ------------------------------- | ----------------------------------------------------- |
| `DATASOURCE_TYPE`               | `prometheus` or `grafana`                             |
| `TEMP_SENSORS`                  | Array of `name:promql:weight` triples                 |
| `FAN_CURVE`                     | `celsius:percent` breakpoints — interpolated linearly |
| `TEMP_CRITICAL`                 | Above this °C, hand control back to BMC               |
| `DISABLE_PCIE_COOLING_RESPONSE` | Suppress BMC's 100% ramp for third-party PCIe cards   |

If the daemon cannot reach the datasource for `MAX_FETCH_FAILURES` consecutive polls, or a sensor exceeds `TEMP_CRITICAL`, it falls back to BMC automatic fan control. It always restores auto mode on shutdown.
