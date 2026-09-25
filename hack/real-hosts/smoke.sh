#!/usr/bin/env bash
# The real hosts trial's smoke test (README.md), against what up.sh brought
# up. Each step says what it proves and fails the run when it does not hold.
#
#   1. A Pool of two MicroVMs becomes Ready, and two Firecracker processes
#      run on the Host.
#   2. The Client Library claims one, runs `uname -a` in it through the Exec
#      Agent on the Host, and releases it (smoke/main.go, run on the Host).
#   3. A MicroVM survives a restart of the Host's flintlockd, and the Pool
#      stays Ready and serves a claim afterwards.
#   4. Deleting the Pool deletes its MicroVMs: the Pool goes, and no
#      Firecracker process is left.
#
#   smoke.sh           every step
#   smoke.sh <step>... only those: pool claim restart delete
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

NS=bo-trial
POOL=trial
POOL_SIZE=2

# firecrackers prints "<pid> <microvm id>" for every Firecracker process on
# the Host.
firecrackers() {
	host_root sh -c 'for p in $(pgrep -x firecracker); do printf "%s %s\n" "$p" "$(tr "\0" " " </proc/$p/cmdline | grep -o "\-\-id [^ ]*" | cut -d" " -f2)"; done'
}

wait_pool_ready() {
	log "Waiting for Pool ${POOL} to be Ready"
	if ! kubectl_ -n "${NS}" wait --for=condition=Ready "pool/${POOL}" --timeout="${1:-600s}"; then
		kubectl_ -n "${NS}" get pool "${POOL}" -o yaml >&2
		die "Pool ${POOL} is not Ready"
	fi
	kubectl_ -n "${NS}" get pool "${POOL}" -o wide
}

step_pool() {
	log "Creating the Holder and Pool ${POOL} (${POOL_SIZE} MicroVMs)"
	kubectl_ apply -f "${HERE}/smoke/trial.yaml"
	wait_pool_ready 900s
	# Not `tee /dev/stderr`: when stderr is a file, tee reopens and truncates
	# it, which wipes the log of a run redirected to one.
	local fcs n
	fcs="$(firecrackers)"
	printf '%s\n' "${fcs}" >&2
	n="$(grep -c . <<<"${fcs}" || true)"
	[[ "${n}" -eq "${POOL_SIZE}" ]] || die "${n} Firecracker processes on the Host, want ${POOL_SIZE}"
	log "PASS: Pool ${POOL} is Ready with ${n} MicroVMs running under Firecracker"
}

build_smoke() {
	local out="${STATE_DIR}/smoke" arch
	arch="$(host_arch)"
	log "Building the Consumer (smoke/main.go) for linux/${arch}"
	(cd "${REPO_ROOT}" && CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" ${GO:-go} build -tags realhosts -o "${out}" ./hack/real-hosts/smoke)
	host_put "${out}" /usr/local/bin/bo-trial-smoke 0755
}

# claim_once runs the Consumer on the Host, where the Exec Agent's address
# is reachable, with k3s's admin kubeconfig to request the Holder's tokens.
claim_once() {
	host_root /usr/local/bin/bo-trial-smoke -kubeconfig /etc/rancher/k3s/k3s.yaml \
		-namespace "${NS}" -pool "${POOL}" -holder trial-holder -command "uname -a"
}

step_claim() {
	build_smoke
	log "Claiming a MicroVM, running uname -a in it through the Exec Agent, releasing it"
	claim_once || die "the Consumer failed"
	local left
	left="$(kubectl_ -n "${NS}" get microvmclaims --no-headers 2>/dev/null | wc -l)"
	[[ "${left}" -eq 0 ]] || die "${left} claims left after the release"
	log "PASS: claimed, ran uname -a through the Exec Agent, released"
	wait_pool_ready 600s
}

step_restart() {
	# The first line, without `| head -1`: head closing the pipe early kills
	# the command on the Host with SIGPIPE, and pipefail ends the script.
	local line pid id
	line="$(firecrackers)"
	line="${line%%$'\n'*}"
	[[ -n "${line}" ]] || die "no MicroVM is running to restart flintlockd under"
	read -r pid id <<<"${line}"
	log "Restarting flintlockd under MicroVM ${id} (Firecracker pid ${pid})"
	host_root systemctl restart flintlockd.service
	local i
	for i in $(seq 1 30); do
		host_root systemctl is-active --quiet flintlockd.service && break
		sleep 2
	done
	host_root systemctl is-active --quiet flintlockd.service || die "flintlockd did not come back"
	# Give flintlockd's reconciler a resync to act on what it finds.
	sleep 30
	if host_root kill -0 "${pid}" 2>/dev/null; then
		log "PASS: Firecracker pid ${pid} of MicroVM ${id} survived the restart of flintlockd"
	else
		warn "FAIL: Firecracker pid ${pid} of MicroVM ${id} is gone after the restart of flintlockd"
		host_root journalctl -u flintlockd --since=-2min --no-pager | tail -40 >&2
		RESTART_FAILED=1
	fi
	firecrackers >&2
	wait_pool_ready 600s
	log "Claiming again after the restart"
	[[ -x "${STATE_DIR}/smoke" ]] || build_smoke
	claim_once || die "the Consumer failed after the restart of flintlockd"
	[[ -z "${RESTART_FAILED:-}" ]] || die "a MicroVM did not survive the restart of flintlockd"
	log "PASS: the Pool is Ready and serves a claim after the restart of flintlockd"
}

step_delete() {
	log "Deleting Pool ${POOL}"
	kubectl_ -n "${NS}" delete pool "${POOL}" --wait=false
	if ! kubectl_ -n "${NS}" wait --for=delete "pool/${POOL}" --timeout=600s; then
		kubectl_ -n "${NS}" get pool "${POOL}" -o yaml >&2
		die "Pool ${POOL} was not deleted"
	fi
	local i n
	for i in $(seq 1 30); do
		n="$(firecrackers | wc -l)"
		[[ "${n}" -eq 0 ]] && break
		sleep 2
	done
	[[ "${n}" -eq 0 ]] || { firecrackers >&2; die "${n} Firecracker processes left after the Pool was deleted"; }
	log "PASS: Pool ${POOL} is gone, and so are its MicroVMs"
}

main() {
	local steps=("$@")
	[[ ${#steps[@]} -gt 0 ]] || steps=(pool claim restart delete)
	local s
	for s in "${steps[@]}"; do
		case "${s}" in
		pool | claim | restart | delete) "step_${s}" ;;
		*) die "unknown step ${s}: pool claim restart delete" ;;
		esac
	done
}

keep_awake
main "$@"
