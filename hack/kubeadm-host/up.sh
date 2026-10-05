#!/usr/bin/env bash
# Brings the kubeadm Host proof up (README.md): a kubeadm control plane in a
# Lima VM, the Host Image made into a disk and booted in a vfkit VM with
# user-data rendered from config/capi's KubeadmConfigTemplate, the Host
# joined, the MicroVM images on it and battery-operator deployed.
#
# Idempotent: each step checks what is there and does only what is missing.
#
#   up.sh            every step, in order
#   up.sh <step>...  only those: controlplane image disk host join images deploy
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

STEPS=(controlplane image disk host join images deploy)

# ---------------------------------------------------------------------------
# controlplane: a Lima VM on the Mac's shared network, with kubeadm and
# Calico.

step_controlplane() {
	if ! cp_exists; then
		log "Creating ${CP_NAME}"
		# vzNAT puts the VM on the Mac's shared network (lima0), where the
		# Host is too, and which the Mac and OrbStack reach directly. So no
		# port is forwarded.
		local forwards='[{"guestPortRange":[1,65535],"ignore":true}]'
		limactl_ create --tty=false --name="${CP_NAME}" --vm-type=vz --network=vzNAT \
			--cpus="${CP_CPUS}" --memory="${CP_MEMORY%GiB}" --disk="${CP_DISK%GiB}" \
			--mount-none \
			--set '.containerd.system = false | .containerd.user = false' \
			--set ".portForwards = ${forwards}" \
			"${CP_TEMPLATE}"
	fi
	if [[ "$(cp_status)" != Running ]]; then
		log "Starting ${CP_NAME}"
		limactl_ start --tty=false "${CP_NAME}"
	fi
	local ip gw
	ip="$(cp_ip)"
	gw="$(shared_gateway)"
	[[ -n "${ip}" && -n "${gw}" ]] || die "${CP_NAME} has no address on the shared network (lima0)"
	log "Making the kubeadm control plane on ${ip}, Kubernetes ${KUBERNETES_VERSION}"
	cp_root env KUBERNETES_VERSION="${KUBERNETES_VERSION}" NODE_IP="${ip}" GATEWAY_IP="${gw}" \
		POD_CIDR="${POD_CIDR}" SERVICE_CIDR="${SERVICE_CIDR}" CALICO_VERSION="${CALICO_VERSION}" LOCAL_PATH_VERSION="${LOCAL_PATH_VERSION}" \
		bash -s <"${KH_HERE}/controlplane/provision.sh"

	# kubeadm's admin kubeconfig, which names the API server by its address
	# on the shared network.
	mkdir -p "${STATE_DIR}"
	(umask 077 && cp_root cat /etc/kubernetes/admin.conf >"${KUBECONFIG_OUT}")
	kubectl_ get nodes -o wide
}

# ---------------------------------------------------------------------------
# image: the Host Image, built from this checkout for the Mac's
# architecture and loaded into Docker.

step_image() {
	if docker image inspect "${HOST_IMG}" >/dev/null 2>&1 && [[ -z "${REBUILD_IMAGE:-}" ]]; then
		log "Using ${HOST_IMG}, already in Docker (REBUILD_IMAGE=1 builds it again)"
		return
	fi
	log "Building the Host Image for linux/arm64 as ${HOST_IMG}"
	(cd "${REPO_ROOT}" && ${MAKE:-make} host-image HOST_IMAGE_PLATFORM=linux/arm64 HOST_IMG="${HOST_IMG}")
}

# ---------------------------------------------------------------------------
# disk: the Host Image installed onto a raw disk with `bootc install
# to-disk`, run from the image itself, and a blank disk for the thin pool.

step_disk() {
	local root="${LOCAL_WORK_DIR}/host-root.raw" thin="${LOCAL_WORK_DIR}/host-thin-pool.raw"
	mkdir -p "${LOCAL_WORK_DIR}" "${STATE_DIR}"
	if [[ -f "${root}" && -z "${REBUILD_DISK:-}" ]]; then
		log "Using the Host's disk ${MAC_WORK_DIR}/host-root.raw (REBUILD_DISK=1 makes it again)"
	else
		log "Installing ${HOST_IMG} onto a ${HOST_ROOT_GIB} GiB disk"
		local work="${STATE_DIR}/disk"
		rm -rf "${work}"
		mkdir -p "${work}"
		docker save "${HOST_IMG}" -o "${work}/host-image.tar"
		truncate -s "${HOST_ROOT_GIB}G" "${work}/host-root.raw"
		# bootc reads the image from the archive, so it needs no podman
		# storage. --generic-image installs the firmware's fallback boot path,
		# which vfkit's EFI needs; console=hvc0 sends the console to vfkit's
		# serial log. SELinux labels come from the image's own policy.
		docker run --rm --privileged --security-opt label=disable \
			-v /dev:/dev -v "${work}:/work" \
			--entrypoint bootc "${HOST_IMG}" \
			install to-disk --via-loopback --generic-image --wipe \
			--source-imgref "oci-archive:/work/host-image.tar" \
			--target-imgref "${HOST_IMG}" \
			--filesystem xfs --karg console=hvc0 \
			/work/host-root.raw
		rm -f "${work}/host-image.tar"
		# Sparse, so that the Mac's copy takes only what is written. dd skips
		# the zero blocks of a new file; cp --sparse punches holes, which
		# OrbStack's mount of the Mac refuses.
		rm -f "${root}"
		dd if="${work}/host-root.raw" of="${root}" bs=1M conv=sparse status=none
		rm -rf "${work}"
		# A new machine: new firmware variables and a blank thin pool disk.
		rm -f "${LOCAL_WORK_DIR}/efi-vars" "${thin}"
	fi
	if [[ ! -f "${thin}" ]]; then
		# Blank: the Host Image makes the thin pool only on a blank device.
		truncate -s "${HOST_THIN_POOL_GIB}G" "${thin}"
	fi
}

# ---------------------------------------------------------------------------
# host: the user-data a Cluster API MachineDeployment would give the Host,
# rendered from config/capi's templates, and the Host booted with it.

# pool_rendered builds the proof's Host pool: config/capi's host-pool and
# host-pool-params, as a pool under config/capi/pools/ builds them, with
# this network's settings and the ssh user.
pool_rendered() {
	local pool="${STATE_DIR}/pool" cidr pub
	cidr="$(shared_cidr)"
	[[ -n "${cidr}" ]] || die "cannot tell the shared network's range from ${CP_NAME}"
	pub="$(cat "${SSH_KEY}.pub")"
	rm -rf "${pool}"
	mkdir -p "${pool}"
	sed -e "s#@NODE_CIDR@#${cidr}#g" -e "s#@POD_CIDR@#${POD_CIDR}#g" -e "s#@SERVICE_CIDR@#${SERVICE_CIDR}#g" \
		-e "s#@KUBERNETES_VERSION@#${KUBERNETES_VERSION}#g" -e "s#@NODE_NAME@#${NODE_NAME}#g" \
		"${KH_HERE}/pool/host-pool.yaml" >"${pool}/host-pool.yaml"
	sed -e "s#@HOST_USER@#${HOST_USER}#g" -e "s#@SSH_PUBLIC_KEY@#${pub}#g" \
		"${KH_HERE}/pool/users.yaml" >"${pool}/users.yaml"
	sed -e "s#@CP_IP@#$(cp_ip)#g" -e "s#@GATEWAY_IP@#$(shared_gateway)#g" \
		"${KH_HERE}/pool/network.yaml" >"${pool}/network.yaml"
	# The kustomization names config/capi by a path from the state
	# directory.
	sed -e "s#@CAPI@#$(realpath --relative-to="${pool}" "${REPO_ROOT}/config/capi")#g" \
		"${KH_HERE}/pool/kustomization.yaml" >"${pool}/kustomization.yaml"
	${KUSTOMIZE:-kubectl kustomize} "${pool}"
}

render_user_data() {
	local ip token hash
	ip="$(cp_ip)"
	token="$(cp_root kubeadm token create --ttl 2h)"
	hash="sha256:$(cp_root sh -c "openssl x509 -pubkey -in /etc/kubernetes/pki/ca.crt | openssl rsa -pubin -outform der 2>/dev/null | sha256sum | cut -d' ' -f1")"
	pool_rendered >"${STATE_DIR}/pool.yaml"
	(cd "${REPO_ROOT}" && ${GO:-go} run -tags kubeadmhost ./hack/kubeadm-host/userdata \
		-api-server "${ip}:6443" -token "${token}" -ca-cert-hash "${hash}") \
		<"${STATE_DIR}/pool.yaml" >"${STATE_DIR}/user-data"
	# The NoCloud meta-data: CAPA's template names the Node after
	# ds.meta_data.local_hostname, which on AWS is the instance's private DNS
	# name and here is NODE_NAME.
	printf 'instance-id: %s\nlocal-hostname: %s\n' "${NODE_NAME}-$(date +%s)" "${NODE_NAME}" >"${STATE_DIR}/meta-data"
	cp "${STATE_DIR}/user-data" "${STATE_DIR}/meta-data" "${LOCAL_WORK_DIR}/"
}

vfkit_running() {
	mac_run sh -c "test -f '${MAC_WORK_DIR}/vfkit.pid' && kill -0 \$(cat '${MAC_WORK_DIR}/vfkit.pid') 2>/dev/null"
}

install_vfkit() {
	local bin="${LOCAL_WORK_DIR}/vfkit"
	if [[ -x "${bin}" ]] && sha256sum "${bin}" | grep -q "^${VFKIT_SHA256} "; then return; fi
	log "Downloading vfkit ${VFKIT_VERSION}"
	curl -fsSL -o "${bin}.tmp" "https://github.com/crc-org/vfkit/releases/download/${VFKIT_VERSION}/vfkit"
	sha256sum "${bin}.tmp" | grep -q "^${VFKIT_SHA256} " || { rm -f "${bin}.tmp"; die "vfkit ${VFKIT_VERSION}'s checksum is not ${VFKIT_SHA256}"; }
	chmod 0755 "${bin}.tmp"
	mv "${bin}.tmp" "${bin}"
}

step_host() {
	[[ -f "${LOCAL_WORK_DIR}/host-root.raw" ]] || die "no Host disk: run up.sh disk"
	if vfkit_running; then
		log "The Host ${NODE_NAME} is running"
		return
	fi
	mkdir -p "${STATE_DIR}"
	[[ -f "${SSH_KEY}" ]] || ssh-keygen -q -t ed25519 -N '' -C "bo-kubeadm-host" -f "${SSH_KEY}"
	install_vfkit
	log "Rendering the Host's user-data from config/capi's KubeadmConfigTemplate"
	render_user_data
	local d="${MAC_WORK_DIR}" efi=",create"
	[[ -f "${LOCAL_WORK_DIR}/efi-vars" ]] && efi=""
	log "Booting the Host ${NODE_NAME}: ${HOST_CPUS} CPUs, ${HOST_MEMORY_MIB} MiB, nested virtualization"
	# The root disk is vda and the thin pool's vdb, which the pool's
	# host.conf names.
	mac_run sh -c "cd '${d}' || exit 1; rm -f console.log; nohup ./vfkit --cpus ${HOST_CPUS} --memory ${HOST_MEMORY_MIB} --nested \
		--bootloader efi,variable-store=efi-vars${efi} \
		--device virtio-blk,path=host-root.raw \
		--device virtio-blk,path=host-thin-pool.raw \
		--device virtio-net,nat,mac=${HOST_MAC_ADDRESS} \
		--device virtio-rng \
		--device virtio-serial,logFilePath=console.log \
		--cloud-init user-data,meta-data \
		--restful-uri unix://${d}/vfkit.sock \
		>vfkit.log 2>&1 & echo \$! >vfkit.pid"
	sleep 3
	vfkit_running || { cat "${LOCAL_WORK_DIR}/vfkit.log" >&2; die "vfkit did not start"; }
}

# ---------------------------------------------------------------------------
# join: cloud-init runs the rendered `kubeadm join`; wait for the Node.

# stand_in_for_ccm does the two things the AWS cloud controller manager does
# for a new Node that matter here. The template starts the kubelet with
# --cloud-provider=external, so the kubelet leaves the Node's addresses
# empty and taints it node.cloudprovider.kubernetes.io/uninitialized until a
# cloud controller manager initializes it. There is none here, so this sets
# the InternalIP and removes the taint.
stand_in_for_ccm() {
	local ip="$1"
	log "Initializing ${NODE_NAME} as a cloud controller manager would: InternalIP ${ip}"
	kubectl_ patch node "${NODE_NAME}" --subresource=status --type=merge \
		-p "{\"status\":{\"addresses\":[{\"type\":\"InternalIP\",\"address\":\"${ip}\"},{\"type\":\"Hostname\",\"address\":\"${NODE_NAME}\"}]}}" >/dev/null
	kubectl_ taint node "${NODE_NAME}" node.cloudprovider.kubernetes.io/uninitialized- >/dev/null 2>&1 || true
}

step_join() {
	log "Waiting for the Host's address"
	local ip
	for _ in $(seq 1 60); do
		ip="$(host_address)"
		[[ -n "${ip}" ]] && break
		sleep 2
	done
	use_host
	# The control plane reaches the Host through the Mac, as the Host
	# reaches it (pool/network.yaml). kubeadm join retries its discovery
	# until this is in place.
	cp_root ip route replace "${ip}/32" via "$(shared_gateway)" dev lima0
	log "Waiting for ${NODE_NAME} to join the cluster"
	for _ in $(seq 1 120); do
		kubectl_ get node "${NODE_NAME}" >/dev/null 2>&1 && break
		sleep 5
	done
	kubectl_ get node "${NODE_NAME}" >/dev/null 2>&1 || {
		tail -50 "${LOCAL_WORK_DIR}/console.log" >&2 || true
		die "${NODE_NAME} did not join"
	}
	stand_in_for_ccm "${ip}"
	kubectl_ wait --for=condition=Ready "node/${NODE_NAME}" --timeout=600s
	host_root test -f /run/cluster-api/bootstrap-success.complete ||
		die "cloud-init did not write CABPK's success file"
	host_root grep -h "This node has joined the cluster" /var/log/cloud-init-output.log >&2 || true
	kubectl_ get node -o wide
	kubectl_ get node "${NODE_NAME}" -o jsonpath='{range .spec.taints[*]}{.key}={.value}:{.effect}{"\n"}{end}' >&2
	# The smoke test's Consumer runs on the Host and acts for the Holder
	# with a kubeconfig that may request the Holder's tokens.
	cp_root cat /etc/kubernetes/admin.conf >"${STATE_DIR}/admin.conf"
	host_put "${STATE_DIR}/admin.conf" /etc/kubernetes/bo-proof-admin.conf 0600
	rm -f "${STATE_DIR}/admin.conf"
}

# ---------------------------------------------------------------------------
# images and deploy: the real hosts trial's steps, against this Host over
# ssh. The Host Image has no registry, so the trial's runs as a container
# on the Host, on 127.0.0.1:5000, where flintlockd's containerd pulls from.

real_hosts() {
	STATE_DIR="${STATE_DIR}" NODE_NAME="${NODE_NAME}" HOST_DRIVER=ssh HOST_SSH="${HOST_SSH}" SSH_OPTS="${SSH_OPTS}" \
		"$@"
}

step_images() {
	use_host
	log "Starting a registry on the Host's 127.0.0.1:5000"
	host_root podman run -d --replace --name bo-registry --network host \
		-e REGISTRY_HTTP_ADDR=127.0.0.1:5000 "docker.io/library/registry:${REGISTRY_VERSION}" >/dev/null
	real_hosts "${KH_HERE}/../real-hosts/up.sh" images
}

step_deploy() {
	use_host
	real_hosts "${KH_HERE}/../real-hosts/up.sh" deploy
	kubectl_ get node "${NODE_NAME}" -o jsonpath='{.metadata.annotations}' | tr ',' '\n' | grep -i battery >&2 || true
}

main() {
	local steps=("$@")
	[[ ${#steps[@]} -gt 0 ]] || steps=("${STEPS[@]}")
	local s
	for s in "${steps[@]}"; do
		case " ${STEPS[*]} " in
		*" ${s} "*) "step_${s}" ;;
		*) die "unknown step ${s}: ${STEPS[*]}" ;;
		esac
	done
}

keep_mac_awake
main "$@"
