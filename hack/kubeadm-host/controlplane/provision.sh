#!/usr/bin/env bash
# Makes the kubeadm Host proof's control plane (../README.md), on Ubuntu
# 24.04, as root: containerd, the kubelet, kubeadm and kubectl at
# KUBERNETES_VERSION, `kubeadm init` on the address of the shared network
# (NODE_IP), and Calico. Idempotent: a second run skips what is there.
#
# Run by up.sh through the VM's shell, with KUBERNETES_VERSION, NODE_IP,
# POD_CIDR, SERVICE_CIDR, CALICO_VERSION, CALICO_MANIFEST_SHA256 and
# LOCAL_PATH_VERSION in the environment.
set -euo pipefail

: "${KUBERNETES_VERSION:?}" "${NODE_IP:?}" "${POD_CIDR:?}" "${SERVICE_CIDR:?}" "${CALICO_VERSION:?}" "${CALICO_MANIFEST_SHA256:?}" "${LOCAL_PATH_VERSION:?}"
minor="$(echo "${KUBERNETES_VERSION}" | cut -d. -f1,2)"
export DEBIAN_FRONTEND=noninteractive

# Kernel settings kubeadm's preflight wants.
cat >/etc/modules-load.d/k8s.conf <<EOF
overlay
br_netfilter
EOF
modprobe overlay
modprobe br_netfilter
cat >/etc/sysctl.d/99-k8s.conf <<EOF
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward = 1
EOF
sysctl --system >/dev/null
swapoff -a

if ! command -v kubeadm >/dev/null; then
	apt-get update -qq
	apt-get install -y -qq apt-transport-https ca-certificates curl gpg containerd >/dev/null
	mkdir -p /etc/apt/keyrings
	curl -fsSL "https://pkgs.k8s.io/core:/stable:/${minor}/deb/Release.key" |
		gpg --dearmor --yes -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
	echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/${minor}/deb/ /" \
		>/etc/apt/sources.list.d/kubernetes.list
	apt-get update -qq
	v="${KUBERNETES_VERSION#v}"
	apt-get install -y -qq "kubelet=${v}-*" "kubeadm=${v}-*" "kubectl=${v}-*" >/dev/null
	apt-mark hold kubelet kubeadm kubectl >/dev/null
fi

# containerd with the systemd cgroup driver, which is kubeadm's default for
# the kubelet.
if ! grep -q 'SystemdCgroup = true' /etc/containerd/config.toml 2>/dev/null; then
	mkdir -p /etc/containerd
	containerd config default | sed 's/SystemdCgroup = false/SystemdCgroup = true/' >/etc/containerd/config.toml
	systemctl restart containerd
fi
systemctl enable --now containerd >/dev/null 2>&1

# The kubelet registers the Node with the address on the shared network,
# not Lima's user-mode network, whose address every Lima VM shares.
echo "KUBELET_EXTRA_ARGS=--node-ip=${NODE_IP}" >/etc/default/kubelet

if [[ ! -f /etc/kubernetes/admin.conf ]]; then
	kubeadm init --kubernetes-version="${KUBERNETES_VERSION}" \
		--apiserver-advertise-address="${NODE_IP}" \
		--pod-network-cidr="${POD_CIDR}" --service-cidr="${SERVICE_CIDR}" \
		--skip-token-print
fi
export KUBECONFIG=/etc/kubernetes/admin.conf

# One Node runs the Operator and cert-manager: the control plane's own.
kubectl taint node --all node-role.kubernetes.io/control-plane:NoSchedule- >/dev/null 2>&1 || true

# Calico, from the manifest the Hosts page (site/hosts.md) applies, checked
# against CALICO_MANIFEST_SHA256, with three settings for this network:
# - CALICO_IPV4POOL_CIDR: Calico's default pool, 192.168.0.0/16, overlaps
#   the Mac's networks. The pool is POD_CIDR, which kubeadm init was given.
# - IP: empty, not "autodetect". IP_AUTODETECTION_METHOD stays unset, so a
#   Node runs Calico's default, first-found, unless it has an address
#   already. The control plane must not: first-found takes eth0 there,
#   Lima's user-mode network, whose address every Lima VM shares. So the
#   control plane's Node gets its address on the shared network in the
#   annotation projectcalico.org/IPv4Address before Calico starts, and
#   calico-node keeps it. The Host has no annotation, so its calico-node
#   autodetects with first-found and must skip the guest bridge (HI-083).
# - CALICO_IPV4POOL_IPIP and CALICO_IPV4POOL_VXLAN: the pool encapsulates
#   in VXLAN, not IP-in-IP. The Mac routes between the VMs and passes TCP
#   and UDP, but not IP protocol 4: each Node sent IP-in-IP packets and
#   neither received one. VXLAN is UDP.
# calico-node and its init containers are privileged, so they need no
# seLinuxOptions on the Host (hostimage/README.md, "Pods run confined").
node_cidr="$(ip -o -4 addr show | awk -v ip="${NODE_IP}" '{ split($4, a, "/"); if (a[1] == ip) print $4 }' | head -n 1)"
[[ -n "${node_cidr}" ]] || {
	echo "no interface has ${NODE_IP}" >&2
	exit 1
}
kubectl annotate --overwrite "$(kubectl get node -l node-role.kubernetes.io/control-plane -o name)" \
	"projectcalico.org/IPv4Address=${node_cidr}" >/dev/null
curl -fsSL -o /tmp/calico.yaml "https://raw.githubusercontent.com/projectcalico/calico/${CALICO_VERSION}/manifests/calico.yaml"
echo "${CALICO_MANIFEST_SHA256}  /tmp/calico.yaml" | sha256sum -c --quiet - || {
	echo "Calico ${CALICO_VERSION}'s manifest does not match CALICO_MANIFEST_SHA256" >&2
	exit 1
}
sed -i -e 's|^            # - name: CALICO_IPV4POOL_CIDR$|            - name: CALICO_IPV4POOL_CIDR|' \
	-e "s|^            #   value: \"192.168.0.0/16\"\$|              value: \"${POD_CIDR}\"|" \
	-e '/^            - name: IP$/{n;s|value: "autodetect"|value: ""|}' \
	-e '/^            - name: CALICO_IPV4POOL_IPIP$/{n;s|value: "Always"|value: "Never"|}' \
	-e '/^            - name: CALICO_IPV4POOL_VXLAN$/{n;s|value: "Never"|value: "Always"|}' \
	/tmp/calico.yaml
if ! grep -q "value: \"${POD_CIDR}\"" /tmp/calico.yaml || ! grep -A1 'name: IP$' /tmp/calico.yaml | grep -q 'value: ""' ||
	grep -q 'IP_AUTODETECTION_METHOD' /tmp/calico.yaml ||
	! grep -A1 'name: CALICO_IPV4POOL_VXLAN$' /tmp/calico.yaml | grep -q 'value: "Always"'; then
	echo "Calico ${CALICO_VERSION}'s manifest no longer has the lines this script changes" >&2
	exit 1
fi
kubectl apply -f /tmp/calico.yaml >/dev/null
rm -f /tmp/calico.yaml
kubectl -n kube-system rollout status daemonset/calico-node --timeout=300s
kubectl -n kube-system rollout status deployment/calico-kube-controllers --timeout=300s

# A default StorageClass for battery's data volume, as k3s has in the real
# hosts trial: local-path, on the control plane's disk.
kubectl apply -f "https://raw.githubusercontent.com/rancher/local-path-provisioner/${LOCAL_PATH_VERSION}/deploy/local-path-storage.yaml" >/dev/null
kubectl annotate storageclass local-path storageclass.kubernetes.io/is-default-class=true --overwrite >/dev/null
kubectl -n local-path-storage rollout status deployment/local-path-provisioner --timeout=300s
kubectl wait --for=condition=Ready node --all --timeout=300s
