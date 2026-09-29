#!/usr/bin/env bash
# Takes the kubeadm Host proof down (README.md): stops the Host's vfkit VM,
# deletes the control plane VM, and deletes the Host's disks on the Mac and
# the state under .state. It touches no other VM. KEEP_DISK=1 keeps the
# Host Image's disk, so that the next up.sh skips making it; the Host then
# boots from where it left off, and a fresh start needs it deleted.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

pid_file="${MAC_WORK_DIR}/vfkit.pid"
if mac_run sh -c "test -f '${pid_file}'"; then
	log "Stopping the Host ${NODE_NAME}"
	mac_run sh -c "kill \$(cat '${pid_file}') 2>/dev/null; for i in 1 2 3 4 5 6 7 8 9 10; do kill -0 \$(cat '${pid_file}') 2>/dev/null || break; sleep 1; done; kill -9 \$(cat '${pid_file}') 2>/dev/null; rm -f '${pid_file}'" || true
fi

# Only the proof's own control plane, by exact name.
[[ "${CP_NAME}" == bo-kubeadm-cp* ]] || die "refusing to delete ${CP_NAME}: not the proof's VM"
if cp_exists; then
	log "Deleting ${CP_NAME}"
	limactl_ stop --force "${CP_NAME}" >/dev/null 2>&1 || true
	limactl_ delete --force "${CP_NAME}"
fi

if [[ -d "${LOCAL_WORK_DIR}" ]]; then
	log "Deleting the Host's files in ${MAC_WORK_DIR}"
	keep=()
	[[ -n "${KEEP_DISK:-}" ]] && keep=(! -name host-root.raw)
	find "${LOCAL_WORK_DIR}" -maxdepth 1 -type f ! -name vfkit "${keep[@]}" -delete
fi
rm -rf "${STATE_DIR}"
log "The proof's VMs are gone"
