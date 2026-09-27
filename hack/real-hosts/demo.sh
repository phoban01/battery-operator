#!/usr/bin/env bash
# The demo of the docs site (site/index.md), against what up.sh brought up:
# a Pool fills with Firecracker MicroVMs, a Consumer claims one and runs a
# command in it through the Exec Agent, and deleting the Pool deletes its
# MicroVMs. Each command is shown before it runs, as if typed.
#
#   demo.sh            run the demo
#   demo.sh --record   record it with asciinema to site/demo.cast
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

NS=demo
POOL=demo
HOLDER=demo-holder
CAST="${CAST:-${REPO_ROOT}/site/demo.cast}"

if [[ "${1:-}" == --record ]]; then
	command -v asciinema >/dev/null || die "asciinema 3 is needed to record the demo (nix shell nixpkgs#asciinema)"
	# Waits longer than two seconds are cut to two in the recording.
	# --headless records without a terminal; asciicast v2 plays in any
	# asciinema player.
	exec asciinema rec --overwrite --headless --output-format asciicast-v2 \
		--idle-time-limit 2 --window-size 110x30 \
		--title "battery-operator: a Pool of Firecracker MicroVMs on Kubernetes" \
		--command "${BASH_SOURCE[0]}" "${CAST}"
fi

# say prints a comment line.
say() { printf '\n\033[2m# %s\033[0m\n' "$*"; sleep 1; }

# run shows a command, then runs it.
run() {
	printf '\033[1;32m$\033[0m '
	local c
	for ((c = 0; c < ${#1}; c++)); do
		printf '%s' "${1:c:1}"
		sleep 0.02
	done
	printf '\n'
	sleep 0.4
	eval "$1"
}

[[ "${HOST_DRIVER}" == lima ]] || die "the demo shows limactl commands, so it needs HOST_DRIVER=lima"

# limactl runs on the Mac, through lib.sh from a Linux machine beside it;
# `limactl shell` starts in / so that no directory of this machine is
# looked for in the VM.
limactl() {
	if [[ "$1" == shell ]]; then
		shift
		limactl_ shell --workdir / "$@"
	else
		limactl_ "$@"
	fi
}

export KUBECONFIG="${KUBECONFIG_OUT}"
cd "${REPO_ROOT}"

# The Consumer (hack/real-hosts/smoke) runs on the Host, where the Exec
# Agent's address is reachable; build it before the demo starts.
out="${STATE_DIR}/smoke"
if [[ ! -x "${out}" ]]; then
	(CGO_ENABLED=0 GOOS=linux GOARCH="$(host_arch)" ${GO:-go} build -tags realhosts -o "${out}" ./hack/real-hosts/smoke)
fi
host_put "${out}" /usr/local/bin/bo-trial-smoke 0755
kubectl delete namespace "${NS}" --ignore-not-found --wait >/dev/null
clear

say "One Kubernetes Node with KVM, a Host for Firecracker MicroVMs"
run "kubectl get nodes"
run "kubectl -n battery-operator-system get pods"

say "A Pool asks for two warm MicroVMs; wait until both are available"
run "grep -A 30 'kind: Pool' examples/demo/demo.yaml | head -30"
run "kubectl apply -f examples/demo/demo.yaml"
run "kubectl -n ${NS} wait --for=jsonpath={.status.available}=2 pool/${POOL} --timeout=10m"
run "kubectl -n ${NS} get pool ${POOL}"

say "battery created them through flintlockd: two Firecracker processes on the Host"
run "limactl shell ${NODE_NAME} pgrep -a firecracker | cut -c1-100"

say "A Consumer claims one with the Client Library, runs a command in it, and lets it go"
run "limactl shell ${NODE_NAME} sudo bo-trial-smoke -kubeconfig /etc/rancher/k3s/k3s.yaml -namespace ${NS} -pool ${POOL} -holder ${HOLDER} -command 'uname -a; uptime'"

say "The Pool starts a new MicroVM in place of the one claimed; wait until it is warm"
run "kubectl -n ${NS} wait --for=jsonpath={.status.available}=2 pool/${POOL} --timeout=10m"
run "kubectl -n ${NS} get pool ${POOL}"

say "A claim is a resource too: claim a MicroVM with kubectl"
run "grep -v '^#' examples/demo/claim.yaml"
run "kubectl apply -f examples/demo/claim.yaml"
run "kubectl -n ${NS} wait --for=jsonpath={.status.phase}=Bound microvmclaim/build-1 --timeout=2m"
run "kubectl -n ${NS} get microvmclaim build-1 -o jsonpath='{.status}' | jq '{phase, microVM, host, leaseExpiresAt}'"

say "The holder renews the Lease by setting spec.renewTime; the expiry moves on"
run "kubectl -n ${NS} patch microvmclaim build-1 --type merge -p '{\"spec\":{\"renewTime\":\"'\$(date -u +%Y-%m-%dT%H:%M:%S.000000Z)'\"}}'"
run "sleep 2; kubectl -n ${NS} get microvmclaim build-1 -o jsonpath='{.status.leaseExpiresAt}{\"\\n\"}'"

say "Unrenewed, the Lease runs out after 30s and the claim goes Expired"
run "kubectl -n ${NS} wait --for=jsonpath={.status.phase}=Expired microvmclaim/build-1 --timeout=2m"
run "kubectl -n ${NS} get microvmclaim build-1 -o jsonpath='{.status.conditions[?(@.type==\"Bound\")].message}{\"\\n\"}'"
run "kubectl -n ${NS} delete microvmclaim build-1"

say "Deleting the Pool deletes its MicroVMs"
run "kubectl -n ${NS} delete pool ${POOL}"
run "limactl shell ${NODE_NAME} pgrep -c firecracker || true"
say "Done"
