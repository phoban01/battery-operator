#!/usr/bin/env bash
# Usage: hostimage/check-labels.sh DIR
#
# The half of the check stage that cannot run inside the image: its OCI
# labels are not in its filesystem. DIR holds what the Dagger module's
# host-image-check read from the built image:
#
#   labels        the image's OCI labels, one KEY=value per line
#   platform      the image's platform, for example linux/amd64
#   want-platform the platform the image was built for, for example
#                 linux/arm64
#   versions.env  /usr/share/battery/versions.env from inside the image
#
# Fails unless every component version of hostimage/versions.env is a label
# with the same value, the image is the platform it was built for and that
# is linux/amd64 or linux/arm64, and the versions file inside it is this
# one.
#
#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image SHALL record the pinned version of each component
#/ of HI-003 as an OCI image label and in a versions file on the image's
#/ `/usr` tree.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
dir=${1:?usage: check-labels.sh DIR}
# shellcheck source=hostimage/versions.env
. "$here/versions.env"

failed=0
label() {
  local key=dev.liquidmetal-x.battery.version.$1 want=$2 got
  got=$(sed -n "s/^${key//./\\.}=//p" "$dir/labels")
  if [ "$got" = "$want" ]; then
    echo "ok    label $key=$got"
  else
    echo "FAIL  label $key is '$got', versions.env pins '$want'"
    failed=1
  fi
}
label containerd "$CONTAINERD_VERSION"
label runc "$RUNC_VERSION"
label firecracker "$FIRECRACKER_VERSION"
label cloud-hypervisor "$CLOUD_HYPERVISOR_VERSION"
label flintlock "$FLINTLOCK_VERSION"
label kubernetes "$KUBERNETES_VERSION"
label cloud-init "$CLOUD_INIT_VERSION"

#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image SHALL be built for the `x86_64` and `aarch64`
#/ architectures.
# linux/amd64 is x86_64 and linux/arm64 is aarch64. CI builds and checks
# each; this checks one build.
platform=$(cat "$dir/platform")
want=$(cat "$dir/want-platform")
case "$want" in
linux/amd64 | linux/arm64) echo "ok    the image was built for $want" ;;
*)
  echo "FAIL  the image was built for $want, which is neither linux/amd64 nor linux/arm64"
  failed=1
  ;;
esac
if [ "$platform" = "$want" ]; then
  echo "ok    the image's platform is $platform"
else
  echo "FAIL  the image's platform is $platform, want $want"
  failed=1
fi

if cmp -s "$dir/versions.env" "$here/versions.env"; then
  echo "ok    /usr/share/battery/versions.env in the image is hostimage/versions.env"
else
  echo "FAIL  /usr/share/battery/versions.env in the image differs from hostimage/versions.env"
  failed=1
fi
exit "$failed"
