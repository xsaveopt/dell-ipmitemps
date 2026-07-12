# dellipmifanctl

**Replaces Dell BMC automatic fan control with a temperature-driven loop — reads temps from Prometheus or Grafana and drives the fans via `ipmitool`.**

> ⚠️ Intended for a trusted LAN. It controls server cooling directly; a bad fan curve or an unreachable datasource can let hardware run hot. Verify your curve under load before relying on it.

## Contents

- [How it works](#how-it-works)
- [Requirements](#requirements)
- [Installation](#installation)
- [Configuration](#configuration)
- [Predictive mode](#predictive-mode)
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
  https://github.com/xsaveopt/dell-ipmitemps/releases/latest/download/dellipmifanctl_linux_amd64
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
| `fetch_failure_fan_speed` | Fail-safe speed to pin fans to on sustained data loss (default 100) |
| `prediction` | Opt-in windowed+trend mode (off by default) |
| `process_prediction` | Opt-in `/proc` learning (off by default) |

## Predictive mode

Both layers below are **opt-in and default off** — with neither configured the daemon behaves exactly as described above (instant readings → curve → speed).

**Windowed + trend (`prediction`).** Instead of reacting to the latest sample, the daemon reads a short window of recent history and drives the curve off `max(quantile(window), last + trend·horizon)`. A high quantile means a brief outlier — say a 1-second NVMe spike that's gone before the next poll — never moves the target, while a genuine sustained rise still ramps the fans *early* via the trend term. The critical-temperature fallback also switches to a *sustained* check (`critical_dwell`), so a momentary blip past the limit won't bounce control to the BMC.

**Process pre-emption (`process_prediction`).** The daemon watches the host's `/proc`, learns each process name's thermal signature over repeated runs (persisted to disk), and acts when a known one launches: a *sustained* load pre-warms the fans (raises a temporary floor) ahead of the heat, and a known *transient* holds the current speed steady through its spike instead of chasing it. A hold is time-bounded, broken the instant temperature climbs past what was learned, and always overridden by the critical fallback — it can delay a needless ramp but can never under-cool a real rise. Requires the daemon to run on the monitored host and to have write access to `model_path`.

## Safety behaviour

If the daemon cannot reach the datasource for `max_fetch_failures` consecutive polls, it keeps manual control and pins the fans to `fetch_failure_fan_speed` (100% by default) rather than handing back to the BMC — the BMC can't see the sensors that just went dark, so the safe move is full cooling until data returns. If instead any sensor exceeds `temp_critical`, it does hand control back to BMC automatic fan control and keeps retrying. Either way, manual curve control resumes once readings return to normal. On `SIGINT`/`SIGTERM`/`SIGHUP`, and whenever it hands back to the BMC, it restores BMC auto mode and re-enables the default PCIe cooling response (retried, so a transient `ipmitool` hiccup can't leave the box stuck in manual).
