#!/usr/bin/env bash
# The kubeadm Host proof's smoke test (README.md): the real hosts trial's
# smoke.sh, run against the Host over ssh. Its Consumer runs on the Host
# with the control plane's admin kubeconfig, which up.sh's join step put
# there, from /var: the Host's /usr is read-only.
#
#   smoke.sh           every step: pool claim network restart delete
#   smoke.sh <step>... only those
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

use_host
keep_mac_awake
STATE_DIR="${STATE_DIR}" NODE_NAME="${NODE_NAME}" HOST_DRIVER=ssh HOST_SSH="${HOST_SSH}" SSH_OPTS="${SSH_OPTS}" \
	CONSUMER_KUBECONFIG=/etc/kubernetes/bo-proof-admin.conf CONSUMER_BIN=/var/lib/bo-proof/bo-trial-smoke \
	"${KH_HERE}/../real-hosts/smoke.sh" "$@"
