#!/usr/bin/env bash
# Brings the real hosts trial up (README.md): one Linux machine with KVM
# (a Lima VM, or a machine reached over ssh; lib.sh) as a Host with
# containerd's thin pool, Firecracker and flintlockd; k3s on it;
# cert-manager and the Manifests; and the MicroVM images in the Host's
# registry.
#
# Idempotent: every step checks what is there and does only what is
# missing, so running it again after a failure, or after `limactl stop`,
# carries on.
#
#   up.sh            every step, in order
#   up.sh <step>...  only those: machine host k3s images deploy
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

# ---------------------------------------------------------------------------
# machine: the Lima VM, created and started; or the ssh driver's machine,
# checked.

create_vm() {
	# Only the API server is forwarded to the Mac; no other port is, so the
	# kubelet's and flintlockd's ports stay inside the VM.
	local forwards='[{"guestPort":6443,"hostPort":'"${API_HOST_PORT}"'},{"guestPortRange":[1,65535],"ignore":true}]'
	log "Creating ${NODE_NAME}"
	# Lima's default user-mode network. No mounts: nothing of the Mac's is
	# shared into the Host.
	limactl_ create --tty=false --name="${NODE_NAME}" \
		--vm-type=vz --nested-virt \
		--cpus="${VM_CPUS}" --memory="${VM_MEMORY%GiB}" --disk="${VM_DISK%GiB}" \
		--mount-none \
		--set '.containerd.system = false | .containerd.user = false' \
		--set ".portForwards = ${forwards}" \
		"${VM_TEMPLATE}"
}

step_machine() {
	if [[ "${HOST_DRIVER}" == lima ]]; then
		vm_exists || create_vm
		if [[ "$(vm_status)" != Running ]]; then
			log "Starting ${NODE_NAME}"
			limactl_ start --tty=false "${NODE_NAME}"
		fi
	else
		host_run true || die "cannot reach ${HOST_SSH} over ssh"
		host_run sudo -n true || die "${HOST_SSH}'s user needs sudo without a password"
		host_run sh -c '. /etc/os-release && [ "$ID" = ubuntu ]' ||
			warn "${HOST_SSH} is not Ubuntu; host/provision.sh uses apt and is written for Ubuntu 24.04"
	fi
	host_root test -c /dev/kvm ||
		die "the Host has no /dev/kvm: it needs KVM (on a Mac, an M3 or later and macOS 15 or later; on a cloud instance, nested virtualization)"
	local ip
	ip="$(host_ip)"
	[[ -n "${ip}" ]] || die "the Host has no IPv4 address on $(host_iface)"
	log "Host ${NODE_NAME}: $(host_arch), ${ip} on $(host_iface)"
}

# ---------------------------------------------------------------------------
# host: the Host prerequisites (host/provision.sh).

step_host() {
	local f
	log "Provisioning the machine as a Host"
	for f in "${HERE}"/host/*.service "${HERE}"/host/*.path; do
		host_put "${f}" "/etc/systemd/system/$(basename "${f}")"
	done
	host_put "${HERE}/host/thin-pool.sh" /usr/local/libexec/bo-trial/thin-pool 0755
	host_put "${HERE}/host/flintlockd-run.sh" /usr/local/libexec/bo-trial/flintlockd-run 0755
	host_put "${HERE}/host/containerd-config.toml" /etc/containerd/config.toml
	# flintlockd serves on the Host's address (flintlockd-run.sh reads the
	# interface from here).
	mkdir -p "${STATE_DIR}"
	printf 'FLINTLOCKD_IFACE=%s\n' "$(host_iface)" >"${STATE_DIR}/flintlockd.env"
	host_put "${STATE_DIR}/flintlockd.env" /etc/default/bo-trial-flintlockd
	host_root env \
		CONTAINERD_VERSION="${CONTAINERD_VERSION}" \
		FIRECRACKER_VERSION="${FIRECRACKER_VERSION}" \
		FLINTLOCK_VERSION="${FLINTLOCK_VERSION}" \
		REGISTRY_VERSION="${REGISTRY_VERSION}" \
		bash -s <"${HERE}/host/provision.sh"
}

# ---------------------------------------------------------------------------
# k3s: a server on the Host, on its address, and its Node labelled as a
# Host.

step_k3s() {
	local ip iface api
	ip="$(host_ip)"
	iface="$(host_iface)"
	api="$(api_address)"
	# k3s's own containerd is separate from flintlockd's: it has its own
	# socket (/run/k3s/containerd) and state, so the kubelet's pods and the
	# MicroVMs never share a snapshotter. Traefik and ServiceLB are not
	# needed. ServiceAccountTokenPodNodeInfo, which the Exec Agent's
	# admission policy relies on, is on by default from Kubernetes 1.32.
	if ! host_root test -x /usr/local/bin/k3s; then
		log "Installing the k3s server"
		host_root env INSTALL_K3S_VERSION="${K3S_VERSION}" \
			INSTALL_K3S_EXEC="server --node-name=${NODE_NAME} --node-ip=${ip} --advertise-address=${ip} --flannel-iface=${iface} --tls-san=127.0.0.1 --tls-san=${ip} --tls-san=${api} --disable=traefik --disable=servicelb --write-kubeconfig-mode=0600" \
			sh -c 'curl -sfL https://get.k3s.io | sh -'
	fi
	host_root systemctl is-active --quiet k3s || host_root systemctl start k3s

	mkdir -p "${STATE_DIR}"
	# With the lima driver the kubeconfig reaches the API server through
	# Lima's forward of 6443 to the Mac's API_HOST_PORT; from a Linux machine
	# beside the Mac (OrbStack), the Mac's loopback is host.internal. With
	# the ssh driver it reaches the machine's 6443 directly.
	local server
	case "${HOST_DRIVER}" in
	lima) server="https://${api}:${API_HOST_PORT}" ;;
	ssh) server="https://${api}:${HOST_API_PORT:-6443}" ;;
	esac
	host_root cat /etc/rancher/k3s/k3s.yaml |
		sed -e "s#https://127.0.0.1:6443#${server}#" >"${KUBECONFIG_OUT}"
	chmod 0600 "${KUBECONFIG_OUT}"
	if [[ "${HOST_DRIVER}" == lima && "${api}" != 127.0.0.1 ]]; then
		# A k3s installed before --tls-san named ${api} has a certificate for
		# 127.0.0.1 and the VM's address only; connect to the Mac but verify
		# the certificate as 127.0.0.1.
		KUBECONFIG="${KUBECONFIG_OUT}" kubectl config set-cluster default --tls-server-name=127.0.0.1 >/dev/null
	fi

	log "Waiting for the Node"
	local i
	for i in $(seq 1 60); do
		kubectl_ get node "${NODE_NAME}" >/dev/null 2>&1 && break
		sleep 5
	done
	kubectl_ wait --for=condition=Ready "node/${NODE_NAME}" --timeout=120s
	kubectl_ label node "${NODE_NAME}" battery.liquidmetal-x.dev/host=true --overwrite
}

# ---------------------------------------------------------------------------
# images: the MicroVM kernel and root filesystem, built for the Host's
# architecture and pushed to its registry.

step_images() {
	local out="${STATE_DIR}/images" arch
	arch="$(host_arch)"
	mkdir -p "${out}"
	command -v docker >/dev/null || die "docker is needed to build the MicroVM images"
	log "Building the MicroVM kernel image for linux/${arch}"
	docker build --platform "linux/${arch}" -f "${HERE}/images/kernel/Containerfile" -t bo-trial/kernel "${HERE}/images/kernel"
	log "Building the MicroVM root filesystem image for linux/${arch}"
	docker build --platform "linux/${arch}" --build-arg GUEST_AGENT_VERSION="${GUEST_AGENT_VERSION}" \
		-f "${HERE}/images/rootfs/Containerfile" -t bo-trial/rootfs "${HERE}/images/rootfs"
	docker save --platform "linux/${arch}" bo-trial/kernel -o "${out}/kernel.tar" 2>/dev/null ||
		docker save bo-trial/kernel -o "${out}/kernel.tar"
	docker save --platform "linux/${arch}" bo-trial/rootfs -o "${out}/rootfs.tar" 2>/dev/null ||
		docker save bo-trial/rootfs -o "${out}/rootfs.tar"
	log "Pushing the MicroVM images to the Host's registry"
	host_root ctr -n bo-trial images import --platform "linux/${arch}" - <"${out}/kernel.tar" >/dev/null
	host_root ctr -n bo-trial images import --platform "linux/${arch}" - <"${out}/rootfs.tar" >/dev/null
	host_root ctr -n bo-trial images tag --force docker.io/bo-trial/kernel:latest "${KERNEL_IMAGE}" >/dev/null
	host_root ctr -n bo-trial images tag --force docker.io/bo-trial/rootfs:latest "${ROOTFS_IMAGE}" >/dev/null
	host_root ctr -n bo-trial images push --plain-http "${KERNEL_IMAGE}" >/dev/null
	host_root ctr -n bo-trial images push --plain-http "${ROOTFS_IMAGE}" >/dev/null
	# The import was only the way into the registry; flintlockd pulls into
	# its own namespace, flintlock.
	host_root sh -c 'ctr -n bo-trial images ls -q | xargs -r ctr -n bo-trial images rm >/dev/null; ctr -n bo-trial content prune references >/dev/null 2>&1 || true'
}

# ---------------------------------------------------------------------------
# deploy: cert-manager, then the Manifests through the overlay in
# manifests/.

step_deploy() {
	log "Installing cert-manager ${CERT_MANAGER_VERSION}"
	kubectl_ apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml" >/dev/null
	kubectl_ -n cert-manager wait --for=condition=Available deployment --all --timeout=300s

	log "Deploying battery-operator at ${IMAGE_TAG}"
	local kustomize="${KUSTOMIZE:-kubectl kustomize}"
	local rendered="${STATE_DIR}/manifests.yaml"
	(cd "${HERE}/manifests" && ${kustomize} .) |
		sed -e "s#ghcr.io/phoban01/battery-operator:latest#ghcr.io/phoban01/battery-operator:${IMAGE_TAG}#" \
			-e "s#ghcr.io/phoban01/battery-operator/exec-agent:latest#ghcr.io/phoban01/battery-operator/exec-agent:${IMAGE_TAG}#" \
			>"${rendered}"
	# The CRDs and the namespace first, so that the objects of their kinds
	# apply in the same run. On a second run the Operator owns battery's
	# config.json, which it rewrote with the Hosts (IN-014); the apply takes
	# it back, and the Operator writes the Hosts back again.
	kubectl_ apply --server-side --force-conflicts -f "${rendered}" >/dev/null ||
		kubectl_ apply --server-side --force-conflicts -f "${rendered}" >/dev/null
	kubectl_ -n battery-operator-system rollout status deployment/battery-operator-controller-manager --timeout=300s
	kubectl_ -n battery-operator-system rollout status daemonset/battery-operator-exec-agent --timeout=300s

	log "Waiting for the Host to report ready"
	local i ready
	for i in $(seq 1 60); do
		ready="$(kubectl_ get node "${NODE_NAME}" -o jsonpath='{.metadata.annotations.battery\.liquidmetal-x\.dev/exec-agent-ready}')"
		[[ "${ready}" == true ]] && break
		sleep 5
	done
	[[ "${ready}" == true ]] || {
		kubectl_ get node "${NODE_NAME}" -o jsonpath='{.metadata.annotations}' >&2
		echo >&2
		die "${NODE_NAME} is not ready as a Host"
	}
	log "The Host is ready"
}

main() {
	local steps=("$@")
	[[ ${#steps[@]} -gt 0 ]] || steps=(machine host k3s images deploy)
	local s
	for s in "${steps[@]}"; do
		case "${s}" in
		machine | host | k3s | images | deploy) "step_${s}" ;;
		*) die "unknown step ${s}: machine host k3s images deploy" ;;
		esac
	done
}

keep_awake
main "$@"
