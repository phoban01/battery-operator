#!/usr/bin/env bash
# Takes the real hosts trial down (README.md). With the lima driver it
# deletes the VM, NODE_NAME (bo-host-1), and no other Lima VM. With the ssh
# driver it leaves the machine alone: it did not create it, and removing
# what up.sh installed is the machine owner's call (k3s-uninstall.sh
# removes k3s). Either way it deletes the state up.sh wrote under .state.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

if [[ "${HOST_DRIVER}" == lima ]]; then
	# Only the trial's own VM, by exact name.
	[[ "${NODE_NAME}" == bo-host-[0-9] ]] || die "refusing to delete ${NODE_NAME}: not a trial VM"
	if vm_exists; then
		log "Deleting ${NODE_NAME}"
		limactl_ stop --force "${NODE_NAME}" >/dev/null 2>&1 || true
		limactl_ delete --force "${NODE_NAME}"
	fi
	log "The trial's VM is gone"
else
	log "Leaving ${HOST_SSH} as it is; /usr/local/bin/k3s-uninstall.sh on it removes k3s and the trial's workloads"
fi
rm -rf "${STATE_DIR}"
