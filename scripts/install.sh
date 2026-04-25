#!/usr/bin/env bash
# Nimbus installation wizard.
# Hypervisor-host-first: detects Proxmox host, defaults to https://localhost:8006.
# Falls back to prompting for cluster IP/hostname when run outside a PVE host.
#
# Flags:
#   --dry-run      Validate inputs and print what would be done; no files written, no systemd installed.
#   --reconfigure  Re-prompt with existing values pre-filled. Default if env file already exists.
#   --upgrade      Replace the binary only; leave env file and systemd unit alone.
#   -h | --help    Show this help.
set -euo pipefail

# ----------------------------------------------------------------------
# Constants
# ----------------------------------------------------------------------
APP_NAME="nimbus"
SERVICE_USER="$APP_NAME"
INSTALL_DIR="/opt/$APP_NAME"
DATA_DIR="/var/lib/$APP_NAME"
ENV_DIR="/etc/$APP_NAME"
ENV_FILE="$ENV_DIR/${APP_NAME}.env"
SERVICE_FILE="/etc/systemd/system/${APP_NAME}.service"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Default values (pre-filled when reconfiguring)
DEFAULT_PORT="8080"
DEFAULT_POOL_OFFSET_START="100"
DEFAULT_POOL_OFFSET_END="199"
TEMPLATE_VMIDS=(9000 9001 9002 9003)
TEMPLATE_NAMES=("Ubuntu 24.04" "Ubuntu 22.04" "Debian 12" "Debian 11")

# Modes
DRY_RUN=0
RECONFIGURE=0
UPGRADE=0

# ----------------------------------------------------------------------
# Output helpers
# ----------------------------------------------------------------------
if [ -t 1 ]; then
  C_RESET=$'\e[0m'; C_BOLD=$'\e[1m'; C_DIM=$'\e[2m'
  C_GREEN=$'\e[32m'; C_YELLOW=$'\e[33m'; C_RED=$'\e[31m'; C_BLUE=$'\e[34m'
else
  C_RESET=""; C_BOLD=""; C_DIM=""; C_GREEN=""; C_YELLOW=""; C_RED=""; C_BLUE=""
fi

info()  { printf "%s%s%s\n" "$C_BLUE" "$*" "$C_RESET"; }
ok()    { printf "%s✓%s %s\n" "$C_GREEN" "$C_RESET" "$*"; }
warn()  { printf "%s⚠%s %s\n" "$C_YELLOW" "$C_RESET" "$*"; }
err()   { printf "%s✗%s %s\n" "$C_RED" "$C_RESET" "$*" >&2; }
die()   { err "$*"; exit 1; }
header() {
  printf "\n%s%s%s\n" "$C_BOLD" "$*" "$C_RESET"
  printf "%s%s%s\n" "$C_DIM" "$(printf '%.0s-' $(seq 1 ${#1}))" "$C_RESET"
}

# ----------------------------------------------------------------------
# Argument parsing
# ----------------------------------------------------------------------
usage() {
  sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
  exit 0
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run)     DRY_RUN=1; shift ;;
    --reconfigure) RECONFIGURE=1; shift ;;
    --upgrade)     UPGRADE=1; shift ;;
    -h|--help)     usage ;;
    *)             die "Unknown flag: $1 (try --help)" ;;
  esac
done

# ----------------------------------------------------------------------
# Preflight
# ----------------------------------------------------------------------
preflight() {
  header "Preflight"

  if [ "$DRY_RUN" -eq 0 ] && [ "$EUID" -ne 0 ]; then
    die "Must be run as root (sudo). Use --dry-run to test prompts without root."
  fi

  for cmd in curl jq; do
    command -v "$cmd" >/dev/null 2>&1 || die "Required command '$cmd' not found. Install it and retry."
  done

  if [ "$DRY_RUN" -eq 0 ]; then
    command -v systemctl >/dev/null 2>&1 || die "systemctl not found. Nimbus requires systemd."
  fi

  ok "All required commands available"

  # Detect deployment context
  if [ -d /etc/pve ] && [ -f /etc/pve/local/pve-ssl.pem ] 2>/dev/null; then
    DEPLOY_CONTEXT="hypervisor"
    ok "Detected Proxmox VE host (this machine is a cluster node)"
  else
    DEPLOY_CONTEXT="external"
    info "Not on a Proxmox host — Nimbus will run as a standalone service that talks to your cluster."
  fi
}

# ----------------------------------------------------------------------
# Auto-detect helpers
# ----------------------------------------------------------------------
detect_default_host() {
  if [ "$DEPLOY_CONTEXT" = "hypervisor" ]; then
    echo "https://localhost:8006"
  else
    echo ""
  fi
}

detect_default_subnet() {
  # Pull the first global IPv4 address with prefix from default-route interface.
  local iface ip
  iface=$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')
  [ -n "$iface" ] || { echo ""; return; }
  ip=$(ip -4 addr show dev "$iface" 2>/dev/null | awk '/inet / {print $2; exit}')
  echo "$ip"
}

detect_default_gateway() {
  ip -4 route show default 2>/dev/null | awk '/default/ {print $3; exit}'
}

ip_to_int() {
  local IFS=. a b c d
  read -r a b c d <<< "$1"
  echo $(( (a << 24) + (b << 16) + (c << 8) + d ))
}

valid_ipv4() {
  local IFS=. a b c d
  read -r a b c d <<< "$1"
  [[ "$1" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]] || return 1
  for octet in "$a" "$b" "$c" "$d"; do
    [ "$octet" -le 255 ] || return 1
  done
  return 0
}

# ----------------------------------------------------------------------
# Prompts
# ----------------------------------------------------------------------
prompt() {
  local label="$1" default_value="${2:-}" answer=""
  if [ -n "$default_value" ]; then
    read -rp "  $label [$default_value]: " answer
    echo "${answer:-$default_value}"
  else
    read -rp "  $label: " answer
    echo "$answer"
  fi
}

prompt_secret() {
  local label="$1" answer=""
  read -srp "  $label: " answer
  echo >&2
  echo "$answer"
}

# Parse existing env file (if reconfiguring) into shell vars
load_existing_env() {
  if [ -f "$ENV_FILE" ]; then
    # shellcheck disable=SC1090
    set -a; source "$ENV_FILE"; set +a
    return 0
  fi
  return 1
}

collect_inputs() {
  header "Configuration"

  local default_host default_pool_subnet pool_prefix pool_subnet
  default_host="$(detect_default_host)"
  default_pool_subnet="$(detect_default_subnet)"

  PROXMOX_HOST=$(prompt "Proxmox API URL" "${PROXMOX_HOST:-$default_host}")
  [[ "$PROXMOX_HOST" =~ ^https?://[^/]+(:[0-9]+)?$ ]] \
    || die "PROXMOX_HOST must look like https://host:8006 (got: $PROXMOX_HOST)"

  echo
  info "Create an API token in the Proxmox UI: Datacenter > Permissions > API Tokens > Add"
  info "  • User: root@pam (or a dedicated service account)"
  info "  • Token ID: $APP_NAME"
  info "  • UNCHECK 'Privilege Separation' (token inherits user's permissions)"
  echo
  PROXMOX_TOKEN_ID=$(prompt "Token ID (format: user@realm!tokenname)" "${PROXMOX_TOKEN_ID:-root@pam!$APP_NAME}")
  [[ "$PROXMOX_TOKEN_ID" =~ ^[a-zA-Z0-9._-]+@[a-zA-Z0-9._-]+\![a-zA-Z0-9._-]+$ ]] \
    || die "Token ID must be of the form user@realm!tokenname (got: $PROXMOX_TOKEN_ID)"

  if [ -n "${PROXMOX_TOKEN_SECRET:-}" ] && [ "$RECONFIGURE" -eq 1 ]; then
    local keep
    keep=$(prompt "Keep existing token secret? (y/N)" "y")
    if [[ ! "$keep" =~ ^[Yy]$ ]]; then
      PROXMOX_TOKEN_SECRET=$(prompt_secret "Token secret (input hidden)")
    fi
  else
    PROXMOX_TOKEN_SECRET=$(prompt_secret "Token secret (input hidden)")
  fi
  [ -n "$PROXMOX_TOKEN_SECRET" ] || die "Token secret cannot be empty"

  echo
  if [ -n "$default_pool_subnet" ]; then
    pool_prefix="${default_pool_subnet%.*}"
    pool_subnet="${default_pool_subnet#*/}"
  else
    pool_prefix="192.168.0"
    pool_subnet="24"
  fi

  IP_POOL_START=$(prompt "IP pool start" "${IP_POOL_START:-${pool_prefix}.${DEFAULT_POOL_OFFSET_START}}")
  valid_ipv4 "$IP_POOL_START" || die "IP_POOL_START is not a valid IPv4 address: $IP_POOL_START"

  IP_POOL_END=$(prompt "IP pool end" "${IP_POOL_END:-${pool_prefix}.${DEFAULT_POOL_OFFSET_END}}")
  valid_ipv4 "$IP_POOL_END" || die "IP_POOL_END is not a valid IPv4 address: $IP_POOL_END"

  local start_int end_int
  start_int=$(ip_to_int "$IP_POOL_START")
  end_int=$(ip_to_int "$IP_POOL_END")
  [ "$end_int" -ge "$start_int" ] || die "IP_POOL_END must be >= IP_POOL_START"
  local pool_size=$(( end_int - start_int + 1 ))
  if [ "$pool_size" -lt 10 ]; then
    warn "Pool has only $pool_size addresses — consider widening it."
  else
    ok "Pool has $pool_size addresses"
  fi

  GATEWAY_IP=$(prompt "Gateway IP" "${GATEWAY_IP:-$(detect_default_gateway)}")
  valid_ipv4 "$GATEWAY_IP" || die "GATEWAY_IP is not a valid IPv4 address: $GATEWAY_IP"

  PORT=$(prompt "HTTP port for the Nimbus UI" "${PORT:-$DEFAULT_PORT}")
  [[ "$PORT" =~ ^[0-9]+$ ]] && [ "$PORT" -ge 1 ] && [ "$PORT" -le 65535 ] \
    || die "PORT must be a number between 1 and 65535 (got: $PORT)"

  NIMBUS_EXCLUDED_NODES=$(prompt "Excluded node names (comma-separated, optional)" "${NIMBUS_EXCLUDED_NODES:-}")
}

# ----------------------------------------------------------------------
# Validation
# ----------------------------------------------------------------------
pve_curl() {
  local path="$1"
  curl -sk -m 10 \
    -H "Authorization: PVEAPIToken=${PROXMOX_TOKEN_ID}=${PROXMOX_TOKEN_SECRET}" \
    "${PROXMOX_HOST}/api2/json${path}"
}

validate_proxmox() {
  header "Validating Proxmox connectivity"

  local resp version
  resp=$(pve_curl "/version" || true)
  if [ -z "$resp" ]; then
    die "Could not reach $PROXMOX_HOST. Check the URL, network, and that port 8006 is open."
  fi

  version=$(echo "$resp" | jq -r '.data.version // empty' 2>/dev/null || true)
  if [ -z "$version" ]; then
    local errmsg
    errmsg=$(echo "$resp" | jq -r '.errors // .message // .' 2>/dev/null || echo "$resp")
    die "Proxmox API returned no version. Check the token. Response: $errmsg"
  fi
  ok "Proxmox VE $version reachable"

  local nodes_resp nodes_count
  nodes_resp=$(pve_curl "/nodes")
  nodes_count=$(echo "$nodes_resp" | jq '.data | length' 2>/dev/null || echo 0)
  if [ "$nodes_count" -lt 1 ]; then
    die "GET /nodes returned 0 nodes. Token likely lacks permissions — verify Privilege Separation is unchecked."
  fi
  ok "Token has access to $nodes_count node(s)"
  echo "$nodes_resp" | jq -r '.data[] | "    " + .node + " (" + .status + ")"'
}

validate_templates() {
  header "Checking templates on each node"

  local nodes_json
  nodes_json=$(pve_curl "/nodes" | jq -r '.data[].node')

  local i template_vmid template_name found_any nodes_with cloud_init_ok
  for i in "${!TEMPLATE_VMIDS[@]}"; do
    template_vmid="${TEMPLATE_VMIDS[$i]}"
    template_name="${TEMPLATE_NAMES[$i]}"
    nodes_with=()
    cloud_init_ok=()

    while IFS= read -r node; do
      [ -n "$node" ] || continue
      local cfg
      cfg=$(pve_curl "/nodes/${node}/qemu/${template_vmid}/config" 2>/dev/null || true)
      local has_cfg
      has_cfg=$(echo "$cfg" | jq -r 'if .data then "yes" else "no" end' 2>/dev/null || echo "no")
      if [ "$has_cfg" = "yes" ]; then
        nodes_with+=("$node")
        # Look for any drive that contains 'cloudinit' (ide2, scsi*, etc.)
        local has_ci
        has_ci=$(echo "$cfg" | jq -r '.data | to_entries[] | select(.value | type == "string" and contains("cloudinit")) | .key' 2>/dev/null | head -1)
        if [ -n "$has_ci" ]; then
          cloud_init_ok+=("$node")
        fi
      fi
    done <<< "$nodes_json"

    found_any=${#nodes_with[@]}
    if [ "$found_any" -eq 0 ]; then
      warn "$template_name (VMID $template_vmid): not found on any node — this OS will be unavailable"
    elif [ "${#cloud_init_ok[@]}" -lt "$found_any" ]; then
      warn "$template_name (VMID $template_vmid): present on $found_any node(s) but missing cloud-init drive on some — provisioning will fail silently for those"
    else
      ok "$template_name (VMID $template_vmid): present + cloud-init drive on $found_any node(s)"
    fi
  done
}

# ----------------------------------------------------------------------
# Write artifacts
# ----------------------------------------------------------------------
write_env_file() {
  header "Writing $ENV_FILE"

  local content
  content=$(cat <<EOF
# Nimbus configuration — written by scripts/install.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ).
# Edit values here OR re-run 'sudo $APP_NAME-install --reconfigure'.
PROXMOX_HOST=${PROXMOX_HOST}
PROXMOX_TOKEN_ID=${PROXMOX_TOKEN_ID}
PROXMOX_TOKEN_SECRET=${PROXMOX_TOKEN_SECRET}
PROXMOX_TEMPLATE_BASE_VMID=9000
NIMBUS_EXCLUDED_NODES=${NIMBUS_EXCLUDED_NODES}
IP_POOL_START=${IP_POOL_START}
IP_POOL_END=${IP_POOL_END}
GATEWAY_IP=${GATEWAY_IP}
NAMESERVER=1.1.1.1 8.8.8.8
SEARCH_DOMAIN=local
PORT=${PORT}
DB_PATH=${DATA_DIR}/${APP_NAME}.db
EOF
)

  if [ "$DRY_RUN" -eq 1 ]; then
    info "(dry-run) Would write the following to $ENV_FILE (token secret redacted):"
    echo "$content" | sed -E 's/^(PROXMOX_TOKEN_SECRET=).*/\1<REDACTED>/'
    return
  fi

  install -d -m 0750 "$ENV_DIR"
  printf '%s\n' "$content" > "$ENV_FILE"
  chmod 0600 "$ENV_FILE"
  chown root:root "$ENV_FILE"
  ok "$ENV_FILE written (mode 0600)"
}

install_binary() {
  header "Installing binary to $INSTALL_DIR"

  local src="$REPO_ROOT/$APP_NAME"
  if [ ! -x "$src" ]; then
    die "Binary not found at $src. Run ./scripts/build.sh first."
  fi

  if [ "$DRY_RUN" -eq 1 ]; then
    info "(dry-run) Would copy $src -> $INSTALL_DIR/$APP_NAME"
    return
  fi

  install -d -m 0755 "$INSTALL_DIR" "$DATA_DIR"
  install -m 0755 "$src" "$INSTALL_DIR/$APP_NAME"
  useradd -r -s /usr/sbin/nologin "$SERVICE_USER" 2>/dev/null || true
  chown -R "$SERVICE_USER:$SERVICE_USER" "$DATA_DIR"
  ok "Binary installed and owned by $SERVICE_USER"
}

write_systemd_unit() {
  header "Writing systemd unit"

  local content
  content=$(cat <<EOF
[Unit]
Description=Nimbus VM provisioning portal
Documentation=https://github.com/smalex-z/nimbus
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$SERVICE_USER
WorkingDirectory=$DATA_DIR
EnvironmentFile=$ENV_FILE
ExecStart=$INSTALL_DIR/$APP_NAME
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=$DATA_DIR

[Install]
WantedBy=multi-user.target
EOF
)

  if [ "$DRY_RUN" -eq 1 ]; then
    info "(dry-run) Would write the following to $SERVICE_FILE:"
    echo "$content"
    return
  fi

  printf '%s\n' "$content" > "$SERVICE_FILE"
  systemctl daemon-reload
  systemctl enable "$APP_NAME" >/dev/null
  ok "$SERVICE_FILE installed and enabled"
}

start_service() {
  if [ "$DRY_RUN" -eq 1 ]; then
    info "(dry-run) Would start ${APP_NAME}.service"
    return
  fi
  header "Starting $APP_NAME"
  systemctl restart "$APP_NAME"
  sleep 2
  if ! systemctl is-active --quiet "$APP_NAME"; then
    err "Service failed to start. Logs:"
    journalctl -u "$APP_NAME" -n 30 --no-pager
    die "Aborting."
  fi

  local health
  health=$(curl -s -m 5 "http://localhost:${PORT}/api/health" || true)
  if echo "$health" | jq -e '.success == true' >/dev/null 2>&1; then
    ok "Health check passed"
  else
    warn "Service is running but /api/health did not report success: ${health:-(no response)}"
  fi
}

# ----------------------------------------------------------------------
# Main
# ----------------------------------------------------------------------
main() {
  printf "\n%s%s%s\n" "$C_BOLD" "Nimbus Installation Wizard" "$C_RESET"
  printf "%s%s%s\n\n" "$C_DIM" "Self-hosted VM provisioning on Proxmox VE" "$C_RESET"

  preflight

  if [ "$UPGRADE" -eq 1 ]; then
    install_binary
    if [ "$DRY_RUN" -eq 0 ]; then
      systemctl restart "$APP_NAME"
      ok "Upgrade complete"
    fi
    return
  fi

  if load_existing_env && [ "$RECONFIGURE" -eq 0 ] && [ "$DRY_RUN" -eq 0 ]; then
    info "Existing config found at $ENV_FILE — entering --reconfigure mode."
    info "Use --upgrade to replace just the binary without re-prompting."
    RECONFIGURE=1
  fi

  collect_inputs
  validate_proxmox
  validate_templates
  write_env_file
  install_binary
  write_systemd_unit
  start_service

  printf "\n%s%s%s\n" "$C_BOLD$C_GREEN" "Nimbus is installed." "$C_RESET"
  printf "  Portal:  %shttp://%s:%s%s\n" "$C_BOLD" "$(hostname -I 2>/dev/null | awk '{print $1}' || echo localhost)" "$PORT" "$C_RESET"
  printf "  Logs:    journalctl -u %s -f\n" "$APP_NAME"
  printf "  Config:  %s\n" "$ENV_FILE"
  printf "  Status:  systemctl status %s\n\n" "$APP_NAME"
}

main "$@"
