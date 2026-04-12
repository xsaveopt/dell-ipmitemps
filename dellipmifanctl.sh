#!/usr/bin/env bash
set -euo pipefail

VERSION="0.0.0"

# ---------------------------------------------------------------------------
# dellipmifanctl — Dell IPMI fan speed controller
# Reads temperatures from Prometheus/Grafana and sets fan speed via ipmitool.
# ---------------------------------------------------------------------------

CONFIG_FILE="${DELLIPMIFANCTL_CONFIG:-/etc/dellipmifanctl/config.conf}"
STATE_FILE="/run/dellipmifanctl.state"

# ---------------------------------------------------------------------------
# Logging

log()       { printf '[%s] %s\n'       "$(date '+%Y-%m-%d %H:%M:%S')" "$*"; }
log_warn()  { printf '[%s] WARN  %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*" >&2; }
log_error() { printf '[%s] ERROR %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*" >&2; }

# ---------------------------------------------------------------------------
# Bootstrap

if [[ ! -f "$CONFIG_FILE" ]]; then
  log_error "Config not found: $CONFIG_FILE"
  log_error "Copy config.conf.example to $CONFIG_FILE and edit it."
  exit 1
fi

# shellcheck source=/dev/null
source "$CONFIG_FILE"

# ---------------------------------------------------------------------------
# Datasource — temperature data fetching
# Expects config variables to be set.

# Query Prometheus directly via HTTP API.
# Usage: _query_prometheus "promql"
# Prints the scalar result or returns 1 on failure.
_query_prometheus() {
  local query="$1"
  local encoded
  encoded="$(python3 -c "import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1]))" "$query" 2>/dev/null \
    || printf '%s' "$query" | sed 's/ /%20/g; s/{/%7B/g; s/}/%7D/g; s/"/%22/g; s/=/%3D/g; s/~/%7E/g; s/,/%2C/g')"

  local result
  result="$(curl -sf --max-time 10 "${PROMETHEUS_URL}/api/v1/query?query=${encoded}")" || return 1

  # Extract the first numeric value from the result vector/scalar
  printf '%s' "$result" | jq -re '
    .data.result[0].value[1] //
    .data.result[0].values[-1][1] //
    empty
  ' 2>/dev/null || return 1
}

# Query via Grafana datasource proxy.
# Usage: _query_grafana "promql"
_query_grafana() {
  local query="$1"
  local encoded
  encoded="$(python3 -c "import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1]))" "$query" 2>/dev/null \
    || printf '%s' "$query" | sed 's/ /%20/g; s/{/%7B/g; s/}/%7D/g; s/"/%22/g; s/=/%3D/g; s/~/%7E/g; s/,/%2C/g')"

  local result
  result="$(curl -sf --max-time 10 \
    -H "Authorization: Bearer ${GRAFANA_TOKEN}" \
    "${GRAFANA_URL}/api/datasources/proxy/uid/${GRAFANA_DATASOURCE_UID}/api/v1/query?query=${encoded}")" || return 1

  printf '%s' "$result" | jq -re '
    .data.result[0].value[1] //
    .data.result[0].values[-1][1] //
    empty
  ' 2>/dev/null || return 1
}

# Public interface: fetch_temp "promql"
# Dispatches to the configured backend. Prints value or returns 1.
fetch_temp() {
  local query="$1"
  case "$DATASOURCE_TYPE" in
    prometheus) _query_prometheus "$query" ;;
    grafana)    _query_grafana    "$query" ;;
    *)
      log_error "Unknown DATASOURCE_TYPE: $DATASOURCE_TYPE"
      return 1
      ;;
  esac
}

# ---------------------------------------------------------------------------
# IPMI — Dell fan control
# Expects IPMI_LOCAL, IPMI_HOST, IPMI_USER, IPMI_PASS.

# Build the base ipmitool command with connection args.
# Remote connections use -I lanplus (IPMI v2.0), which Dell iDRAC requires.
_ipmitool() {
  if [[ "$IPMI_LOCAL" == "true" ]]; then
    ipmitool "$@"
  else
    ipmitool -I lanplus -H "$IPMI_HOST" -U "$IPMI_USER" -P "$IPMI_PASS" "$@"
  fi
}

# Disable BMC automatic fan control (manual mode).
ipmi_set_manual() {
  _ipmitool raw 0x30 0x30 0x01 0x00
}

# Re-enable BMC automatic fan control.
ipmi_set_auto() {
  _ipmitool raw 0x30 0x30 0x01 0x01
}

# Set fan speed as a percentage (0–100).
# The value is the decimal percentage expressed as hex: 80% → 0x50.
# Usage: ipmi_set_speed 45
ipmi_set_speed() {
  local pct="$1"
  local hex
  hex="$(printf '0x%02x' "$pct")"
  _ipmitool raw 0x30 0x30 0x02 0xff "$hex"
}

# Disable the BMC's aggressive fan response to unrecognised third-party PCIe
# cards (e.g. non-Dell GPUs). Without this, the BMC ramps fans to ~100%
# whenever it detects an unknown card, regardless of actual temperature.
# WARNING: ensure your cooling is adequate before using this — the BMC's
# default behaviour exists for a reason. Run stress tests after enabling.
ipmi_disable_pcie_cooling() {
  _ipmitool raw 0x30 0xce 0x00 0x16 0x05 0x00 0x00 0x00 0x05 0x00 0x01 0x00 0x00
}

# Re-enable the BMC's default third-party PCIe cooling response.
ipmi_enable_pcie_cooling() {
  _ipmitool raw 0x30 0xce 0x00 0x16 0x05 0x00 0x00 0x00 0x05 0x00 0x00 0x00 0x00
}

# ---------------------------------------------------------------------------
# Fan curve — interpolation
# Expects FAN_CURVE and MIN_FAN_SPEED.

# Given a temperature, return the fan speed % from the configured curve.
# Uses linear interpolation between breakpoints.
# Usage: curve_speed 67
curve_speed() {
  local temp="$1"
  local -a temps speeds

  for point in "${FAN_CURVE[@]}"; do
    temps+=( "${point%%:*}" )
    speeds+=( "${point##*:}" )
  done

  local n="${#temps[@]}"

  # Below first breakpoint
  if (( $(echo "$temp <= ${temps[0]}" | bc -l) )); then
    printf '%d' "${speeds[0]}"
    return
  fi

  # Above last breakpoint
  if (( $(echo "$temp >= ${temps[$((n-1))]}" | bc -l) )); then
    printf '100'
    return
  fi

  # Find surrounding breakpoints and interpolate
  local i
  for (( i=1; i<n; i++ )); do
    if (( $(echo "$temp <= ${temps[$i]}" | bc -l) )); then
      local t0="${temps[$((i-1))]}" t1="${temps[$i]}"
      local s0="${speeds[$((i-1))]}" s1="${speeds[$i]}"
      # speed = s0 + (temp - t0) / (t1 - t0) * (s1 - s0)
      local result
      result="$(echo "scale=0; $s0 + ($temp - $t0) / ($t1 - $t0) * ($s1 - $s0)" | bc -l)"
      printf '%d' "${result%.*}"
      return
    fi
  done
}

# Given an array of (name temp weight) triples, compute final fan speed.
# Final speed = max of each sensor's weighted curve speed, floored at MIN_FAN_SPEED.
# Usage: compute_fan_speed "cpu 62 1.0" "nvme 48 0.8"
compute_fan_speed() {
  local max_speed="$MIN_FAN_SPEED"

  for entry in "$@"; do
    local name temp weight
    read -r name temp weight <<< "$entry"

    local raw_speed
    raw_speed="$(curve_speed "$temp")"

    # Apply weight: scale speed toward minimum proportionally
    local weighted
    weighted="$(echo "scale=0; $MIN_FAN_SPEED + ($raw_speed - $MIN_FAN_SPEED) * $weight" | bc -l)"
    weighted="${weighted%.*}"

    if (( weighted > max_speed )); then
      max_speed="$weighted"
    fi
  done

  printf '%d' "$max_speed"
}

# ---------------------------------------------------------------------------
# Signal handling — always restore auto mode on exit

_restore_auto() {
  log "Restoring BMC automatic fan control."
  ipmi_set_auto
  if [[ "${DISABLE_PCIE_COOLING_RESPONSE:-false}" == "true" ]]; then
    log "Restoring BMC PCIe cooling response."
    ipmi_enable_pcie_cooling
  fi
  rm "$STATE_FILE" || true
  exit 0
}
trap _restore_auto SIGTERM SIGINT SIGHUP

# ---------------------------------------------------------------------------
# Main loop

consecutive_failures=0
in_auto_mode=false
last_speed=-1

if [[ -f "$STATE_FILE" ]]; then
  last_speed="$(< "$STATE_FILE")"
  log "Restored last speed from state: ${last_speed}%"
fi

log "dellipmifanctl starting. Poll interval: ${POLL_INTERVAL}s. Sensors: ${#TEMP_SENSORS[@]}."

if [[ "${DISABLE_PCIE_COOLING_RESPONSE:-false}" == "true" ]]; then
  log "Disabling BMC PCIe cooling response."
  ipmi_disable_pcie_cooling
fi

ipmi_set_manual
log "Manual fan control enabled."

while true; do
  sensor_readings=()
  fetch_failed=false
  critical_temp=false

  for sensor_def in "${TEMP_SENSORS[@]}"; do
    IFS=':' read -r name query weight <<< "$sensor_def"

    temp="$(fetch_temp "$query")" || { fetch_failed=true; break; }

    log "  ${name}: ${temp}°C (weight ${weight})"

    if (( $(echo "$temp >= $TEMP_CRITICAL" | bc -l) )); then
      log_warn "Critical temperature on ${name}: ${temp}°C (limit ${TEMP_CRITICAL}°C)"
      critical_temp=true
    fi

    sensor_readings+=( "$name $temp $weight" )
  done

  if $fetch_failed; then
    consecutive_failures=$(( consecutive_failures + 1 ))
    log_warn "Fetch failed (${consecutive_failures}/${MAX_FETCH_FAILURES})."

    if (( consecutive_failures >= MAX_FETCH_FAILURES )) && ! $in_auto_mode; then
      log_warn "Too many failures — handing control back to BMC."
      ipmi_set_auto
      in_auto_mode=true
    fi

    sleep "$POLL_INTERVAL"
    continue
  fi

  consecutive_failures=0

  if $critical_temp; then
    if ! $in_auto_mode; then
      log_warn "Critical temp detected — handing control back to BMC."
      ipmi_set_auto
      in_auto_mode=true
    fi
    sleep "$POLL_INTERVAL"
    continue
  fi

  # Clear auto mode and take back control if we were in fallback
  if $in_auto_mode; then
    log "Temperatures normal — resuming manual control."
    ipmi_set_manual
    in_auto_mode=false
    last_speed=-1  # BMC may have changed speed; force re-apply
  fi

  target="$(compute_fan_speed "${sensor_readings[@]}")"
  if [[ "$target" != "$last_speed" ]]; then
    log "Setting fan speed: ${target}%"
    ipmi_set_speed "$target"
    last_speed="$target"
    printf '%s' "$target" > "$STATE_FILE"
  else
    log "Fan speed unchanged: ${target}%"
  fi

  sleep "$POLL_INTERVAL"
done
