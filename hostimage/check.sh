#!/usr/bin/bash
# The Host Image's check stage. It runs inside the built container image, as
# /usr/libexec/battery/check, from the Containerfile's check stage and from
# `make host-image-check`, and needs neither systemd running, nor KVM, nor a
# block device: it inspects the image and runs the boot scripts against
# temporary directories. Every failure is reported before it exits non-zero.
#
#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image build SHALL run a check stage inside the built
#/ container image that fails unless every component of HI-003 reports its
#/ pinned version and every unit this document requires is enabled.
# The patterns below are literal on purpose.
# shellcheck disable=SC2016
set -uo pipefail
export PATH=/usr/sbin:/usr/bin

VERSIONS=${BATTERY_VERSIONS:-/usr/share/battery/versions.env}
UNITS=/usr/lib/systemd/system
LIBEXEC=/usr/libexec/battery
failures=0
ok() { printf 'ok    %s\n' "$*"; }
fail() {
  printf 'FAIL  %s\n' "$*"
  failures=$((failures + 1))
}
skip() { printf 'skip  %s\n' "$*"; }
# expect DESCRIPTION COMMAND...: the command has to succeed.
expect() {
  local what=$1
  shift
  if "$@" >/dev/null 2>&1; then ok "$what"; else fail "$what"; fi
}
# refute DESCRIPTION COMMAND...: the command has to fail.
refute() {
  local what=$1
  shift
  if "$@" >/dev/null 2>&1; then fail "$what"; else ok "$what"; fi
}
has() { grep -qE -- "$2" "$1"; }
# code_has PATTERN FILE...: the pattern occurs outside a comment.
code_has() {
  local pattern=$1
  shift
  cat "$@" 2>/dev/null | grep -v "^[[:space:]]*#" | grep -qE -- "$pattern"
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=hostimage/versions.env
. "$VERSIONS" || {
  echo "FAIL  $VERSIONS is missing"
  exit 1
}

# ---------------------------------------------------------------------------
echo "== build"

#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image SHALL be defined by one Containerfile whose base
#/ is a bootc base image pinned by digest.
# The running image is a bootc image: bootc and its ostree layout are here,
# and `bootc container lint` passed in the build. That the base is pinned by
# digest is checked on the Containerfile by `make host-image-lint` in CI.
expect "bootc is installed" command -v bootc
expect "the image has the ostree layout of a bootc image" test -d /sysroot/ostree

#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image SHALL be built for the `x86_64` and `aarch64`
#/ architectures.
# The image's architecture is that of the base image's own bash, which the
# builder chose from the base's multi-architecture index. It has to be one
# of the two, and every pinned binary has to be built for it: a download for
# the wrong architecture fails here. The ELF machine field is read from the
# file, so no emulation layer can answer for it.
elf_machine() {
  case "$(od -An -tx1 -j18 -N2 "$(readlink -f "$1")" 2>/dev/null | tr -d ' \n')" in
  3e00) echo x86_64 ;;
  b700) echo aarch64 ;;
  '') echo "not a file" ;;
  *) echo other ;;
  esac
}
arch=$(elf_machine /usr/bin/bash)
case "$arch" in
x86_64 | aarch64) ok "the image is built for $arch" ;;
*) fail "the image's /usr/bin/bash is $arch, neither x86_64 nor aarch64" ;;
esac
for b in containerd containerd-shim-runc-v2 ctr runc firecracker jailer cloud-hypervisor-static flintlockd kubelet kubeadm; do
  got=$(elf_machine "/usr/bin/$b")
  if [ "$got" = "$arch" ]; then
    ok "$b is an $arch binary"
  else
    fail "$b is $got, not an $arch binary"
  fi
done

#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image SHALL contain containerd, Firecracker with its
#/ jailer, Cloud Hypervisor, `flintlockd`, the kubelet, `kubeadm` and
#/ cloud-init at the versions pinned in one versions file that the
#/ Containerfile reads.
# reports NAME PINNED COMMAND...: the first version-looking token of the
# command's output has to equal the pinned version, "v" prefix or not.
reports() {
  local name=$1 want=${2#v} out got
  shift 2
  out=$("$@" 2>&1 | head -n 5)
  got=$(printf '%s\n' "$out" | grep -Eo 'v?[0-9]+\.[0-9]+(\.[0-9]+)?[0-9A-Za-z.+-]*' | head -n1)
  got=${got#v}
  # cloud-hypervisor reports one more component than its tag: v48.0 is 48.0.0.
  if [ -n "$got" ] && { [ "$got" = "$want" ] || [ "$got" = "$want.0" ]; }; then
    ok "$name reports $got, pinned ${want}"
  elif printf '%s' "$out" | grep -q 'Function not implemented' && [ -n "${BATTERY_CHECK_BINARY:-}" ] &&
    grep -aqF -- "$want" "$BATTERY_CHECK_BINARY"; then
    # User-mode emulation lacks system calls some binaries make before they
    # print anything (the jailer calls close_range). The binary could not
    # speak, so its embedded version string is read instead. This says less
    # than running it, and says so; on a kernel of the image's own
    # architecture the branch is not reached.
    ok "$name embeds $want (could not run here: $(printf '%s' "$out" | grep -m1 -o 'Failed to call.*' | cut -c1-80))"
  else
    fail "$name reports '${got:-nothing}', pinned $want: $out"
  fi
}
reports containerd "$CONTAINERD_VERSION" sh -c "containerd --version | awk '{print \$3}'"
# runc is asked under another name. An emulation layer that runs this image
# on another architecture may answer for a binary called "runc" with
# the machine's own native runc (OrbStack does), and the check has to hear
# from the bytes that were pinned, on every machine.
cp /usr/bin/runc "$work/runc-under-test"
reports runc "$RUNC_VERSION" "$work/runc-under-test" --version
reports firecracker "$FIRECRACKER_VERSION" firecracker --version
BATTERY_CHECK_BINARY=/usr/bin/jailer reports jailer "$FIRECRACKER_VERSION" jailer --version
reports cloud-hypervisor "$CLOUD_HYPERVISOR_VERSION" cloud-hypervisor-static --version
reports flintlockd "$FLINTLOCK_VERSION" flintlockd version --short
reports kubelet "$KUBERNETES_VERSION" kubelet --version
reports kubeadm "$KUBERNETES_VERSION" kubeadm version -o short
reports cloud-init "$CLOUD_INIT_VERSION" cloud-init --version
expect "containerd has the devmapper snapshotter built in" \
  sh -c "containerd config default | grep -q 'io.containerd.snapshotter.v1.devmapper'"

#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image build SHALL verify the checksum of every binary
#/ it downloads against a checksum recorded in the versions file and SHALL
#/ fail when one does not match.
# The fetch stage has already refused any mismatch, or this image would not
# exist. What is checked here is that no download can escape it: every
# pinned download has a well-formed checksum beside its version, for each
# architecture the image is built for.
for k in CONTAINERD RUNC FIRECRACKER CLOUD_HYPERVISOR FLINTLOCK; do
  for a in AMD64 ARM64; do
    sum=${k}_SHA256_$a
    if [[ ${!sum:-} =~ ^[0-9a-f]{64}$ ]]; then ok "$sum is a sha256"; else fail "$sum is not a sha256"; fi
  done
done
if [[ ${KUBERNETES_REPO_KEY_SHA256:-} =~ ^[0-9a-f]{64}$ ]] &&
  echo "$KUBERNETES_REPO_KEY_SHA256  /etc/pki/rpm-gpg/RPM-GPG-KEY-kubernetes" | sha256sum -c - >/dev/null 2>&1; then
  ok "the Kubernetes repository key matches its pinned checksum"
else
  fail "the Kubernetes repository key does not match its pinned checksum"
fi
expect "the Kubernetes repository verifies packages" has /etc/yum.repos.d/kubernetes.repo '^gpgcheck=1$'

#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image SHALL record the pinned version of each component
#/ of HI-003 as an OCI image label and in a versions file on the image's
#/ `/usr` tree.
# The versions file half. The labels are outside the filesystem; `make
# image-check` compares them with this file after the build.
for k in CONTAINERD_VERSION RUNC_VERSION FIRECRACKER_VERSION CLOUD_HYPERVISOR_VERSION FLINTLOCK_VERSION KUBERNETES_VERSION CLOUD_INIT_VERSION; do
  if [ -n "${!k:-}" ]; then ok "$VERSIONS pins $k=${!k}"; else fail "$VERSIONS pins no $k"; fi
done

#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image SHALL NOT contain any credential, private key or
#/ token.
leaks=$(grep -rIl -e 'PRIVATE KE[Y]-----' /etc /root /var /home /usr/share/battery /usr/libexec/battery "$UNITS" 2>/dev/null | head -n 20)
if [ -z "$leaks" ]; then ok "no private key under /etc, /var, /root, /home or the image's own files"; else fail "private keys: $leaks"; fi
for p in /etc/ssh/ssh_host_*_key /etc/kubernetes/*.conf /etc/kubernetes/pki /var/lib/kubelet/pki \
  /var/lib/cloud/instance /root/.ssh /root/.aws /root/.kube /root/.docker /etc/battery/host.conf; do
  if [ -e "$p" ]; then fail "$p exists in the image"; else ok "no $p"; fi
done
if [ -s /etc/machine-id ] && [ "$(cat /etc/machine-id)" != uninitialized ]; then
  fail "/etc/machine-id is set"
else
  ok "/etc/machine-id is unset"
fi
refute "flintlockd is given no token" grep -Eq -- '--basic-auth-token' "$UNITS/flintlockd.service"
# flintlockd's key is a file the Exec Agent writes on the Host, never one
# the image carries.
expect "flintlockd's key is the one the Exec Agent writes on the Host" has "$UNITS/flintlockd.service" '--tls-key /etc/battery/flintlockd/tls\.key '
refute "the image carries nothing in /etc/battery/flintlockd" sh -c 'ls -A /etc/battery/flintlockd 2>/dev/null | grep -q .'
refute "root has no password" sh -c "awk -F: '\$1 == \"root\" && \$2 !~ /^[!*]/ && \$2 != \"\" { found = 1 } END { exit !found }' /etc/shadow"

#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image build SHALL complete on a machine that has neither
#/ `/dev/kvm` nor credentials for any cloud provider.
# This stage is part of the build, and CI runs it on a hosted runner with
# neither. It records what it found rather than requiring their absence.
if [ -e /dev/kvm ]; then skip "/dev/kvm exists here; the build did not use it"; else ok "built and checked without /dev/kvm"; fi
refute "no AWS command line or credentials are needed or present" sh -c 'command -v aws || test -e /root/.aws'

# ---------------------------------------------------------------------------
echo "== units"
enabled() {
  local state
  state=$(systemctl is-enabled "$1" 2>/dev/null)
  case "$state" in
  enabled | enabled-runtime | static | alias | indirect) ok "$1 is $state" ;;
  *) fail "$1 is ${state:-missing}, want enabled" ;;
  esac
}
for u in battery-host-config battery-kvm battery-thin-pool battery-network battery-dnsmasq battery-kubelet-config \
  battery-flintlockd-certs containerd flintlockd kubelet cloud-init-local cloud-config cloud-final; do
  enabled "$u.service"
done
# flintlockd itself is started by its path unit once its certificates
# exist (HI-071), so it is static and the path units are enabled.
for u in flintlockd.path battery-flintlockd-restart.path; do
  enabled "$u"
done
if [ "$(systemctl is-enabled flintlockd.service 2>/dev/null)" = static ]; then ok "flintlockd.service starts only when flintlockd.path starts it"; else fail "flintlockd.service is $(systemctl is-enabled flintlockd.service 2>/dev/null), want static"; fi
if [ -e "$UNITS/cloud-init-network.service" ]; then enabled cloud-init-network.service; else enabled cloud-init.service; fi
refute "the distribution's dnsmasq.service is not enabled" systemctl is-enabled dnsmasq.service
for s in host-config kvm-gate thin-pool network kubelet-config flintlockd-certs; do
  expect "$LIBEXEC/$s is executable and parses" sh -c "test -x $LIBEXEC/$s && bash -n $LIBEXEC/$s"
done
if command -v systemd-analyze >/dev/null 2>&1; then
  out=$(systemd-analyze verify --man=no "$UNITS"/battery-*.service "$UNITS"/battery-*.path "$UNITS/flintlockd.service" "$UNITS/flintlockd.path" "$UNITS/containerd.service" 2>&1 |
    grep -E 'battery-|flintlockd|containerd|20-battery' || true)
  if [ -z "$out" ]; then ok "systemd-analyze verify has nothing to say about the image's units"; else fail "systemd-analyze verify: $out"; fi
else
  skip "systemd-analyze is not installed"
fi

# ---------------------------------------------------------------------------
echo "== kernel and KVM"

#= docs/requirements/11-host-image.md#kernel-and-kvm
#= type=test
#/ The Host Image SHALL load the `kvm`, `vhost_vsock`,
#/ `dm_thin_pool`, `tun` and `bridge` kernel modules at boot.
kernel=$(find /usr/lib/modules -mindepth 1 -maxdepth 1 -type d | head -n1)
for m in kvm vhost_vsock dm_thin_pool tun bridge; do
  expect "modules-load.d loads $m" has /usr/lib/modules-load.d/battery.conf "^$m\$"
  file_name=${m//_/[_-]}
  if find "$kernel" -name "$file_name.ko*" 2>/dev/null | grep -q . ||
    grep -qE "/${file_name}\.ko" "$kernel/modules.builtin" 2>/dev/null; then
    ok "the image's kernel ships $m"
  else
    fail "the image's kernel $kernel has no $m module"
  fi
done

#= docs/requirements/11-host-image.md#kernel-and-kvm
#= type=test
#/ If `/dev/kvm` is absent or unusable at boot, then the Host Image
#/ SHALL NOT start `flintlockd` and SHALL report the Host as not ready with
#/ the reason that KVM is unavailable.
export BATTERY_RUN=$work/run BATTERY_NOT_READY_DIR=$work/run/not-ready.d
mkdir -p "$BATTERY_NOT_READY_DIR"
refute "the KVM gate fails without a KVM device" env BATTERY_KVM_DEVICE="$work/no-kvm" "$LIBEXEC/kvm-gate"
expect "the KVM gate records that KVM is unavailable" has "$BATTERY_NOT_READY_DIR/battery-kvm" '^KVM is unavailable: .* does not exist$'
: >"$work/not-a-device"
refute "the KVM gate fails on a KVM node that is no device" env BATTERY_KVM_DEVICE="$work/not-a-device" "$LIBEXEC/kvm-gate"
expect "the KVM gate says why" has "$BATTERY_NOT_READY_DIR/battery-kvm" 'is not a character device$'
expect "the KVM gate passes a usable device and clears the reason" \
  sh -c "BATTERY_KVM_DEVICE=/dev/null $LIBEXEC/kvm-gate && ! test -e $BATTERY_NOT_READY_DIR/battery-kvm"
expect "flintlockd requires the KVM gate" has "$UNITS/flintlockd.service" '^Requires=.*battery-kvm\.service'
expect "flintlockd starts after the KVM gate" has "$UNITS/flintlockd.service" '^After=.*battery-kvm\.service'

#= docs/requirements/11-host-image.md#kernel-and-kvm
#= type=test
#/ The Host Image SHALL NOT disable SELinux globally.
expect "SELinux is configured enforcing" has /etc/selinux/config '^SELINUX=enforcing$'
refute "no kernel argument turns SELinux off" \
  sh -c "grep -rqsE '(selinux=0|enforcing=0)' /usr/lib/bootc/kargs.d /etc/default/grub /usr/lib/bootupd 2>/dev/null"
refute "nothing in the image calls setenforce" code_has setenforce "$LIBEXEC"/[!c]* "$UNITS"/battery-*.service /etc/cloud/cloud.cfg.d/90-battery.cfg

#= docs/requirements/11-host-image.md#kernel-and-kvm
#= type=test
#/ Where a component cannot run under the base image's SELinux
#/ policy, the Host Image SHALL ship a policy module that relaxes confinement
#/ for that component's domain only.
if semodule -l 2>/dev/null | grep -qx battery; then ok "the battery policy module is installed"; else fail "the battery policy module is not installed"; fi
refute "no domain is made permissive by the image" sh -c "semodule -l 2>/dev/null | grep -q '^permissive_'"
if command -v sesearch >/dev/null 2>&1; then
  expect "dnsmasq_t may read battery_run_t" sh -c "sesearch -A -s dnsmasq_t -t battery_run_t -c file -p read | grep -q allow"
else
  skip "sesearch is not installed; the module's rules are not inspected"
fi

#= docs/requirements/11-host-image.md#kernel-and-kvm
#= type=test
#/ The Host Image SHALL label the Host environment file and the not ready
#/ reason directory it writes under `/run/battery` so that the containers of
#/ pods on the Host can read them, under the base image's SELinux policy and
#/ without changing the domain of any container or the label of any other
#/ path.
# The label each path takes from the policy the module was installed into,
# and that the boot scripts label what they write before it takes its name.
# That container_t may read container_ro_file_t and write container_file_t
# is container-selinux's, and is inspected where sesearch is installed.
if "$LIBEXEC/check-selinux-contexts-cases" "$work"; then
  ok "SELinux context cases (labels of the paths pods on the Host mount, labelled before rename)"
else
  fail "SELinux context cases"
fi
expect "container-selinux is installed" rpm -q container-selinux

#= docs/requirements/11-host-image.md#kernel-and-kvm
#= type=test
#/ The Host Image SHALL configure the container runtime interface
#/ of containerd to run every unprivileged container of a Kubernetes pod under
#/ SELinux confinement, in the container domain the base image's policy
#/ assigns or the one the pod names, rather than unconfined.
# The effective value, as the pinned containerd reads its configuration:
# `config dump` merges the file over the defaults without starting the
# daemon. Where it cannot run, the file itself is read.
# cri_selinux prints enable_selinux of containerd v2's CRI runtime plugin
# table. The file quotes the plugin's name with double quotes, and
# `config dump` with single ones.
cri_selinux() {
  awk '/^\[/ { t = $0 } t ~ /^\[plugins\.["'\'']io\.containerd\.cri\.v1\.runtime["'\'']\]$/ && $1 == "enable_selinux" { print $3 }' "$1"
}
expect "containerd's configuration is in containerd v2's format, version 3" has /etc/containerd/config.toml '^version = 3$'
if containerd --config /etc/containerd/config.toml config dump >"$work/containerd-dump.toml" 2>"$work/containerd-dump.err"; then
  sed -i 's/^[[:space:]]*//' "$work/containerd-dump.toml"
  got=$(cri_selinux "$work/containerd-dump.toml")
  if [ "$got" = true ]; then ok "containerd's effective CRI configuration has enable_selinux = true"; else fail "containerd's effective CRI enable_selinux is '$got'"; fi
else
  skip "containerd config dump cannot run here: $(head -n1 "$work/containerd-dump.err")"
  sed 's/^[[:space:]]*//' /etc/containerd/config.toml >"$work/containerd-file.toml"
  got=$(cri_selinux "$work/containerd-file.toml")
  if [ "$got" = true ]; then ok "/etc/containerd/config.toml sets enable_selinux = true for the CRI plugin"; else fail "/etc/containerd/config.toml sets CRI enable_selinux to '$got'"; fi
fi
# The container domain comes from the base policy's container contexts,
# which containerd reads through go-selinux; nothing in the image overrides
# them.
expect "the base policy assigns containers container_t" has /etc/selinux/targeted/contexts/lxc_contexts '^process = "system_u:system_r:container_t:s0"$'
if command -v sesearch >/dev/null 2>&1; then
  expect "container_t may read container_ro_file_t" sh -c "sesearch -A -s container_t -t container_ro_file_t -c file -p read | grep -q allow"
  expect "container_t may write container_file_t" sh -c "sesearch -A -s container_t -t container_file_t -c file -p write | grep -q allow"
  refute "container_t may not write container_ro_file_t" sh -c "sesearch -A -s container_t -t container_ro_file_t -c file -p write | grep -q allow"
fi

# ---------------------------------------------------------------------------
echo "== Host configuration"
export BATTERY_HOST_ENV=$work/run/host.env
host_config() { BATTERY_HOST_CONF=$1 "$LIBEXEC/host-config"; }

#= docs/requirements/11-host-image.md#host-configuration
#= type=test
#/ If the Host configuration file is absent, then the Host Image
#/ SHALL boot with its defaults for every setting.
expect "host-config succeeds with no Host configuration file" host_config "$work/absent.conf"
for kv in BATTERY_GUEST_SUBNET=10.220.0.0/16 BATTERY_GATEWAY=10.220.0.1 BATTERY_PREFIX=16 BATTERY_NETMASK=255.255.0.0 \
  BATTERY_DHCP_START=10.220.0.10 BATTERY_DHCP_END=10.220.255.254 BATTERY_THIN_POOL_DEVICE= BATTERY_PROTECTED_CIDRS= \
  BATTERY_HOST_RESERVE_VCPU=2 BATTERY_HOST_RESERVE_MEMORY_MB=4096 \
  BATTERY_HOST_CONTROL_PORTS=9090,8090,10248,10250,10255,10256,10270,1338 \
  BATTERY_EXEC_AGENT_UID=65532 BATTERY_FLINTLOCKD_CLIENT_CIDRS= \
  BATTERY_GATEWAY_SERVICE_PORTS= BATTERY_GATEWAY_SERVICE_UIDS=; do
  expect "default $kv" grep -qxF -- "$kv" "$BATTERY_HOST_ENV"
done

#= docs/requirements/11-host-image.md#host-configuration
#= type=test
#/ The Host Image SHALL read per-Host settings from one Host
#/ configuration file that cloud-init writes at first boot, containing the
#/ guest subnet, the thin pool device, the protected CIDRs and the Host
#/ reserve.
cat >"$work/host.conf" <<'EOF'
# written by cloud-init
GUEST_SUBNET = "10.200.4.0/22"
THIN_POOL_DEVICE=/dev/nvme1n1
PROTECTED_CIDRS=10.0.0.0/16, 192.168.0.0/16
HOST_RESERVE_VCPU=4
HOST_RESERVE_MEMORY_MB=8192
JOIN_TOKEN=abcdef.0123456789abcdef
EOF
expect "host-config reads a Host configuration file" host_config "$work/host.conf"
for kv in BATTERY_GUEST_SUBNET=10.200.4.0/22 BATTERY_GATEWAY=10.200.4.1 BATTERY_NETMASK=255.255.252.0 \
  BATTERY_DHCP_START=10.200.4.10 BATTERY_DHCP_END=10.200.7.254 BATTERY_THIN_POOL_DEVICE=/dev/nvme1n1 \
  BATTERY_PROTECTED_CIDRS=10.0.0.0/16,192.168.0.0/16 BATTERY_HOST_RESERVE_VCPU=4 BATTERY_HOST_RESERVE_MEMORY_MB=8192; do
  expect "configured $kv" grep -qxF -- "$kv" "$BATTERY_HOST_ENV"
done
expect "cloud-init is told where the file goes" has /etc/cloud/cloud.cfg.d/90-battery.cfg '/etc/battery/host.conf'
expect "host-config waits for cloud-init's write_files stage" has "$UNITS/battery-host-config.service" '^After=.*cloud-init-network\.service'

#= docs/requirements/11-host-image.md#host-configuration
#= type=test
#/ The Host Image SHALL NOT read any secret from the Host
#/ configuration file or from instance user-data.
refute "a key the image does not know never reaches the other units" grep -rq -e JOIN_TOKEN -e abcdef "$work/run"
refute "nothing in the image reads instance user-data" \
  code_has 'user-data|user_data|169\.254\.169\.254/' "$LIBEXEC"/[!c]* "$UNITS"/battery-*.service
printf 'GUEST_SUBNET=not-a-subnet\n' >"$work/bad.conf"
refute "host-config refuses an invalid guest subnet" host_config "$work/bad.conf"
expect "and records why" has "$BATTERY_NOT_READY_DIR/battery-host-config" '^host configuration invalid: GUEST_SUBNET'
printf 'PROTECTED_CIDRS=10.0.0.0/16;reboot\n' >"$work/bad.conf"
refute "host-config refuses a value that is not a CIDR list" host_config "$work/bad.conf"
host_config "$work/host.conf" >/dev/null 2>&1
refute "a valid configuration clears the reason" test -e "$BATTERY_NOT_READY_DIR/battery-host-config"

# ---------------------------------------------------------------------------
echo "== storage"

#= docs/requirements/11-host-image.md#image-storage
#= type=test
#/ If no device is named and none is detected, then the Host Image
#/ SHALL NOT start `flintlockd` and SHALL report the Host as not ready with
#/ the reason that no thin pool device is available.
# The build container has no instance-store disk, which is the condition.
host_config "$work/absent.conf" >/dev/null 2>&1
if lsblk -dno MODEL 2>/dev/null | grep -q 'Instance Storage'; then
  skip "this machine has an instance-store disk; the no-device case cannot be run"
else
  refute "thin-pool fails when no device is named and none is detected" "$LIBEXEC/thin-pool"
  expect "thin-pool records that no thin pool device is available" \
    has "$BATTERY_NOT_READY_DIR/battery-thin-pool" '^no thin pool device is available: none is named'
fi
expect "flintlockd requires the thin pool unit" has "$UNITS/flintlockd.service" '^Requires=.*battery-thin-pool\.service'

#= docs/requirements/11-host-image.md#image-storage
#= type=test
#/ The Host Image SHALL create the containerd devicemapper thin
#/ pool at boot on the block device named in the Host configuration file, or,
#/ when none is named, on the unused instance-store device it detects.
printf 'THIN_POOL_DEVICE=%s\n' "$work/no-such-device" >"$work/named.conf"
refute "host-config refuses a thin pool device outside /dev" host_config "$work/named.conf"
printf 'THIN_POOL_DEVICE=/dev/battery-check-no-such-device\n' >"$work/named.conf"
host_config "$work/named.conf" >/dev/null 2>&1
refute "thin-pool fails when the named device is no block device" \
  env BATTERY_DEVICE_WAIT=1 "$LIBEXEC/thin-pool"
expect "and names the device in the reason" has "$BATTERY_NOT_READY_DIR/battery-thin-pool" '/dev/battery-check-no-such-device, named in the Host configuration'
expect "containerd's devmapper snapshotter uses the pool the unit creates" \
  has /etc/containerd/config.toml '^ *pool_name = "flintlock-thinpool"$'
expect "thin-pool detects instance-store disks by model" has "$LIBEXEC/thin-pool" "INSTANCE_STORE_MODEL='Instance Storage'"
expect "the thin pool is ready before containerd starts" has "$UNITS/battery-thin-pool.service" '^Before=containerd\.service'

#= docs/requirements/11-host-image.md#image-storage
#= type=test
#/ If the thin pool already exists at boot, then the Host Image
#/ SHALL NOT recreate it or wipe its device.
#
#= docs/requirements/11-host-image.md#image-storage
#= type=test
#/ The Host Image SHALL NOT select a device that holds a mounted
#/ filesystem or the root volume, and SHALL treat a device that already backs
#/ the thin pool as the thin pool device rather than as in use.
# Both are run against stand-ins for the LVM and block device tools, because
# a build container has no block device to make a pool on. The stand-ins
# describe a Host after its first boot: nvme0n1 is the root disk, nvme1n1 is
# an instance-store disk that carries the pool and another mounted volume.
if "$LIBEXEC/check-thin-pool-cases" "$work"; then
  ok "thin-pool cases (existing pool, root disk, mounted disk, pool member)"
else
  fail "thin-pool cases"
fi

#= docs/requirements/11-host-image.md#image-storage
#= type=test
#/ The Host Image SHALL keep all `flintlockd` and containerd state under
#/ `/var`.
expect "flintlockd keeps its state under /var" has "$UNITS/flintlockd.service" '--state-dir /var/lib/flintlock '
expect "containerd keeps its state under /var" has /etc/containerd/config.toml '^root = "/var/lib/containerd"$'
expect "the devmapper snapshotter keeps its state under /var" has /etc/containerd/config.toml 'root_path = "/var/lib/containerd/'
expect "tmpfiles.d creates the state directories" has /usr/lib/tmpfiles.d/battery.conf '^d /var/lib/flintlock '
# The kubelet's CNI plugins are state too: a CNI DaemonSet such as
# Flannel's writes its own plugin into containerd's bin_dir, /opt/cni/bin,
# which is /var/opt/cni/bin through HI-082's link.
expect "containerd looks for CNI plugins in /opt/cni/bin" has /etc/containerd/config.toml '^ *bin_dirs = \["/opt/cni/bin"\]$'
expect "tmpfiles.d copies the CNI plugins into /var/opt/cni/bin" has /usr/lib/tmpfiles.d/battery.conf '^C /var/opt/cni/bin .* /usr/libexec/cni$'
expect "the CNI plugins are in the image" test -x /usr/libexec/cni/bridge

#= docs/requirements/11-host-image.md#kubernetes-node
#= type=test
#/ The Host Image SHALL make `/opt`, `/usr/local` and
#/ `/usr/libexec/kubernetes` links to `/var/opt`, `/var/usrlocal` and
#/ `/var/libexec/kubernetes`, SHALL create each target at boot, and SHALL
#/ label each target and every file in it as the base image's policy labels
#/ the path that links to it.
# Each link, the tmpfiles.d line that creates its target, and the target's
# label in the policy the module was installed into. A label is what
# systemd-tmpfiles gives a directory it creates, and what a file made under
# it inherits.
for lt in /opt=var/opt /usr/local=../var/usrlocal /usr/libexec/kubernetes=../../var/libexec/kubernetes; do
  l=${lt%%=*} t=${lt#*=}
  expect "$l is a link to $t" test "$(readlink "$l")" = "$t"
done
expect "/opt/cni resolves to /var/opt/cni" test "$(readlink -m /opt/cni)" = /var/opt/cni
expect "tmpfiles.d creates /var/opt" sh -c "cat /usr/lib/tmpfiles.d/*.conf | grep -qE '^d /var/opt '"
expect "tmpfiles.d creates /var/usrlocal" sh -c "cat /usr/lib/tmpfiles.d/*.conf | grep -qE '^d /var/usrlocal '"
expect "tmpfiles.d creates /var/usrlocal/bin" has /usr/lib/tmpfiles.d/battery.conf '^d /var/usrlocal/bin '
expect "tmpfiles.d creates /var/libexec/kubernetes" has /usr/lib/tmpfiles.d/battery.conf '^d /var/libexec/kubernetes '
expect "tmpfiles.d creates the kubelet's volume plugin directory" has /usr/lib/tmpfiles.d/battery.conf '^d /var/libexec/kubernetes/kubelet-plugins/volume/exec '
if ! command -v matchpathcon >/dev/null 2>&1; then
  skip "labels of the link targets: matchpathcon is not installed"
elif ! semodule -l 2>/dev/null | grep -qx battery; then
  skip "labels of the link targets: the battery module is not installed here"
else
  # Each label is the one the base policy gives the path under the link
  # (matchpathcon in the base: /opt usr_t, /opt/cni/bin and /usr/local/bin
  # bin_t, /usr/libexec/kubernetes and everything under it bin_t). The
  # lookup resolves links in a path that exists, so the targets are looked
  # up by name.
  for pl in /var/libexec/kubernetes=bin_t /var/libexec/kubernetes/kubelet-plugins/volume/exec=bin_t \
    /var/libexec/kubernetes/kubelet-plugins/volume/exec/vendor~driver/driver=bin_t \
    /var/opt=usr_t /var/opt/cni/bin=bin_t /var/opt/local-path-provisioner=container_file_t \
    /var/usrlocal=usr_t /var/usrlocal/bin=bin_t /var/libexec=var_t; do
    p=${pl%%=*} want=system_u:object_r:${pl#*=}:s0
    got=$(matchpathcon -n "$p" 2>/dev/null)
    if [ "$got" = "$want" ]; then ok "$p is labelled $want"; else fail "$p is labelled '$got', want $want"; fi
  done
fi

# ---------------------------------------------------------------------------
echo "== networking"
host_config "$work/host.conf" >/dev/null 2>&1
expect "network renders the firewall and the dnsmasq configuration" "$LIBEXEC/network" render "$work/net"
nftf=$work/net/guest-firewall.nft
dnsf=$work/net/dnsmasq.conf

#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL create a Linux bridge at boot with the
#/ guest subnet from the Host configuration file and SHALL configure
#/ `flintlockd` to attach TAP interfaces to it.
expect "network creates the bridge" has "$LIBEXEC/network" 'ip link add "\$bridge" type bridge'
expect "the bridge takes the gateway address of the configured subnet" has "$LIBEXEC/network" 'ip addr add "\$BATTERY_GATEWAY/\$BATTERY_PREFIX" dev "\$bridge"'
expect "flintlockd attaches TAP interfaces to the bridge battery-network names" has "$UNITS/flintlockd.service" '--bridge-name \$\{BATTERY_BRIDGE\} '
expect "battery-network gives flintlockd the bridge's name" grep -qx 'BATTERY_BRIDGE=virbr-battery' "$work/net/flintlockd.env"
expect "the bridge is named virbr-battery" has "$LIBEXEC/lib.sh" '^BATTERY_BRIDGE=virbr-battery$'

#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL give the guest bridge a name that matches the
#/ default interface exclude list of Calico's IP address autodetection.
# Calico's first-found method skips an interface whose name matches its
# exclude list (DEFAULT_INTERFACES_TO_EXCLUDE in
# node/pkg/lifecycle/startup/autodetection/autodetection_linux.go, the same
# in v3.31.2 and v3.32.2). ^virbr.* is the entry the bridge's name takes. A
# name off the list lets calico-node take the gateway address as the Node's.
bridge_name=$(sed -n 's/^BATTERY_BRIDGE=//p' "$LIBEXEC/lib.sh")
expect "the bridge's name matches Calico's ^virbr.* exclude" grep -qE '^virbr' <<<"$bridge_name"
expect "the bridge's name fits Linux's 15 characters" test "${#bridge_name}" -ge 4 -a "${#bridge_name}" -le 15

#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL run a DHCP and DNS service bound to the
#/ bridge so that guests obtain an address, gateway and resolver without
#/ static configuration.
expect "dnsmasq serves the bridge only" has "$dnsf" '^interface=virbr-battery$'
expect "dnsmasq hands out the subnet" has "$dnsf" '^dhcp-range=10\.200\.4\.10,10\.200\.7\.254,255\.255\.252\.0,12h$'
expect "dnsmasq hands out the gateway" has "$dnsf" '^dhcp-option=option:router,10\.200\.4\.1$'
expect "dnsmasq hands out the resolver" has "$dnsf" '^dhcp-option=option:dns-server,10\.200\.4\.1$'
mkdir -p /run/systemd/resolve /var/lib/dnsmasq
[ -e /run/systemd/resolve/resolv.conf ] || : >/run/systemd/resolve/resolv.conf
expect "dnsmasq accepts the configuration" dnsmasq --test --conf-file="$dnsf"

#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL enable IPv4 forwarding and SHALL configure
#/ source NAT from the guest subnet to the Host's primary interface.
expect "sysctl.d enables IPv4 forwarding" has /usr/lib/sysctl.d/90-battery.conf '^net\.ipv4\.ip_forward = 1$'
expect "guests are masqueraded out of the primary interface" has "$nftf" 'ip saddr 10\.200\.4\.0/22 oifname "eth0" masquerade'

#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL drop traffic from the guest subnet to the instance
#/ metadata service addresses.
expect "the metadata service is dropped" has "$nftf" 'iifname "virbr-battery" ip daddr 169\.254\.169\.254 drop'
expect "the IPv6 metadata service is dropped" has "$nftf" 'iifname "virbr-battery" ip6 daddr fd00:ec2::254 drop'

#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL drop traffic from the guest subnet to the Host's own
#/ `flintlockd`, kubelet, Exec Agent and metrics ports.
expect "the kubelet, Exec Agent, flintlockd and metrics ports are dropped" \
  has "$nftf" 'iifname "virbr-battery" tcp dport \{ 9090, 8090, 10248, 10250, 10255, 10256, 10270, 1338 \} drop'

#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL drop traffic from the guest subnet to every
#/ protected CIDR listed in the Host configuration file.
expect "the protected CIDRs are a set" has "$nftf" 'elements = \{ 10\.0\.0\.0/16, 192\.168\.0\.0/16 \}'
expect "traffic to the protected set is dropped" has "$nftf" 'iifname "virbr-battery" ip daddr @protected drop'
drop_line=$(grep -n '@protected drop' "$nftf" | tail -n 1 | cut -d: -f1)
accept_line=$(grep -n 'oifname "eth0" accept' "$nftf" | cut -d: -f1)
expect "the drops come before guests are let out" test "${drop_line:-9999}" -lt "${accept_line:-0}"

#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL allow traffic from the guest subnet to the bridge
#/ gateway address only on the DHCP and DNS ports and the gateway service
#/ ports, and SHALL keep every other port on the gateway closed to guests.
expect "DHCP is allowed, as a broadcast and on the gateway" has "$nftf" 'iifname "virbr-battery" ip daddr \{ 255\.255\.255\.255, 10\.200\.4\.1 \} udp dport 67 accept'
expect "DNS is allowed on the gateway" has "$nftf" 'iifname "virbr-battery" ip daddr 10\.200\.4\.1 udp dport 53 accept'
expect "DNS over TCP is allowed on the gateway" has "$nftf" 'iifname "virbr-battery" ip daddr 10\.200\.4\.1 tcp dport 53 accept'
# DHCP's and DNS's are the only accepts for guests in the input chain.
accepts=$(awk '/chain input/ { on = 1 } on && /^\t}/ { exit } on && /^[[:space:]]*iifname "virbr-battery".* accept$/ { n++ } END { print n + 0 }' "$nftf")
expect "no other port on the gateway is open to guests" test "$accepts" = 3
expect "everything else from the bridge is dropped" has "$nftf" '^[[:space:]]*iifname "virbr-battery" drop$'
last_input=$(awk '/chain input/ { on = 1 } on && /^\t}/ { exit } on && /(accept|drop)$/ { l = $0 } END { print l }' "$nftf")
expect "the drop is the input chain's last rule" test "$(echo "$last_input" | xargs)" = 'iifname virbr-battery drop'

#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL drop traffic from the guest subnet to every
#/ address of the Host other than the bridge gateway address.
#
#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL forward traffic from the guest subnet only
#/ out of the Host's primary interface, and SHALL drop traffic from the guest
#/ subnet to every other interface of the Host.
#
#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL drop traffic from the guest subnet whose
#/ destination before any destination NAT on the Host is in a protected CIDR.
#
#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL allow TCP traffic from the guest subnet to
#/ the bridge gateway address on the gateway service ports, which it reads
#/ from the Host configuration file, and SHALL allow it on no port when
#/ none are set.
#
#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL drop traffic to the gateway service ports on
#/ the bridge gateway address that arrives on any interface other than the
#/ bridge and loopback.
# The rules, rendered from the installed scripts. That the kernel enforces
# them needs network namespaces, which a build container does not grant;
# `make host-image-lint` shows it where they are available.
if "$LIBEXEC/check-guest-isolation-cases" "$work"; then
  ok "guest isolation cases (the Host's addresses, other interfaces, Services, gateway service ports)"
else
  fail "guest isolation cases"
fi

#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL drop traffic from the gateway service user
#/ ids, which it reads from the Host configuration file, to the instance
#/ metadata service addresses and to the Host's own `flintlockd`, kubelet,
#/ Exec Agent and metrics ports.
if "$LIBEXEC/check-gateway-service-egress-cases" "$work"; then
  ok "gateway service egress cases (listed user ids kept from the metadata service and the control ports)"
else
  fail "gateway service egress cases"
fi
# nft -c needs a netlink socket, which a build container may not grant.
nft_out=$(nft -c -f "$nftf" 2>&1)
case "$?:$nft_out" in
0:*) ok "nft accepts the ruleset" ;;
*"Operation not permitted"* | *"netlink"* | *"Protocol not supported"*) skip "nft cannot open netlink here: $(echo "$nft_out" | head -n1)" ;;
*) fail "nft rejects the ruleset: $nft_out" ;;
esac

#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL use a default guest subnet that the Host
#/ configuration file can override, so that it can be kept clear of the
#/ cluster's node, pod and service ranges.
expect "the image has a default guest subnet" has /usr/share/battery/host.conf.defaults '^GUEST_SUBNET=10\.220\.0\.0/16$'
# The default clashes with neither AWS's default VPC, 172.31.0.0/16, nor
# flintlock's documented bridge subnet, 192.168.100.0/24.
refute "the default is not AWS's default VPC range" grep -q '^GUEST_SUBNET=172\.31\.' /usr/share/battery/host.conf.defaults
refute "the default is not flintlock's documented bridge subnet" grep -q '^GUEST_SUBNET=192\.168\.' /usr/share/battery/host.conf.defaults
expect "the Host configuration file overrode it above" grep -qxF BATTERY_GUEST_SUBNET=10.200.4.0/22 "$BATTERY_HOST_ENV"

# ---------------------------------------------------------------------------
echo "== flintlockd"
fl=$UNITS/flintlockd.service
# The unit without its comments, for checks on what it actually passes.
flx=$work/flintlockd.service
grep -v "^[[:space:]]*#" "$fl" >"$flx"

#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL run `flintlockd`, its containerd and every
#/ hypervisor process they start as systemd services outside the cgroup of
#/ any Kubernetes pod.
expect "flintlockd is a systemd service of the image" test -f "$fl"
expect "containerd is a systemd service of the image" test -f "$UNITS/containerd.service"
refute "flintlockd is put in no slice of the kubelet's" grep -Eq '^Slice=.*kubepods' "$fl"
refute "no static pod or manifest runs flintlockd" grep -rqs flintlockd /etc/kubernetes/manifests

#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ When the Host Image stops or restarts `flintlockd`, it SHALL
#/ stop the `flintlockd` process alone and SHALL leave every hypervisor
#/ process that `flintlockd` started running.
# KillMode=process makes systemd signal flintlockd's main process alone, on
# a stop and on the stop half of a restart. The hypervisor processes stay
# in the unit's cgroup, and flintlockd finds them again when it starts.
expect "a flintlockd stop or restart signals flintlockd alone" has "$fl" '^KillMode=process$'
refute "nothing else stops the hypervisor processes with flintlockd" grep -Eq '^(ExecStop|ExecStopPost)=' "$flx"
expect "a certificate change restarts flintlockd, which keeps KillMode" has "$LIBEXEC/flintlockd-certs" 'try-restart flintlockd\.service'
refute "no drop-in changes how flintlockd is stopped" sh -c "grep -rqsE '^(KillMode|ExecStop)' /usr/lib/systemd/system/flintlockd.service.d /etc/systemd/system/flintlockd.service.d"

#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL start `flintlockd` only after the thin
#/ pool and the bridge are present.
for dep in battery-thin-pool battery-network containerd; do
  expect "flintlockd requires $dep" has "$fl" "^Requires=.*$dep\\.service"
  expect "flintlockd starts after $dep" has "$fl" "^After=.*$dep\\.service"
done

#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL configure `flintlockd` to serve its gRPC
#/ API only with TLS, on port 9090 of the Host's internal address, with the
#/ serving certificate and key that the Exec Agent writes to
#/ `/etc/battery/flintlockd`.
# The endpoint is the one battery-network rendered above for the primary
# interface's address, and the only one: no loopback, no HTTP gateway, no
# debug endpoint, no plaintext.
expect "flintlockd takes its endpoint from battery-network" has "$fl" '^EnvironmentFile=/run/battery/flintlockd\.env$'
expect "flintlockd listens on BATTERY_FLINTLOCKD_ENDPOINT" has "$fl" '--grpc-endpoint \$\{BATTERY_FLINTLOCKD_ENDPOINT\} '
expect "battery-network renders the primary interface's address and port 9090 as the endpoint" \
  grep -qx 'BATTERY_FLINTLOCKD_ENDPOINT=192\.0\.2\.10:9090' "$work/net/flintlockd.env"
endpoints=$(grep -Eo -- '--(grpc|http|debug)-endpoint [^ ]+' "$flx" | awk '{print $2}' | xargs)
if [ "$endpoints" = '${BATTERY_FLINTLOCKD_ENDPOINT}' ]; then ok "flintlockd has one endpoint, the Host's internal address"; else fail "flintlockd's endpoints are: $endpoints"; fi
refute "flintlockd does not run without TLS" grep -q -- "--insecure" "$flx"
refute "the HTTP gateway stays off" grep -q -- "--enable-http" "$flx"
refute "no flintlockd configuration file overrides the unit" test -e /etc/opt/flintlockd/config.yaml
expect "flintlockd serves the Exec Agent's certificate" has "$fl" '--tls-cert /etc/battery/flintlockd/tls\.crt '
expect "flintlockd serves the Exec Agent's key" has "$fl" '--tls-key /etc/battery/flintlockd/tls\.key '
expect "guests are refused the flintlockd port" has "$nftf" 'tcp dport \{ 9090,'

#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL configure `flintlockd` to require a client
#/ certificate on every connection and to verify it against the `flintlockd`
#/ client CA bundle that the Exec Agent writes to `/etc/battery/flintlockd`.
# --tls-client-validate is what makes flintlockd require a certificate
# (tls.RequireAndVerifyClientCert); --tls-client-ca alone only names a pool.
expect "flintlockd requires a client certificate" has "$fl" '--tls-client-validate '
expect "flintlockd verifies it against the Exec Agent's client CA bundle" has "$fl" '--tls-client-ca /etc/battery/flintlockd/client-ca\.crt '
refute "flintlockd is given no token instead" grep -q -- "--basic-auth-token" "$flx"

#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL drop every connection to `flintlockd`'s
#/ port that arrives from outside the Host unless its source address is in
#/ the Operator's pod network, which it reads from the Host configuration
#/ file.
#
#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL refuse connections to `flintlockd`'s port
#/ from the Host's own processes unless they belong to the Exec Agent's user
#/ id, which it reads from the Host configuration file with a default when
#/ none is set.
# The rules the firewall loads, rendered from the installed scripts for the
# defaults, configured values and values that are neither. That the kernel
# enforces them cannot be shown here: a build container has no netlink for
# nft to load them with, let alone a second user to connect as.
if "$LIBEXEC/check-flintlockd-access-cases" "$work"; then
  ok "flintlockd access cases (Exec Agent user id, Operator's pod network, refused values)"
else
  fail "flintlockd access cases"
fi
expect "the rendered firewall admits only the Exec Agent's user id to flintlockd from the Host" \
  has "$nftf" '^[[:space:]]*oifname "lo" tcp dport 9090 meta skuid != 65532 counter reject with tcp reset$'
expect "the rendered firewall drops flintlockd's port from off the Host outside the Operator's pod network" \
  has "$nftf" '^[[:space:]]*iifname != \{ "lo", "virbr-battery" \} tcp dport 9090 counter drop$'
expect "battery-network loads the rules before flintlockd starts" has "$UNITS/battery-network.service" '^Before=flintlockd\.service'

#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL start `flintlockd` only once the serving
#/ certificate, its key and the client CA bundle exist in
#/ `/etc/battery/flintlockd`, and SHALL restart `flintlockd` when the serving
#/ certificate or the client CA bundle changes.
#
#= docs/requirements/11-host-image.md#kernel-and-kvm
#= type=test
#/ The Host Image SHALL create `/etc/battery/flintlockd` owned by
#/ the Exec Agent's user id, and SHALL label that directory and every file in
#/ it so that the Exec Agent's containers can write them and `flintlockd` can
#/ read them
# The units that wait for and follow the files, and flintlockd-certs run
# against stand-ins for systemctl and restorecon. That systemd watches the
# files, and that the labels are enforced, is for a booted Host; the labels
# themselves are looked up in the policy by the SELinux context cases.
if "$LIBEXEC/check-flintlockd-certs-cases" "$work"; then
  ok "flintlockd certificate cases (path units, restart only on change, directory owner and labels)"
else
  fail "flintlockd certificate cases"
fi

#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL enable the `flintlockd` exec API.
expect "the exec API is enabled" has "$fl" '--enable-exec-api '

#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL configure `flintlockd` and the container
#/ runtime interface of containerd to read per-registry configuration
#/ from `/etc/containerd/certs.d`, and SHALL ship no registry
#/ configuration there.
# The CRI half as containerd reads it, from the `config dump` of the kernel
# and KVM section, or from the file where that could not run.
expect "flintlockd reads registry configuration from /etc/containerd/certs.d" has "$fl" '--containerd-hosts-dir /etc/containerd/certs\.d '
cri_registry() {
  awk '/^\[/ { t = $0 } t ~ /^\[plugins\.["'\'']io\.containerd\.cri\.v1\.images["'\'']\.registry\]$/ && $1 == "config_path" { print $3 }' "$1" | tr -d "\"'"
}
if [ -s "$work/containerd-dump.toml" ]; then
  src=$work/containerd-dump.toml what="containerd's effective CRI configuration"
else
  src=$work/containerd-file.toml what=/etc/containerd/config.toml
fi
got=$(cri_registry "$src")
if [ "$got" = /etc/containerd/certs.d ]; then ok "$what reads registry configuration from /etc/containerd/certs.d alone"; else fail "$what reads registry configuration from '$got'"; fi
refute "the image ships no registry configuration" sh -c "find /etc/containerd/certs.d /etc/docker/certs.d -type f 2>/dev/null | grep -q ."
# Every flag the unit passes has to exist in the pinned flintlockd.
help=$(flintlockd run --help 2>&1)
# shellcheck disable=SC2013
for flag in $(grep -Eo -- "--[a-z][a-z-]+" "$flx" | sort -u); do
  if printf '%s\n' "$help" | grep -q -- "$flag"; then ok "flintlockd $FLINTLOCK_VERSION knows $flag"; else fail "flintlockd $FLINTLOCK_VERSION has no $flag"; fi
done

# ---------------------------------------------------------------------------
echo "== Kubernetes node"

#= docs/requirements/11-host-image.md#kubernetes-node
#= type=test
#/ The Host Image SHALL register its kubelet with the label
#/ `battery.liquidmetal-x.dev/host-image` set to a value derived from the
#/ image digest, and with one label per hypervisor carrying its pinned
#/ version.
digest=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
expect "kubelet-config runs" env BATTERY_IMAGE_DIGEST=$digest BATTERY_CPUS=96 BATTERY_MEM_KIB=394264576 "$LIBEXEC/kubelet-config"
kenv=$work/run/kubelet.env
p=battery\\.liquidmetal-x\\.dev
expect "label host-image from the digest" has "$kenv" "--node-labels=([^ ]*,)?$p/host-image=0123456789abcdef0123456789abcdef(,| )"
expect "label firecracker version" has "$kenv" "--node-labels=[^ ]*$p/firecracker=${FIRECRACKER_VERSION//./\\.}(,| )"
expect "label cloud-hypervisor version" has "$kenv" "--node-labels=[^ ]*$p/cloud-hypervisor=${CLOUD_HYPERVISOR_VERSION//./\\.}(,| )"

#= docs/requirements/11-host-image.md#kubernetes-node
#= type=test
#/ The Host Image SHALL register its kubelet with the label
#/ `battery.liquidmetal-x.dev/host` set to `true`.
expect "label battery.liquidmetal-x.dev/host=true, for the Exec Agent" \
  has "$kenv" "--node-labels=([^ ]*,)?battery\\.liquidmetal-x\\.dev/host=true(,| )"
expect "the kubelet is started with the labels" has "$UNITS/kubelet.service.d/20-battery.conf" '^ExecStart=/usr/bin/kubelet .*\$BATTERY_KUBELET_ARGS$'
expect "kubeadm's drop-in is the one 20-battery.conf extends" test -f "$UNITS/kubelet.service.d/10-kubeadm.conf"
expect "kubeadm's drop-in still starts the kubelet the way 20-battery.conf repeats" \
  has "$UNITS/kubelet.service.d/10-kubeadm.conf" '^ExecStart=/usr/bin/kubelet \$KUBELET_KUBECONFIG_ARGS \$KUBELET_CONFIG_ARGS \$KUBELET_KUBEADM_ARGS \$KUBELET_EXTRA_ARGS$'

#= docs/requirements/11-host-image.md#kubernetes-node
#= type=test
#/ The Host Image SHALL configure the kubelet to reserve all CPU
#/ and memory beyond the configured Host reserve, so that the Host's Node
#/ offers only the Host reserve to pods and leaves the rest to the MicroVMs
#/ that battery places on the Host.
# 96 CPUs and 376 GiB with a Host reserve of 4 CPUs and 8192 MiB.
expect "all but the Host reserve is reserved" has "$kenv" -- "--system-reserved=cpu=92000m,memory=$((394264576 - 8192 * 1024))Ki\$"
expect "a machine smaller than the reserve reserves nothing" \
  sh -c "BATTERY_CPUS=2 BATTERY_MEM_KIB=1048576 $LIBEXEC/kubelet-config && grep -q -- '--system-reserved=cpu=0m,memory=0Ki' $kenv"

#= docs/requirements/11-host-image.md#kubernetes-node
#= type=test
#/ The Host Image SHALL disable automatic bootc updates, so that
#/ an operating system update is applied only to a drained Host.
for u in bootc-fetch-apply-updates.timer bootc-fetch-apply-updates.service; do
  if [ "$(systemctl is-enabled "$u" 2>/dev/null)" = masked ]; then ok "$u is masked"; else fail "$u is not masked"; fi
done

echo
if [ "$failures" -ne 0 ]; then
  echo "$failures check(s) failed"
  exit 1
fi
echo "every check passed"
