#!/usr/bin/env bash
# Makes the kubeadm Host proof's control plane (../README.md), on Ubuntu
# 24.04, as root: containerd, the kubelet, kubeadm and kubectl at
# KUBERNETES_VERSION, `kubeadm init` on the address of the shared network
# (NODE_IP), and Flannel. Idempotent: a second run skips what is there.
#
# Run by up.sh through the VM's shell, with KUBERNETES_VERSION, NODE_IP,
# POD_CIDR, SERVICE_CIDR, FLANNEL_VERSION, LOCAL_PATH_VERSION and GATEWAY_IP (the
# shared network's gateway) in the environment.
set -euo pipefail

: "${KUBERNETES_VERSION:?}" "${NODE_IP:?}" "${POD_CIDR:?}" "${SERVICE_CIDR:?}" "${FLANNEL_VERSION:?}" "${LOCAL_PATH_VERSION:?}" "${GATEWAY_IP:?}"
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

# Flannel, on the shared network's interface on every Node: the one that
# reaches the network's gateway.
curl -fsSL "https://github.com/flannel-io/flannel/releases/download/${FLANNEL_VERSION}/kube-flannel.yml" |
	sed -e "s#10.244.0.0/16#${POD_CIDR}#" \
		-e "s#- --kube-subnet-mgr#- --kube-subnet-mgr\n        - --iface-can-reach=${GATEWAY_IP}#" |
	kubectl apply -f - >/dev/null
# The Host enforces SELinux and labels pods (HI-066). Flannel's two init
# containers are unprivileged and copy its plugin into /opt/cni/bin and its
# configuration into /etc/cni/net.d, which container_t may not write. So
# they run as spc_t, the base policy's type for containers that manage the
# host (hostimage/README.md, "Pods run confined").
kubectl -n kube-flannel patch daemonset kube-flannel-ds --type=json -p '[
  {"op": "add", "path": "/spec/template/spec/initContainers/0/securityContext", "value": {"seLinuxOptions": {"type": "spc_t"}}},
  {"op": "add", "path": "/spec/template/spec/initContainers/1/securityContext", "value": {"seLinuxOptions": {"type": "spc_t"}}}
]' >/dev/null
kubectl -n kube-flannel rollout status daemonset/kube-flannel-ds --timeout=300s

# A default StorageClass for battery's data volume, as k3s has in the real
# hosts trial: local-path, on the control plane's disk.
kubectl apply -f "https://raw.githubusercontent.com/rancher/local-path-provisioner/${LOCAL_PATH_VERSION}/deploy/local-path-storage.yaml" >/dev/null
kubectl annotate storageclass local-path storageclass.kubernetes.io/is-default-class=true --overwrite >/dev/null
kubectl -n local-path-storage rollout status deployment/local-path-provisioner --timeout=300s
kubectl wait --for=condition=Ready node --all --timeout=300s
