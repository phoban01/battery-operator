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

Nothing else in a cluster approves or signs a signer name it does not know,
so the Operator is the only authority over these certificates, and the
cluster's cert-manager and its approval settings are untouched.

CT-006 makes the checks hold even for a request someone else approved:
anyone granted `approve` on one of these signer names could otherwise have
the Operator sign whatever they approve. CT-007 keeps a requester from
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
- **CT-013** If a request for any of the three signer names fails any check,
  then the Operator SHALL deny it with a reason that names the check.
- **CT-014** The Operator SHALL approve a
  `battery.liquidmetal-x.dev/exec-agent-serving` request only when the
  requester is the Exec Agent's ServiceAccount, the requester's
  `authentication.kubernetes.io/node-name` names an existing Node, and the
  request's subject alternative names are exactly one of that Node's
  internal addresses and
  `spiffe://<trust domain>/flintlock/client/exec-agent/<node name>`.

A pod's ServiceAccount token carries the Node the pod was scheduled on, and
the API server records it on every request the pod makes. That is what lets
CT-010, CT-011 and CT-014 tie a certificate to the requester's own Host: an
Exec Agent can obtain a certificate only for the Host it runs on, and a
compromised Host cannot obtain one for another. It requires Kubernetes 1.32
or later. A dual-stack Node has two internal addresses; a request names one
of them.

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
