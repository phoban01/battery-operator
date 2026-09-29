#!/usr/bin/env bash
# Usage: hostimage/publish-ami.sh IMAGE --bucket BUCKET --region REGION [--name NAME] [--rootfs xfs] [--dry-run]
#
# Converts a published Host Image to an AMI with bootc-image-builder, and
# tags the AMI, for the Cluster API host pools in config/capi. It runs only
# when someone asks for it (`make host-image-ami`), never as part of a
# build, and it is the only step of the Host Image that needs AWS: an S3
# bucket for the snapshot import, the `vmimport` service role, and
# credentials in the environment or ~/.aws that may use both and
# ec2:RegisterImage, ec2:DescribeImages and ec2:CreateTags. It also needs
# podman and sudo. See hostimage/README.md, "Publishing an AMI".
#
# IMAGE is a Host Image in a registry, for example
# ghcr.io/phoban01/battery-operator/host-image:v0.2.0, which CI publishes
# on a version tag. The script pulls it into root's podman storage, where
# bootc-image-builder reads it.
#
# UNTESTED against AWS: written from the bootc-image-builder
# documentation, and run only with --dry-run. flintlock-runner, where it
# comes from, had no AWS account either.
#
#= docs/requirements/11-host-image.md#image-build
#/ Where publishing an AMI is requested, the Host Image build SHALL
#/ convert the container image to an AMI with `bootc-image-builder` and SHALL
#/ tag the AMI with the container image digest and the pinned Kubernetes
#/ version.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=hostimage/versions.env
. "$here/versions.env"

# bootc-image-builder reads the image from root's podman storage, so this is
# podman and nothing else. quay.io/centos-bootc/bootc-image-builder:latest
# as of 2026-09-21; it builds Fedora bootc images too.
BIB_IMAGE=${BIB_IMAGE:-quay.io/centos-bootc/bootc-image-builder@sha256:2b52843ea2bfda73b0a08d97e76b734393b1d3a804681b9fabb26723bd3a2f0b}
# The AMI's tags: the image it was made from, and the Kubernetes version a
# MachineDeployment that boots it has to name (config/capi).
TAG_DIGEST=battery.liquidmetal-x.dev/image-digest
TAG_KUBERNETES=battery.liquidmetal-x.dev/kubernetes-version
TAG_HOST_IMAGE=battery.liquidmetal-x.dev/host-image

usage() {
  sed -n '2p' "${BASH_SOURCE[0]}" | sed 's/^# //' >&2
  exit 2
}
image=${1:-}
[ -n "$image" ] || usage
case $image in -*) usage ;; esac
shift
bucket="" region=${AWS_REGION:-${AWS_DEFAULT_REGION:-}} name="" rootfs=xfs dry_run=${DRY_RUN:-0}
while [ "$#" -gt 0 ]; do
  case "$1" in
  --bucket) bucket=${2:-}; shift 2 ;;
  --region) region=${2:-}; shift 2 ;;
  --name) name=${2:-}; shift 2 ;;
  --rootfs) rootfs=${2:-}; shift 2 ;;
  --dry-run) dry_run=1; shift ;;
  *) usage ;;
  esac
done
if [ -z "$bucket" ] || [ -z "$region" ]; then usage; fi

run() {
  if [ "$dry_run" = 1 ]; then
    printf 'would run:'
    printf ' %q' "$@"
    printf '\n'
  else
    "$@"
  fi
}

# The digest names the container image the AMI was made from. An image
# that was only built locally has none until it is pushed; refuse rather
# than tag an AMI with something that names nothing.
#
# The Kubernetes version is the one the image carries, from its OCI label
# (HI-005), so that an image older than this checkout is tagged with its
# own version. A dry run has no image and takes versions.env's.
kubernetes=$KUBERNETES_VERSION
if [ "$dry_run" = 1 ]; then
  digest=${IMAGE_DIGEST:-sha256:0000000000000000000000000000000000000000000000000000000000000000}
else
  command -v podman >/dev/null || { echo "podman is required" >&2; exit 1; }
  command -v aws >/dev/null || { echo "the aws command line is required to tag the AMI" >&2; exit 1; }
  sudo podman image exists "$image" || sudo podman pull "$image"
  digest=${IMAGE_DIGEST:-$(sudo podman image inspect --format '{{ .Digest }}' "$image")}
  kubernetes=$(sudo podman image inspect --format '{{ index .Labels "dev.liquidmetal-x.battery.version.kubernetes" }}' "$image")
fi
[[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || {
  echo "cannot determine the digest of $image (got '$digest'); push it, pull it as root, or set IMAGE_DIGEST" >&2
  exit 1
}
[[ $kubernetes =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
  echo "$image has no Kubernetes version label (got '$kubernetes'); is it a Host Image?" >&2
  exit 1
}
[ -n "$name" ] || name="battery-host-${kubernetes}-${digest:7:12}"

# Credentials reach the builder from the environment when they are there,
# and from ~/.aws otherwise. They are never written anywhere.
creds=()
if [ -n "${AWS_ACCESS_KEY_ID:-}" ]; then
  creds+=(--env AWS_ACCESS_KEY_ID --env AWS_SECRET_ACCESS_KEY)
  [ -n "${AWS_SESSION_TOKEN:-}" ] && creds+=(--env AWS_SESSION_TOKEN)
else
  creds+=(-v "$HOME/.aws:/root/.aws:ro" --env "AWS_PROFILE=${AWS_PROFILE:-default}")
fi

# --preserve-env keeps the credentials across sudo without putting them on
# a command line.
run sudo --preserve-env=AWS_ACCESS_KEY_ID,AWS_SECRET_ACCESS_KEY,AWS_SESSION_TOKEN \
  podman run --rm --privileged --pull=newer \
  --security-opt label=type:unconfined_t \
  -v /var/lib/containers/storage:/var/lib/containers/storage \
  "${creds[@]}" \
  "$BIB_IMAGE" \
  --type ami \
  --target-arch amd64 \
  --rootfs "$rootfs" \
  --aws-ami-name "$name" \
  --aws-bucket "$bucket" \
  --aws-region "$region" \
  "$image"

if [ "$dry_run" = 1 ]; then
  ami="ami-00000000000000000"
else
  ami=$(aws ec2 describe-images --region "$region" --owners self \
    --filters "Name=name,Values=$name" --query 'Images[0].ImageId' --output text)
fi
[[ $ami =~ ^ami-[0-9a-f]+$ ]] || {
  echo "bootc-image-builder finished but no AMI named $name was found in $region" >&2
  exit 1
}
run aws ec2 create-tags --region "$region" --resources "$ami" --tags \
  "Key=$TAG_DIGEST,Value=$digest" \
  "Key=$TAG_KUBERNETES,Value=$kubernetes" \
  "Key=$TAG_HOST_IMAGE,Value=true"
echo "$ami $name $digest $kubernetes"
