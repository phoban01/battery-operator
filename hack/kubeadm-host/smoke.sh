#!/usr/bin/env bash
# The kubeadm Host proof's smoke test (README.md): the pod network on the
# Host, then the real hosts trial's smoke.sh run against the Host over ssh,
# then the guest network's isolation from the pod network. The trial's
# Consumer runs on the Host with the control plane's admin kubeconfig, which
# up.sh's join step put there, from /var: the Host's /usr is read-only.
#
#   smoke.sh           every step: pods pool claim network isolation restart delete
#   smoke.sh <step>... only those
#
# pods and isolation are this proof's own steps; the rest are the trial's.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

STEPS=(pods pool claim network isolation restart delete)
CONSUMER_KUBECONFIG=/etc/kubernetes/bo-proof-admin.conf
CONSUMER_BIN=/var/lib/bo-proof/bo-trial-smoke
# The pod network step's namespace and image. agnhost serves HTTP and has
# curl and nslookup.
PODNET_NS=bo-podnet
AGNHOST_IMAGE="${AGNHOST_IMAGE:-registry.k8s.io/e2e-test-images/agnhost:2.53}"

trial() {
	STATE_DIR="${STATE_DIR}" NODE_NAME="${NODE_NAME}" HOST_DRIVER=ssh HOST_SSH="${HOST_SSH}" SSH_OPTS="${SSH_OPTS}" \
		CONSUMER_KUBECONFIG="${CONSUMER_KUBECONFIG}" CONSUMER_BIN="${CONSUMER_BIN}" \
		"${KH_HERE}/../real-hosts/smoke.sh" "$@"
}

# podnet_objects are two echo servers, one on the control plane and one on
# the Host, and a ClusterIP Service in front of the first. Neither pod uses
# the host's network. The Host's pod tolerates the Host taint and selects
# the Host label, as the Exec Agent does.
podnet_objects() {
	cat <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: ${PODNET_NS}
---
apiVersion: v1
kind: Pod
metadata:
  name: cp-echo
  namespace: ${PODNET_NS}
  labels:
    app: cp-echo
spec:
  nodeSelector:
    node-role.kubernetes.io/control-plane: ""
  tolerations:
    - key: node-role.kubernetes.io/control-plane
      operator: Exists
      effect: NoSchedule
  containers:
    - name: echo
      image: ${AGNHOST_IMAGE}
      args: ["netexec", "--http-port=8080"]
      ports:
        - containerPort: 8080
---
apiVersion: v1
kind: Service
metadata:
  name: cp-echo
  namespace: ${PODNET_NS}
spec:
  type: ClusterIP
  selector:
    app: cp-echo
  ports:
    - port: 80
      targetPort: 8080
---
apiVersion: v1
kind: Pod
metadata:
  name: host-echo
  namespace: ${PODNET_NS}
spec:
  nodeSelector:
    battery.liquidmetal-x.dev/host: "true"
  tolerations:
    - key: battery.liquidmetal-x.dev/host
      operator: Equal
      value: "true"
      effect: NoSchedule
  containers:
    - name: echo
      image: ${AGNHOST_IMAGE}
      args: ["netexec", "--http-port=8080"]
      ports:
        - containerPort: 8080
EOF
}

pod_ip() { kubectl_ -n "${PODNET_NS}" get pod "$1" -o jsonpath='{.status.podIP}'; }
service_ip() { kubectl_ -n "${PODNET_NS}" get service cp-echo -o jsonpath='{.spec.clusterIP}'; }
in_host_pod() { kubectl_ -n "${PODNET_NS}" exec host-echo -- "$@"; }

# in_cidr reports whether the IPv4 address $1 is in the range $2.
in_cidr() {
	local ip="$1" net="${2%/*}" bits="${2#*/}" a b c d
	IFS=. read -r a b c d <<<"${ip}"
	local x=$(((a << 24) | (b << 16) | (c << 8) | d))
	IFS=. read -r a b c d <<<"${net}"
	local y=$(((a << 24) | (b << 16) | (c << 8) | d))
	local mask=$(((0xffffffff << (32 - bits)) & 0xffffffff))
	[[ $((x & mask)) -eq $((y & mask)) ]]
}

# step_pods checks the pod network on the Host: a pod without the host's
# network gets an address from the pod range, reaches a pod on the control
# plane by its address and through a ClusterIP Service, and resolves the
# Service's name through the cluster's DNS.
step_pods() {
	log "Starting an echo pod on the control plane and one on ${NODE_NAME}, neither on the host's network"
	podnet_objects | kubectl_ apply -f - >/dev/null
	if ! kubectl_ -n "${PODNET_NS}" wait --for=condition=Ready pod/cp-echo pod/host-echo --timeout=300s; then
		kubectl_ -n "${PODNET_NS}" get pods -o wide >&2
		kubectl_ -n "${PODNET_NS}" describe pod host-echo >&2
		die "the echo pods are not Ready"
	fi
	kubectl_ -n "${PODNET_NS}" get pods -o wide
	local node ip cp_ip svc out
	node="$(kubectl_ -n "${PODNET_NS}" get pod host-echo -o jsonpath='{.spec.nodeName}')"
	[[ "${node}" == "${NODE_NAME}" ]] || die "host-echo runs on ${node}, not ${NODE_NAME}"
	[[ "$(kubectl_ -n "${PODNET_NS}" get pod host-echo -o jsonpath='{.spec.hostNetwork}')" != true ]] ||
		die "host-echo uses the host's network"
	ip="$(pod_ip host-echo)"
	in_cidr "${ip}" "${POD_CIDR}" || die "host-echo's address ${ip} is not in the pod range ${POD_CIDR}"
	log "PASS: host-echo on ${NODE_NAME} has the pod address ${ip}, in ${POD_CIDR}"

	cp_ip="$(pod_ip cp-echo)"
	out="$(in_host_pod curl -sS --max-time 10 --retry 5 --retry-all-errors "http://${cp_ip}:8080/hostname")" ||
		die "host-echo cannot reach cp-echo at ${cp_ip}:8080"
	[[ "${out}" == cp-echo ]] || die "cp-echo at ${cp_ip}:8080 answered ${out}, not cp-echo"
	log "PASS: host-echo reached cp-echo at ${cp_ip} on the control plane"

	svc="$(service_ip)"
	out="$(in_host_pod curl -sS --max-time 10 --retry 5 --retry-all-errors "http://${svc}/hostname")" ||
		die "host-echo cannot reach the Service cp-echo at ${svc}"
	[[ "${out}" == cp-echo ]] || die "the Service at ${svc} answered ${out}, not cp-echo"
	log "PASS: host-echo reached the ClusterIP Service cp-echo at ${svc}"

	local name="cp-echo.${PODNET_NS}.svc.cluster.local"
	out="$(in_host_pod nslookup "${name}")" || { printf '%s\n' "${out}" >&2; die "host-echo cannot resolve ${name}"; }
	grep -qF "${svc}" <<<"${out}" || { printf '%s\n' "${out}" >&2; die "${name} did not resolve to ${svc}"; }
	out="$(in_host_pod curl -sS --max-time 10 "http://cp-echo/hostname")" ||
		die "host-echo cannot reach the Service by its short name"
	[[ "${out}" == cp-echo ]] || die "http://cp-echo answered ${out}, not cp-echo"
	log "PASS: host-echo resolved ${name} to ${svc} through the cluster's DNS, and reached it by its short name"
}

# step_isolation checks that a MicroVM reaches none of the pod network
# (HI-035, HI-075, HI-076): not a pod on the control plane, not a pod on its
# own Host, and not a ClusterIP Service, nor the API server's Service. The
# Host itself reaches each of them first, so each is live. A guest's packet
# to any of them is dropped, so curl times out (exit 28). The pods step
# makes the targets.
step_isolation() {
	host_root test -x "${CONSUMER_BIN}" || die "no Consumer on the Host: run smoke.sh claim first"
	local cp_ip host_ip svc api targets t
	cp_ip="$(pod_ip cp-echo)"
	host_ip="$(pod_ip host-echo)"
	svc="$(service_ip)"
	api="$(kubectl_ -n default get service kubernetes -o jsonpath='{.spec.clusterIP}')"
	[[ -n "${cp_ip}" && -n "${host_ip}" && -n "${svc}" ]] || die "no echo pods: run smoke.sh pods first"
	targets="http://${cp_ip}:8080/hostname http://${host_ip}:8080/hostname http://${svc}/hostname https://${api}/healthz"
	for t in ${targets}; do
		host_root curl -sk -o /dev/null --max-time 10 "${t}" || die "the Host itself cannot reach ${t}"
	done
	log "The Host reaches each target: ${targets}"
	log "Claiming a MicroVM and trying each target from it"
	local out
	if ! out="$(host_root "${CONSUMER_BIN}" -kubeconfig "${CONSUMER_KUBECONFIG}" -namespace bo-trial -pool trial -holder trial-holder \
		-command "for t in ${targets}; do curl -sk -o /dev/null --max-time 5 \$t; echo \"guest-curl \$t \$?\"; done; true")"; then
		printf '%s\n' "${out}" >&2
		die "the Consumer failed"
	fi
	printf '%s\n' "${out}" >&2
	local failed=""
	for t in ${targets}; do
		local rc
		rc="$(awk -v t="${t}" '$1 == "guest-curl" && $2 == t { print $3 }' <<<"${out}")"
		case "${rc}" in
		28) log "PASS: the MicroVM's request to ${t} timed out: dropped" ;;
		"") warn "FAIL: no result for ${t}"; failed=1 ;;
		*) warn "FAIL: the MicroVM's request to ${t} ended with curl exit ${rc}, not a timeout"; failed=1 ;;
		esac
	done
	[[ -z "${failed}" ]] || die "a MicroVM reached into the pod network"
}

main() {
	local steps=("$@")
	[[ ${#steps[@]} -gt 0 ]] || steps=("${STEPS[@]}")
	local s
	for s in "${steps[@]}"; do
		case "${s}" in
		pods | isolation) "step_${s}" ;;
		pool | claim | network | restart | delete) trial "${s}" ;;
		*) die "unknown step ${s}: ${STEPS[*]}" ;;
		esac
	done
}

use_host
keep_mac_awake
main "$@"
