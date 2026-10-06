#!/usr/bin/env bash
# Routes the MicroVMs on br-battery out through the Host (bo-guest-nat.service):
# IP forwarding on, and NAT (masquerade) for their subnet, 10.220.0.0/24,
# out of whichever interface the Host routes to. Idempotent: a rule is added
# only when it is not there.
#
# k3s keeps its own rules in its own chains (KUBE-*, FLANNEL-*), and adds
# its jumps to the built-in chains beside these. The rules here match only
# br-battery and its subnet, so they leave the pods' traffic alone. Each has the
# comment bo-trial, to find them.
set -euo pipefail

subnet=10.220.0.0/24
bridge=br-battery

# k3s turns forwarding on too; /etc/sysctl.d/90-bo-trial.conf keeps it on
# from boot.
sysctl -q -w net.ipv4.ip_forward=1

# rule adds an iptables rule to a table's chain unless it is there already:
# rule <table> <chain> <-A|-I> <match and target...>.
rule() {
	local table="$1" chain="$2" how="$3"
	shift 3
	iptables -t "${table}" -C "${chain}" -m comment --comment bo-trial "$@" 2>/dev/null ||
		iptables -t "${table}" "${how}" "${chain}" -m comment --comment bo-trial "$@"
}

# The guests' traffic to anywhere but their own subnet leaves with the
# Host's address.
rule nat POSTROUTING -A -s "${subnet}" ! -d "${subnet}" -j MASQUERADE
# FORWARD's policy is ACCEPT on the trial's Host. These accept the guests'
# traffic even where a firewall drops by default: out from the bridge, and
# the replies back.
rule filter FORWARD -I -i "${bridge}" -j ACCEPT
rule filter FORWARD -I -o "${bridge}" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
