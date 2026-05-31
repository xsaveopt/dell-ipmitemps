# dellipmifanctl

**Replaces Dell BMC automatic fan control with a temperature-driven loop — reads temps from Prometheus or Grafana and drives the fans via `ipmitool`.**

> ⚠️ Intended for a trusted LAN. It controls server cooling directly; a bad fan curve or an unreachable datasource can let hardware run hot. Verify your curve under load before relying on it.

## Contents

- [How it works](#how-it-works)
- [Requirements](#requirements)
- [Installation](#installation)
- [Configuration](#configuration)
- [Safety behaviour](#safety-behaviour)

## How it works

A single Go binary polls one or more PromQL queries on each cycle, interpolates each reading against a user-defined fan curve, weights it, and takes the maximum across sensors as the target fan speed. That percentage is pushed to the Dell BMC over `ipmitool` (local `/dev/ipmi0` or remote LAN+). The BMC's own automatic control is disabled while the daemon runs and always restored on exit. If the datasource goes dark or a sensor crosses a critical threshold, control is handed straight back to the BMC until things recover.

## Requirements

- Dell server with iDRAC, reachable via `ipmitool` (locally or over LAN+)
- `ipmitool` installed on the host
- A Prometheus or Grafana datasource exposing temperature metrics (e.g. `node_exporter`)

No other runtime dependencies — HTTP, JSON, and the fan math are all in the binary.

## Installation

Download the latest binary for your architecture and make it executable:

```bash
sudo curl -fL -o /usr/local/bin/dellipmifanctl \
  https://github.com/sratabix/dell-ipmitemps/releases/latest/download/dellipmifanctl_linux_amd64
sudo chmod +x /usr/local/bin/dellipmifanctl
```

Replace `amd64` with `arm64` on ARM hosts. To run it as a service, see [docs/systemd.md](docs/systemd.md).

## Configuration

Configuration lives in a YAML file, by default `/etc/dellipmifanctl/config.yaml` (override with `-config <path>` or the `DELLIPMIFANCTL_CONFIG` env var). The daemon refuses to start if the file is missing. Copy [`config.yaml.example`](config.yaml.example) as a starting point — it documents every option inline.

| Setting | Purpose |
| --- | --- |
| `datasource.type` | `prometheus` or `grafana` |
| `sensors` | List of `{ name, query, weight }` — PromQL per sensor; weight scales its pull |
| `fan_curve` | Ascending `{ temp, percent }` breakpoints, interpolated linearly |
| `ipmi.local` | `true` for `/dev/ipmi0`, `false` for a remote BMC over LAN+ |
| `temp_critical` | Above this °C, hand control back to the BMC |
| `disable_pcie_cooling_response` | Suppress the BMC's 100% ramp for third-party PCIe cards |
| `min_fan_speed` / `poll_interval` / `max_fetch_failures` | Floor speed, poll cadence, failure tolerance |

## Safety behaviour

If the daemon cannot reach the datasource for `max_fetch_failures` consecutive polls, or any sensor exceeds `temp_critical`, it hands control back to BMC automatic fan control and keeps retrying; manual control resumes once readings return to normal. On `SIGINT`/`SIGTERM`/`SIGHUP` it always restores BMC auto mode (and the default PCIe cooling response, if it was disabled) before exiting.
