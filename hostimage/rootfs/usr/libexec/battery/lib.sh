# shellcheck shell=bash disable=SC2034
# Shared by every /usr/libexec/battery script of the Host Image. Sourced,
# never executed. The logic comes from flintlock-runner's Host Image, which
# ported it from scripts that did the same work over SSH on a running
# instance.

set -euo pipefail
umask 022
export PATH=${BATTERY_PATH:-/usr/sbin:/usr/bin}

BATTERY_UNIT=${BATTERY_UNIT:-$(basename "$0")}
BATTERY_RUN=${BATTERY_RUN:-/run/battery}
BATTERY_NOT_READY_DIR=${BATTERY_NOT_READY_DIR:-$BATTERY_RUN/not-ready.d}
# The Host configuration file that cloud-init writes (HI-050), the image's
# defaults for it (HI-051) and the validated result the other units read.
BATTERY_HOST_CONF=${BATTERY_HOST_CONF:-/etc/battery/host.conf}
BATTERY_HOST_DEFAULTS=${BATTERY_HOST_DEFAULTS:-/usr/share/battery/host.conf.defaults}
BATTERY_HOST_ENV=${BATTERY_HOST_ENV:-$BATTERY_RUN/host.env}

# Fixed names. The bridge and the volume group are flintlock's defaults.
# The scripts that source this file use them.
BATTERY_BRIDGE=flbr0
BATTERY_VG=flintlock
BATTERY_METADATA_V4=169.254.169.254
BATTERY_METADATA_V6=fd00:ec2::254
# flintlockd's port, the one the Exec Agent is given
# (--flintlockd=$(HOST_IP):9090). flintlockd serves it on the Host's
# internal address, which battery-network writes into /run/battery/flintlockd.env for
# flintlockd.service (HI-067).
BATTERY_FLINTLOCKD_PORT=9090
# Where the Exec Agent writes flintlockd's serving
# certificate, its key and the client CA bundle, and the names it gives
# them; tls.crt is always written last (--flintlockd-cert-dir and
# internal/execagent/certificates.go there; HI-067, HI-068, HI-071).
BATTERY_FLINTLOCKD_CERT_DIR=${BATTERY_FLINTLOCKD_CERT_DIR:-/etc/battery/flintlockd}
BATTERY_FLINTLOCKD_CERT=tls.crt
BATTERY_FLINTLOCKD_KEY=tls.key
BATTERY_FLINTLOCKD_CLIENT_CA=client-ca.crt

log() { printf '%s: %s\n' "$BATTERY_UNIT" "$*" >&2; }

#= docs/requirements/11-host-image.md#kernel-and-kvm
#/ The Host Image SHALL label the Host environment file and the not ready
#/ reason directory it writes under `/run/battery` so that the containers of
#/ pods on the Host can read them, under the base image's SELinux policy and
#/ without changing the domain of any container or the label of any other
#/ path.
# relabel PATH... gives each PATH the label the policy's file contexts name
# for it (selinux/battery.fc), where SELinux is enabled, and does nothing where
# it is not: the build's check stage and `make host-image-lint`. -F sets the
# whole context, level included, which restorecon otherwise leaves alone on
# the container types; it never recurses.
relabel() {
  if [ -n "${BATTERY_RELABEL:-}" ]; then
    "$BATTERY_RELABEL" "$@"
  elif command -v selinuxenabled >/dev/null 2>&1 && selinuxenabled; then
    restorecon -F "$@"
  fi
}
die() {
  log "error: $*"
  exit 1
}

# The not-ready contract (hostimage/README.md): one file per unit under
# /run/battery/not-ready.d, holding a one-line reason, present exactly while the
# unit's condition holds. The Exec Agent reports each in its
# Host's Node report (EA-033).
not_ready() {
  local tmp
  if [ ! -d "$BATTERY_NOT_READY_DIR" ]; then
    mkdir -p "$BATTERY_NOT_READY_DIR"
    relabel "$BATTERY_NOT_READY_DIR" || log "warning: could not label $BATTERY_NOT_READY_DIR for the Exec Agent"
  fi
  tmp=$(mktemp "$BATTERY_NOT_READY_DIR/.$BATTERY_UNIT.XXXXXX")
  printf '%s\n' "$(printf '%s' "$*" | tr '\n' ' ')" >"$tmp"
  chmod 0644 "$tmp"
  # A rename keeps the label of the file renamed, so the reason is labelled
  # for the Exec Agent before it takes its name (HI-065).
  relabel "$tmp" || log "warning: could not label the reason for the Exec Agent"
  mv -f "$tmp" "$BATTERY_NOT_READY_DIR/$BATTERY_UNIT"
  log "not ready: $*"
}
ready() { rm -f "$BATTERY_NOT_READY_DIR/$BATTERY_UNIT"; }
# refuse REASON records the reason and fails the unit, which keeps every
# unit that requires it, flintlockd above all, from starting.
refuse() {
  not_ready "$*"
  exit 1
}

# ip4_to_int A.B.C.D and int_to_ip4 N convert dotted quads.
ip4_to_int() {
  local a b c d
  IFS=. read -r a b c d <<<"$1"
  echo $(((a << 24) | (b << 16) | (c << 8) | d))
}
int_to_ip4() {
  echo "$((($1 >> 24) & 255)).$((($1 >> 16) & 255)).$((($1 >> 8) & 255)).$(($1 & 255))"
}

# valid_cidr4 CIDR succeeds for an IPv4 prefix with in-range octets.
valid_cidr4() {
  local a b c d p
  [[ $1 =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})/([0-9]{1,2})$ ]] || return 1
  a=${BASH_REMATCH[1]} b=${BASH_REMATCH[2]} c=${BASH_REMATCH[3]} d=${BASH_REMATCH[4]} p=${BASH_REMATCH[5]}
  [ "$((10#$a))" -le 255 ] && [ "$((10#$b))" -le 255 ] && [ "$((10#$c))" -le 255 ] &&
    [ "$((10#$d))" -le 255 ] && [ "$((10#$p))" -le 32 ]
}

# valid_ports LIST succeeds for a comma-separated list of TCP ports, or "".
valid_ports() {
  local p
  [ -z "$1" ] && return 0
  [[ $1 =~ ^[0-9]+(,[0-9]+)*$ ]] || return 1
  for p in ${1//,/ }; do
    if [ "$((10#$p))" -lt 1 ] || [ "$((10#$p))" -gt 65535 ]; then return 1; fi
  done
}

# The seams check-thin-pool-cases uses to run thin-pool against stand-ins:
# where device nodes live and how one is recognised. A Host sets neither.
BATTERY_DEV=${BATTERY_DEV:-/dev}
is_block() {
  if [ -n "${BATTERY_BLOCK_TEST:-}" ]; then
    $BATTERY_BLOCK_TEST "$1"
  else
    [ -b "$1" ]
  fi
}

# load_host_env reads the validated settings battery-host-config wrote.
load_host_env() {
  [ -r "$BATTERY_HOST_ENV" ] || die "$BATTERY_HOST_ENV is missing; battery-host-config.service has not run"
  # shellcheck disable=SC1090
  . "$BATTERY_HOST_ENV"
}
