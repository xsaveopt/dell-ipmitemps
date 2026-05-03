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

# URL-encode a string. Prefers python3, falls back to a sed translation
# of the characters that actually appear in PromQL.
_url_encode() {
  python3 -c "import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1]))" "$1" 2>/dev/null \
    || printf '%s' "$1" | sed 's/ /%20/g; s/{/%7B/g; s/}/%7D/g; s/"/%22/g; s/=/%3D/g; s/~/%7E/g; s/,/%2C/g'
}

# Parse a Prometheus-shape JSON response.
# Distinguishes: invalid JSON, API-level error, empty result, non-numeric value.
# Usage: _parse_prom_response "<body>" <value_var> <reason_var>
# On success sets value_var; on failure sets reason_var and returns 1.
_parse_prom_response() {
  local body="$1" __value_var="$2" __reason_var="$3"
  local status err_type err_msg result_count value

  if ! printf '%s' "$body" | jq -e . >/dev/null 2>&1; then
    printf -v "$__reason_var" 'response is not valid JSON (got %d bytes)' "${#body}"
    return 1
  fi

  status="$(printf '%s' "$body" | jq -r '.status // "missing"')"
  if [[ "$status" != "success" ]]; then
    err_type="$(printf '%s' "$body" | jq -r '.errorType // "unknown"')"
    err_msg="$(printf '%s' "$body" | jq -r '.error // "(no message)"')"
    printf -v "$__reason_var" 'API returned status=%s errorType=%s error=%q' \
      "$status" "$err_type" "$err_msg"
    return 1
  fi

  result_count="$(printf '%s' "$body" | jq -r '.data.result | length')"
  if [[ "$result_count" == "0" ]]; then
    printf -v "$__reason_var" 'query matched no series (metric missing or label filters too narrow)'
    return 1
  fi

  value="$(printf '%s' "$body" | jq -r '
    .data.result[0].value[1] //
    .data.result[0].values[-1][1] //
    empty
  ')"

  if [[ -z "$value" ]]; then
    printf -v "$__reason_var" 'series exists but has no sample value'
    return 1
  fi

  if [[ "$value" == "NaN" || "$value" == "+Inf" || "$value" == "-Inf" ]]; then
    printf -v "$__reason_var" 'value is non-numeric (%s) — sensor likely stale or absent' "$value"
    return 1
  fi

  if ! [[ "$value" =~ ^-?[0-9]+(\.[0-9]+)?$ ]]; then
    printf -v "$__reason_var" 'value is not a number: %q' "$value"
    return 1
  fi

  printf -v "$__value_var" '%s' "$value"
}

# HTTP fetch with status-code awareness. Captures body and HTTP code separately
# so 4xx/5xx responses can be reported with their actual status.
# Usage: _http_get <body_var> <code_var> <reason_var> <url> [curl-args...]
# Returns 0 on 2xx, 1 otherwise (with reason set).
_http_get() {
  local __body_var="$1" __code_var="$2" __reason_var="$3" url="$4"
  shift 4
  local tmp curl_exit _code _body
  tmp="$(mktemp)"

  # shellcheck disable=SC2086
  _code="$(curl -s -o "$tmp" -w '%{http_code}' --max-time 10 "$@" "$url")"
  curl_exit=$?
  _body="$(< "$tmp")"
  rm -f "$tmp"

  if (( curl_exit != 0 )); then
    case "$curl_exit" in
      6)  printf -v "$__reason_var" 'DNS resolution failed for %s' "$url" ;;
      7)  printf -v "$__reason_var" 'connection refused / unreachable: %s' "$url" ;;
      28) printf -v "$__reason_var" 'request timed out after 10s: %s' "$url" ;;
      35|60|77) printf -v "$__reason_var" 'TLS error (curl exit %d): %s' "$curl_exit" "$url" ;;
      *)  printf -v "$__reason_var" 'curl failed (exit %d): %s' "$curl_exit" "$url" ;;
    esac
    return 1
  fi

  if [[ "$_code" != 2* ]]; then
    case "$_code" in
      401|403) printf -v "$__reason_var" 'HTTP %s — authentication/authorization rejected' "$_code" ;;
      404)     printf -v "$__reason_var" 'HTTP 404 — endpoint not found (check URL/datasource UID)' ;;
      429)     printf -v "$__reason_var" 'HTTP 429 — rate limited' ;;
      5*)      printf -v "$__reason_var" 'HTTP %s — server error' "$_code" ;;
      *)       printf -v "$__reason_var" 'HTTP %s' "$_code" ;;
    esac
    return 1
  fi

  printf -v "$__body_var" '%s' "$_body"
  printf -v "$__code_var" '%s' "$_code"
}

# Query Prometheus directly via HTTP API.
# Usage: _query_prometheus "promql" <value_var> <reason_var>
_query_prometheus() {
  local query="$1" __value_var="$2" __reason_var="$3"
  local encoded body code
  encoded="$(_url_encode "$query")"

  _http_get body code "$__reason_var" \
    "${PROMETHEUS_URL}/api/v1/query?query=${encoded}" || return 1

  _parse_prom_response "$body" "$__value_var" "$__reason_var"
}

# Query via Grafana datasource proxy.
# Usage: _query_grafana "promql" <value_var> <reason_var>
_query_grafana() {
  local query="$1" __value_var="$2" __reason_var="$3"
  local encoded body code
  encoded="$(_url_encode "$query")"

  _http_get body code "$__reason_var" \
    "${GRAFANA_URL}/api/datasources/proxy/uid/${GRAFANA_DATASOURCE_UID}/api/v1/query?query=${encoded}" \
    -H "Authorization: Bearer ${GRAFANA_TOKEN}" || return 1

  _parse_prom_response "$body" "$__value_var" "$__reason_var"
}

# Public interface: fetch_temp "promql" <value_var> <reason_var>
# Dispatches to the configured backend. On success sets value_var; on failure
# sets reason_var and returns 1.
fetch_temp() {
  local query="$1" __value_var="$2" __reason_var="$3"
  case "$DATASOURCE_TYPE" in
    prometheus) _query_prometheus "$query" "$__value_var" "$__reason_var" ;;
    grafana)    _query_grafana    "$query" "$__value_var" "$__reason_var" ;;
    *)
      printf -v "$__reason_var" 'unknown DATASOURCE_TYPE: %q' "$DATASOURCE_TYPE"
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
  critical_temp=false
  sensors_total="${#TEMP_SENSORS[@]}"
  sensors_failed=0

  for sensor_def in "${TEMP_SENSORS[@]}"; do
    IFS=':' read -r name query weight <<< "$sensor_def"

    temp=""
    fetch_reason=""
    if ! fetch_temp "$query" temp fetch_reason; then
      log_warn "Sensor '${name}' fetch failed: ${fetch_reason}"
      log_warn "  query: ${query}"
      sensors_failed=$(( sensors_failed + 1 ))
      continue
    fi

    log "  ${name}: ${temp}°C (weight ${weight})"

    if (( $(echo "$temp >= $TEMP_CRITICAL" | bc -l) )); then
      log_warn "Critical temperature on ${name}: ${temp}°C (limit ${TEMP_CRITICAL}°C)"
      critical_temp=true
    fi

    sensor_readings+=( "$name $temp $weight" )
  done

  if (( sensors_failed > 0 && sensors_failed < sensors_total )); then
    log_warn "${sensors_failed}/${sensors_total} sensors failed — proceeding with remaining ${#sensor_readings[@]}."
  fi

  if (( sensors_failed == sensors_total )); then
    consecutive_failures=$(( consecutive_failures + 1 ))
    log_warn "Fetch failed (${consecutive_failures}/${MAX_FETCH_FAILURES})."

    if (( consecutive_failures >= MAX_FETCH_FAILURES )) && ! $in_auto_mode; then
      log_warn "Too many failures — handing control back to BMC."
      ipmi_set_auto
      if [[ "${DISABLE_PCIE_COOLING_RESPONSE:-false}" == "true" ]]; then
        ipmi_enable_pcie_cooling
      fi
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
      if [[ "${DISABLE_PCIE_COOLING_RESPONSE:-false}" == "true" ]]; then
        ipmi_enable_pcie_cooling
      fi
      in_auto_mode=true
    fi
    sleep "$POLL_INTERVAL"
    continue
  fi

  # Clear auto mode and take back control if we were in fallback
  if $in_auto_mode; then
    log "Temperatures normal — resuming manual control."
    ipmi_set_manual
    if [[ "${DISABLE_PCIE_COOLING_RESPONSE:-false}" == "true" ]]; then
      ipmi_disable_pcie_cooling
    fi
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
