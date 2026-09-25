#!/usr/bin/env bash
# Starts flintlockd (flintlockd.service) on the Host's internal address,
# the address of FLINTLOCKD_IFACE (up.sh writes it to
# /etc/default/bo-trial-flintlockd), with mutual TLS from the files the Exec
# Agent writes.
#
# The flags follow flintlock-runner's Host Image unit
# (image/rootfs/usr/lib/systemd/system/flintlockd.service) where they can:
# the exec API on, the bridge flbr0, flintlockd's containerd and state
# directory. What differs is what battery-operator's ADR 0002 changes: the
# endpoint is the Host's internal address, not loopback, and the transport
# is TLS with client certificates validated against the flintlockd client
# CA, instead of --insecure.
set -euo pipefail

dir=/etc/battery/flintlockd
iface="${FLINTLOCKD_IFACE:-eth0}"
addr="$(ip -4 -o addr show dev "${iface}" | awk '!found { sub("/.*", "", $4); print $4; found = 1 }')"
[[ -n "${addr}" ]] || { echo "no IPv4 address on ${iface}" >&2; exit 1; }

for f in tls.crt tls.key client-ca.crt; do
	[[ -s "${dir}/${f}" ]] || { echo "${dir}/${f} is missing: the Exec Agent has not written it yet" >&2; exit 1; }
done

exec /usr/local/bin/flintlockd run \
	--grpc-endpoint "${addr}:9090" \
	--tls-cert "${dir}/tls.crt" \
	--tls-key "${dir}/tls.key" \
	--tls-client-validate \
	--tls-client-ca "${dir}/client-ca.crt" \
	--enable-exec-api \
	--bridge-name flbr0 \
	--containerd-socket /run/containerd/containerd.sock \
	--state-dir /var/lib/flintlock \
	--firecracker-bin /usr/local/bin/firecracker \
	--verbosity "${FLINTLOCKD_VERBOSITY:-0}"
