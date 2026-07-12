# Running under systemd

Reference unit for running `dellipmifanctl` as a service. The binary runs fine
in the foreground for testing (`sudo ./dellipmifanctl -config ./config.yaml`);
nothing here is mandatory.

## Privileges

This daemon drives the BMC through `ipmitool`. In **local** mode it talks to
`/dev/ipmi0`, which requires root, so the unit below runs as root — the
hardening sandbox a typical exporter uses would block that device. In **remote**
mode (`ipmi.local: false`) it only needs network access, so you can drop the
device dependency and tighten the sandbox if you prefer.

It also writes its last commanded speed to `/run/dellipmifanctl.state` so a
restart resumes where it left off.

## Unit file

Drop at `/etc/systemd/system/dellipmifanctl.service`:

```ini
[Unit]
Description=Dell IPMI Fan Temperature Controller
Documentation=https://github.com/xsaveopt/dell-ipmitemps
After=network-online.target
Wants=network-online.target
# Local mode only: wait for the IPMI device node.
After=dev-ipmi0.device
Wants=dev-ipmi0.device

[Service]
Type=simple
ExecStart=/usr/local/bin/dellipmifanctl -config /etc/dellipmifanctl/config.yaml
Restart=on-failure
RestartSec=15
# Give the daemon time to restore BMC auto mode on stop before SIGKILL.
TimeoutStopSec=15

[Install]
WantedBy=multi-user.target
```

Install the binary and config, then enable:

```sh
sudo install -m 0755 dellipmifanctl /usr/local/bin/dellipmifanctl
sudo install -d /etc/dellipmifanctl
sudo install -m 0640 config.yaml.example /etc/dellipmifanctl/config.yaml
sudo nano /etc/dellipmifanctl/config.yaml   # edit before first start

sudo systemctl daemon-reload
sudo systemctl enable --now dellipmifanctl
sudo journalctl -fu dellipmifanctl
```

## Removing

```sh
sudo systemctl disable --now dellipmifanctl
sudo rm /etc/systemd/system/dellipmifanctl.service
sudo rm /usr/local/bin/dellipmifanctl
# config is left in place; remove it too if you want a clean uninstall:
sudo rm -rf /etc/dellipmifanctl
sudo systemctl daemon-reload
```
