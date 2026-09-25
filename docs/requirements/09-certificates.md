# Certificates

The Operator approves and signs the per-Host certificates that `flintlockd`
and the Exec Agents use, through Kubernetes' `CertificateSigningRequest` API
under three signer names of this project's own
([ADR 0003](../adr/0003-host-certificates-through-kubernetes-csrs.md),
[ADR 0004](../adr/0004-exec-agent-serving-certificate.md)).
What the Exec Agent requests is in `05-exec-agent.md#certificates`.

## Signing {#signing}

- **CT-001** The Operator SHALL approve and sign `CertificateSigningRequest`s
  for the signer names `battery.liquidmetal-x.dev/flintlockd-serving`,
  `battery.liquidmetal-x.dev/flintlockd-client` and
  `battery.liquidmetal-x.dev/exec-agent-serving`, and for no other signer
  name.
- **CT-002** The Operator SHALL sign an approved
  `battery.liquidmetal-x.dev/flintlockd-serving` or
  `battery.liquidmetal-x.dev/exec-agent-serving` request with the serving
  CA, and an approved `battery.liquidmetal-x.dev/flintlockd-client` request
  with the `flintlockd` client CA, each read from its Secret in the
  Operator's namespace.
- **CT-003** The Operator SHALL sign a request only while it carries the
  condition `Approved` and not the condition `Denied`.
- **CT-004** The Operator SHALL issue each certificate for the shorter of the
  request's `expirationSeconds` and the configured maximum duration, and
  never beyond the signing CA's own expiry.
- **CT-005** The Operator SHALL publish the certificates of the serving CA
  and the `flintlockd` client CA, without their keys, in a ConfigMap in its
  namespace.
- **CT-006** The Operator SHALL sign a request only when it passes every
  check of [Approval](#approval) for its signer name, whoever approved it.
- **CT-007** The Operator SHALL set the subject of every certificate it
  issues to a common name of the requester's Node name, ignoring the
  subject the request asks for.
- **CT-008** If a request for any of the three signer names carries the
  condition `Approved` and fails any check of [Approval](#approval), then
  the Operator SHALL mark it `Failed`, with a reason that names the check,
  and never sign it.

Nothing else in a cluster approves or signs a signer name it does not know,
so the Operator is the only authority over these certificates, and the
cluster's cert-manager and its approval settings are untouched.

CT-006 makes the checks hold even for a request someone else approved:
anyone granted `approve` on one of these signer names could otherwise have
the Operator sign whatever they approve. The API server lets nobody
withdraw `Approved`, or add `Denied` beside it, so CT-008 marks such a
request `Failed` where CT-013 denies one that nobody has approved. CT-007 keeps a requester from
choosing a name that a verifier might one day trust, since nothing checks
the requested subject.

## Approval {#approval}

- **CT-010** The Operator SHALL approve a
  `battery.liquidmetal-x.dev/flintlockd-serving` request only when the
  requester is the Exec Agent's ServiceAccount, the requester's
  `authentication.kubernetes.io/node-name` names an existing Node, and the
  request's subject alternative names are exactly one of that Node's
  internal addresses and `spiffe://<trust domain>/flintlock/host/<node name>`.
- **CT-011** The Operator SHALL approve a
  `battery.liquidmetal-x.dev/flintlockd-client` request only when the
  requester is the Exec Agent's ServiceAccount and the request's only
  subject alternative name is
  `spiffe://<trust domain>/flintlock/client/exec-agent/<node name>` for the
  Node in the requester's `authentication.kubernetes.io/node-name`.
- **CT-012** The Operator SHALL approve a request only when its key usages
  are digital signature and key encipherment, with server auth for
  `flintlockd-serving` and `exec-agent-serving`, or with client auth for
  `flintlockd-client`.
- **CT-013** If a request for any of the three signer names fails any check
  and does not carry the condition `Approved`, then the Operator SHALL deny
  it with a reason that names the check.
- **CT-014** The Operator SHALL approve a
  `battery.liquidmetal-x.dev/exec-agent-serving` request only when the
  requester is the Exec Agent's ServiceAccount, the requester's
  `authentication.kubernetes.io/node-name` names an existing Node, and the
  request's subject alternative names are exactly one of that Node's
  internal addresses and
  `spiffe://<trust domain>/flintlock/client/exec-agent/<node name>`.
- **CT-015** The Operator SHALL approve a
  `battery.liquidmetal-x.dev/flintlockd-serving` or
  `battery.liquidmetal-x.dev/exec-agent-serving` request only when its IP
  address is the address pinned to the requester's Node or, when no address
  is pinned to that Node, an address pinned to no other Node.
- **CT-016** When the Operator signs a
  `battery.liquidmetal-x.dev/flintlockd-serving` or
  `battery.liquidmetal-x.dev/exec-agent-serving` request for a Node that has
  no pinned address, the Operator SHALL pin the request's IP address to that
  Node, and SHALL store the certificate only once the pin is stored.
- **CT-017** The Operator SHALL keep the pinned addresses in the ConfigMap
  `host-address-pins` in its namespace, one entry for each Node name, and
  SHALL NOT change or remove an entry, even when its Node is deleted.
- **CT-018** The Manifests SHALL grant write access to the ConfigMap
  `host-address-pins` to the Operator's identity and to no other identity
  they create.

A pod's ServiceAccount token carries the Node the pod was scheduled on, and
the API server records it on every request the pod makes. That is what lets
CT-010, CT-011 and CT-014 tie a certificate to the requester's own Host: an
Exec Agent can obtain a certificate only for the Host it runs on. It
requires Kubernetes 1.32 or later. A dual-stack Node has two internal
addresses; a request names one of them.

A Node's internal addresses are not enough on their own to tie an address
to a Host, because the Host's kubelet writes them. NodeRestriction lets a
kubelet change only its own Node, but it may list any address there. A
compromised Host holds its kubelet's credentials as well as its Exec
Agent's token, so it could list another Host's address, have its Exec Agent
request a serving certificate for it, and put its addresses back (#75).
battery verifies `flintlockd` by address (ADR 0003, decision 5), so with a
network position on the Host network that certificate would let it pose as
the other Host's `flintlockd`, and as its Exec Agent to a consumer that
verifies by address.

CT-015 to CT-018 close that by pinning. The first time the Operator signs a
serving certificate for a Node, it records the certificate's address as
that Node's. From then on it signs serving certificates for that Node only
for that address, and for no other Node for that address. Both serving
signer names share the pin, so a Host's `flintlockd` and its Exec Agent
serve on the same address, as EA-061 and EA-068 have them do; a dual-stack
Host's certificates name the address its first one named. The pin is
stored before the certificate is, and its write is conditional on the
version of the ConfigMap the check read. So a crash cannot leave a
certificate signed without its pin, and two Nodes cannot pin one address
at once.

The pins live outside the Node, where neither the kubelet nor the Exec
Agent can write. A kubelet may write any annotation of its own Node, and
may delete its Node and register it again, which would take a pin kept on
the Node with it. For the same reason the Operator never removes a pin
itself, not even a deleted Node's.

Pinning is trust on first use. It trusts the address a Node's first serving
certificate names, so it protects a Host from the moment the Operator first
signs for it, and every Host from any other Host already pinned. A Host
compromised before its first serving certificate is signed can still pin an
address that no Node has pinned yet, such as that of a Host that has yet to
obtain its own first certificate. Clearing a compromised Host's pin reopens
the same window.

An administrator clears a pin when a Host really changes address, by
removing its Node's entry from the ConfigMap:

```sh
kubectl -n battery-operator-system patch configmap host-address-pins \
  --type=json -p '[{"op": "remove", "path": "/data/<node name>"}]'
```

The Exec Agent's next request then pins the new address. Clearing a pin
takes permission to update that ConfigMap, which the Manifests give no one
but the Operator (CT-018), so it falls to the cluster's administrators. A
Node deleted for good keeps its pin, which an administrator removes the
same way before another Node may take its address.

When DHCP moves an address from one Host to another, the Operator refuses
the second Host's requests for it, because the address is pinned to the
first Host's Node. The administrator clears both Nodes' pins: the first
Host's, which holds the address, and the second Host's, which holds the
address that Host had before. Each Host's next request then pins its new
address. The first Host's certificates still name the moved address until
they expire, which short certificate lifetimes bound.

The Exec Agent's serving certificate (CT-014,
[ADR 0004](../adr/0004-exec-agent-serving-certificate.md)) carries the same
SPIFFE ID as its client certificate, because a SPIFFE ID names a workload,
not the role it plays in a connection.

## Identity {#spiffe-identity}

- **CT-020** The Operator SHALL take its SPIFFE trust domain from its
  configuration, and SHALL refuse to start without one.
- **CT-021** The Manifests SHALL create the serving CA and the `flintlockd`
  client CA as cert-manager CA certificates in the Operator's namespace.
- **CT-022** The Manifests SHALL grant read access to the Secrets of the
  serving CA and the `flintlockd` client CA to the Operator's identity only.
- **CT-023** The Manifests SHALL issue battery's client certificate from the
  `flintlockd` client CA with cert-manager, naming
  `spiffe://<trust domain>/flintlock/client/battery` as its only subject
  alternative name.

The SPIFFE IDs follow flintlock's proposed naming scheme
([flintlock#1242](https://github.com/liquidmetal-dev/flintlock/pull/1242)).
Nothing here depends on flintlock reading them, but once it does, a policy
can tell battery apart from the Exec Agents.
