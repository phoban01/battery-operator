#!/usr/bin/env bash
# Checks the SELinux labels of the paths that pods on the Host, the Exec
# Agent among them, mount (HI-065, HI-072):
# that the boot scripts label what they write before it takes its name,
# that tmpfiles.d restores the labels, that the module names
# them and gives no container domain anything, and, where the policy with
# the module installed is at hand, which label each path gets. Installed as
# /usr/libexec/battery/check-selinux-contexts-cases and called by the check
# stage, where the module is installed and SELINUX is left empty;
# `hostimage/check-selinux-contexts.sh DIR LIBEXEC SELINUX TMPFILES DEFAULTS`
# runs it outside the image against the sources, which `make host-image-lint`
# does, and there the label lookups are skipped.
#
# What no build can show is the kernel enforcing the labels: a build
# container has SELinux disabled. That is for the first boot of a Host.
#
#= docs/requirements/11-host-image.md#kernel-and-kvm
#= type=test
#/ The Host Image SHALL label the Host environment file and the not ready
#/ reason directory it writes under `/run/battery` so that the containers of
#/ pods on the Host can read them, under the base image's SELinux policy and
#/ without changing the domain of any container or the label of any other
#/ path.
#
#= docs/requirements/11-host-image.md#kernel-and-kvm
#= type=test
#/ and SHALL label that directory and every file in
#/ it so that the Exec Agent's containers can write them and `flintlockd` can
#/ read them, under the base image's SELinux policy and without changing the
#/ domain of any container.
set -uo pipefail
work=${1:-$(mktemp -d)}
libexec=${2:-/usr/libexec/battery}
selinux=${3:-}
tmpfiles=${4:-/usr/lib/tmpfiles.d/battery.conf}
defaults=${5:-/usr/share/battery/host.conf.defaults}
C=$work/selinux-contexts-cases
failures=0
ok() { printf 'ok    %s\n' "$*"; }
fail() {
  printf 'FAIL  %s\n' "$*"
  failures=$((failures + 1))
}
skip() { printf 'skip  %s\n' "$*"; }
rm -rf "$C"
mkdir -p "$C/run"

# A stand-in for restorecon: it records each path it is asked to label and
# whether the name the file will take already holds this run's content.
cat >"$C/relabel" <<'EOF'
#!/usr/bin/env bash
for p in "$@"; do
  state=new
  grep -qs "^BATTERY_GUEST_SUBNET=10.99.0.0/16$" "$BATTERY_HOST_ENV" && state=replaced
  printf '%s %s\n' "$p" "$state" >>"$RELABEL_LOG"
done
exit "${RELABEL_EXIT:-0}"
EOF
chmod +x "$C/relabel"
env_for() {
  printf '%s\n' BATTERY_PATH="$PATH" BATTERY_LIB="$libexec/lib.sh" BATTERY_RELABEL="$C/relabel" RELABEL_LOG="$C/relabel.log" \
    BATTERY_HOST_DEFAULTS="$defaults" BATTERY_HOST_CONF="$C/host.conf" \
    BATTERY_RUN="$C/run" BATTERY_NOT_READY_DIR="$C/run/not-ready.d" BATTERY_HOST_ENV="$C/run/host.env"
}
run() {
  local vars
  mapfile -t vars < <(env_for)
  env "${vars[@]}" "$@" >"$C/out" 2>&1
}

# host-config labels host.env's temporary file, and only it, before the
# rename that makes it host.env.
printf 'GUEST_SUBNET=10.99.0.0/16\n' >"$C/host.conf"
: >"$C/relabel.log"
if run bash "$libexec/host-config"; then
  log=$(cat "$C/relabel.log")
  if [[ $log =~ ^$C/run/\.host\.env\.[^/]+\ new$ ]]; then
    ok "host-config labels host.env before it takes its name, not after"
  else
    fail "host-config labelled: $log"
  fi
else
  fail "host-config fails: $(cat "$C/out")"
fi
: >"$C/relabel.log"
if RELABEL_EXIT=1 run bash "$libexec/host-config"; then
  fail "host-config succeeds when host.env cannot be labelled"
elif grep -qs '^host configuration invalid\|^cannot label' "$C/run/not-ready.d/battery-host-config"; then
  ok "host-config fails, and says why, when host.env cannot be labelled"
else
  fail "host-config fails without a reason when host.env cannot be labelled"
fi

# A not ready reason is labelled before it takes its name too, and the
# directory when the unit has to make it.
rm -rf "$C/run/not-ready.d"
: >"$C/relabel.log"
run env BATTERY_KVM_DEVICE="$C/no-kvm" bash "$libexec/kvm-gate"
if grep -qx "$C/run/not-ready.d [a-z]*" "$C/relabel.log" && grep -Eq "^$C/run/not-ready\.d/\.battery-kvm\.[^/ ]+ " "$C/relabel.log" &&
  [ -e "$C/run/not-ready.d/battery-kvm" ] && ! grep -q "^$C/run/not-ready.d/battery-kvm " "$C/relabel.log"; then
  ok "a not ready reason, and the directory it made, is labelled before the reason takes its name"
else
  fail "the not ready reason was labelled: $(cat "$C/relabel.log")"
fi

# tmpfiles.d restores each label, without recursing, whenever it runs.
for p in /run/battery/not-ready.d /run/battery/host.env; do
  if grep -Eq "^z $p - - - -$" "$tmpfiles"; then ok "tmpfiles.d restores the label of $p"; else fail "tmpfiles.d has no z line for $p"; fi
done

# The module gives no domain anything new beyond what HI-013 gives dnsmasq,
# and makes nothing permissive or unconfined.
te=${selinux:+$selinux/battery.te}
if [ -n "$te" ]; then
  rules=$(grep -Ev '^[[:space:]]*(#|$)' "$te" | grep -E '^[[:space:]]*(allow|auditallow|dontaudit|type_transition|range_transition|typeattribute|permissive|unconfined|domain_|container_|kernel_|corecmd_)' || true)
  if [ "$rules" = "$(printf '%s\n' 'allow dnsmasq_t battery_run_t:dir { getattr search open read };' 'allow dnsmasq_t battery_run_t:file { getattr open read ioctl lock };')" ]; then
    ok "the module's only rules are dnsmasq_t's two reads of battery_run_t; it has none for a container domain"
  else
    fail "the module has other rules: $rules"
  fi
fi

# What each path's label is, from the installed policy. The paths pods on
# the Host mount take the container types at s0; everything else under
# /run/battery stays battery_run_t, and /etc/battery, with the Host
# configuration file in it, keeps the base policy's label.
lookup() { matchpathcon -n "$1" 2>/dev/null; }
if ! command -v matchpathcon >/dev/null 2>&1; then
  skip "labels: matchpathcon is not installed"
elif ! semodule -l 2>/dev/null | grep -qx battery; then
  skip "labels: the battery module is not installed here"
else
  ro=system_u:object_r:container_ro_file_t:s0
  rw=system_u:object_r:container_file_t:s0
  run_t=system_u:object_r:battery_run_t:s0
  for pl in /run/battery/host.env=$ro /run/battery/.host.env.Ab12Cd=$ro /var/run/battery/host.env=$ro \
    /run/battery/not-ready.d=$ro /run/battery/not-ready.d/battery-kvm=$ro /run/battery/not-ready.d/.battery-kvm.Ab12Cd=$ro \
    /run/battery=$run_t /run/battery/dnsmasq.conf=$run_t /run/battery/guest-firewall.nft=$run_t /run/battery/kubelet.env=$run_t \
    /run/battery/host.env.bak=$run_t /etc/battery/host.conf=system_u:object_r:etc_t:s0 \
    /etc/battery/flintlockd=$rw /etc/battery/flintlockd/tls.crt=$rw /etc/battery/flintlockd/tls.key=$rw \
    /etc/battery/flintlockd/client-ca.crt=$rw /etc/battery/flintlockd/.tls.crt.123456=$rw /etc/battery=system_u:object_r:etc_t:s0; do
    p=${pl%%=*} want=${pl#*=}
    got=$(lookup "$p")
    if [ "$got" = "$want" ]; then ok "$p is labelled $want"; else fail "$p is labelled '$got', want $want"; fi
  done
fi

rm -rf "$C"
[ "$failures" -eq 0 ]
