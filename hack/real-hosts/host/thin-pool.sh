#!/usr/bin/env bash
# Creates containerd's thin pool, flintlock-thinpool, at boot
# (bo-thin-pool.service): a device-mapper thin pool on two loop devices
# over sparse files, as flintlock's own `provision devpool` does. A real
# Host has a volume group for it (flintlock-runner's Host Image: the logical
# volume thinpool in the volume group flintlock); the name is the same, so
# the Exec Agent's --thin-pool default holds.
set -euo pipefail

name=flintlock-thinpool
dir=/var/lib/bo-trial/thinpool
data="${dir}/data"
meta="${dir}/metadata"
data_size="${THIN_POOL_DATA_SIZE:-24G}"
meta_size="${THIN_POOL_METADATA_SIZE:-1G}"

if dmsetup status "${name}" >/dev/null 2>&1; then
	exit 0
fi

mkdir -p "${dir}"
[[ -f "${data}" ]] || truncate -s "${data_size}" "${data}"
[[ -f "${meta}" ]] || truncate -s "${meta_size}" "${meta}"

attach() {
	local dev
	dev="$(losetup --output NAME --noheadings --associated "$1" | head -1)"
	[[ -n "${dev}" ]] || dev="$(losetup --find --show "$1")"
	echo "${dev}"
}
datadev="$(attach "${data}")"
metadev="$(attach "${meta}")"

sectors=$(($(blockdev --getsize64 "${datadev}") / 512))
# 64 KiB blocks (128 sectors) and a low water mark of 32768 blocks, as
# flintlock's provision uses.
dmsetup create "${name}" --table "0 ${sectors} thin-pool ${metadev} ${datadev} 128 32768 1 skip_block_zeroing"
