# dellipmifanctl

dellipmifanctl takes fan control on a Dell server away from the BMC and drives the fans from temperatures it reads out of Prometheus or Grafana.
On every poll it runs one PromQL query per sensor, maps each reading onto a fan curve, scales it by that sensor's weight, and sends the highest result to the BMC through ipmitool, either on the local /dev/ipmi0 device or to a remote BMC over LAN+.
BMC automatic fan control stays switched off while it runs and is restored when it exits.

It runs against a Dell server with iDRAC that ipmitool can reach, with ipmitool installed on the host and a Prometheus or Grafana datasource that exposes temperature metrics, such as the ones node_exporter provides.

## Installation

Each release carries a static binary for linux amd64 and arm64.

```sh
sudo curl -fL -o /usr/local/bin/dellipmifanctl \
  https://github.com/xsaveopt/dell-ipmitemps/releases/latest/download/dellipmifanctl_linux_amd64
sudo chmod +x /usr/local/bin/dellipmifanctl
```

On an ARM host the file ends in arm64 instead.
Local mode opens /dev/ipmi0, which needs root, and docs/systemd.md has a unit file for running the daemon as a service.

## Configuration

The daemon reads a YAML file from /etc/dellipmifanctl/config.yaml, or from the path in the DELLIPMIFANCTL_CONFIG environment variable, and a -config flag takes precedence over both.
It refuses to start when that file is missing or contains an unknown key.
config.yaml.example documents every option and makes a good starting copy.

| Key | Purpose |
| --- | --- |
| `datasource` | `type` is `prometheus` or `grafana`, with a `url` for Prometheus, or a `url`, `token` and `datasource_uid` for Grafana |
| `ipmi` | `local: true` uses /dev/ipmi0, and `local: false` connects to `host` with `user` and `pass` over LAN+ |
| `sensors` | List of `{ name, query, weight }`, where the weight scales how far above `min_fan_speed` that sensor can push the fans |
| `fan_curve` | `{ temp, percent }` breakpoints in ascending temperature, interpolated linearly, with the first point's percent below it and 100 at or past the last point |
| `min_fan_speed` | Lowest speed the daemon ever commands |
| `poll_interval` | Seconds between polls |
| `temp_critical` | A reading at or above this hands control back to the BMC |
| `max_fetch_failures` | Consecutive failed polls before the fail-safe speed applies |
| `fetch_failure_fan_speed` | Speed the fans are pinned to while sensor data is missing |
| `disable_pcie_cooling_response` | Turns off the BMC's fan ramp for third-party PCIe cards |
| `prediction` | Opt-in windowed and trend readings |
| `smoothing` | Opt-in damping of the applied fan speed |
| `process_prediction` | Opt-in learning of process thermal signatures from /proc |

## Safety behaviour

When the datasource fails for max_fetch_failures polls in a row, the daemon keeps manual control and pins the fans to fetch_failure_fan_speed, because the BMC cannot see the sensors that went dark either.
A sensor at or above temp_critical is handled the other way round, by handing control back to BMC automatic mode.
In both cases curve control resumes on its own once readings return to normal.
On SIGINT, SIGTERM or SIGHUP, and on every hand-back to the BMC, it restores automatic mode and re-enables the default PCIe cooling response, retrying each ipmitool call so a single failed command leaves nothing stuck in manual.

## Predictive mode

The three blocks below are off by default, and with none of them enabled each poll drives the curve from the latest reading.

With prediction enabled, each sensor is read as a short window of recent history, and the curve is fed the higher of a quantile over that window and a Theil-Sen trend projected trend_horizon ahead, where only a rising slope is projected.
Neither term hangs on the newest sample, so a one-scrape spike leaves the fans where they were, while a temperature that keeps climbing lifts both terms and ramps the fans early.
The critical check becomes sustained as well, so a sensor has to stay at or above temp_critical for critical_dwell before control goes back to the BMC.

Smoothing works on the fan speed coming out of the curve, which matters on a steep curve where a degree of noise turns into an audible step.
A change within deadband is skipped, drops are limited to max_step_down per poll so the fans coast down, and rises are limited by max_step_up, which the example sets to 100 so a rise past the deadband applies at once.
Any reading at or above urgent_temp bypasses smoothing entirely.

Process prediction watches /proc, learns the thermal signature of each process name over repeated runs, and keeps that model at model_path.
When a known process starts, a sustained load raises a temporary fan floor ahead of the heat, and a known transient holds the current speed through its spike.
A hint lasts at most preempt_ttl, a hold breaks as soon as the temperature climbs past the learned rise scaled by hold_margin, and the critical fallback always wins over both.
The daemon has to run on the monitored host for this and needs write access to model_path.

## License

GPL-2.0, see LICENSE.
