# 0003. Host certificates from cert-manager, requested by the Exec Agent

- **Status:** Proposed
- **Date:** 2026-09-23
- **Follows:** [0002](0002-battery-reaches-flintlockd-over-mtls.md)

## Context

ADR 0002 has every Host's `flintlockd` serve on the Host's internal address
with mutual TLS. That needs three kinds of certificate:

| Certificate | Held by | Names | Signed by |
|---|---|---|---|
| `flintlockd` serving | each Host's `flintlockd` | the Host's internal address | the serving CA, which battery and the Exec Agents trust |
| battery's client | battery, in the Operator's pod | the Operator | the `flintlockd` client CA |
| Exec Agent client | each Host's Exec Agent | that Exec Agent | the `flintlockd` client CA |

Something has to issue them, deliver the Host's to `flintlockd`, which runs
outside Kubernetes as a systemd service, and renew them. The Exec Agent
already runs on every Host as a DaemonSet pod, and cert-manager is the usual
issuer in a cluster.

Three ways were considered for the per-Host certificates:

1. **cert-manager's csi-driver** in the Exec Agent's pod. The key never
   leaves the Host, but the driver templates only the pod's name, namespace,
   uid and ServiceAccount, and `ip-sans` takes no variables at all. It
   cannot issue a serving certificate for the Host's address.
2. **A `Certificate` per Node**, created by the Operator, with the Node's
   address. cert-manager writes each into a Secret, but a DaemonSet mounts
   the same Secret on every Node, so each Exec Agent would read its Secret
   through the API. RBAC cannot limit that to the Agent's own Node, so every
   Exec Agent could read every Host's serving key.
3. **The Exec Agent asks for its own certificates.** It generates each key
   on the Host and sends only a signing request. An approver checks that the
   request names the requester's own Host before cert-manager signs it.

Option 3 depends on knowing which Node a request comes from. Since
Kubernetes 1.32, a ServiceAccount token bound to a pod carries the pod's
Node, and the API server reports it as `authentication.kubernetes.io/node-name`
in the requester's user info. cert-manager's `CertificateRequest` records the
requester's username, groups and that extra info in its `spec`.

## Decision

1. **cert-manager issues every certificate**, from two CA issuers: one for
   `flintlockd` serving certificates and one, the `flintlockd` client CA,
   for client certificates. The Manifests create both.
2. **battery's client certificate is an ordinary cert-manager
   `Certificate`** in the Operator's namespace. Its Secret is mounted into
   the battery sidecar (DP-005).
3. **The Exec Agent requests its Host's certificates itself (option 3).**
   On every Host it:
   - generates the `flintlockd` serving key and its own client key on the
     Host, and never sends a key to the API;
   - creates a `CertificateRequest` for each: the serving one naming the
     Host's internal address and nothing else, the client one naming the
     Exec Agent;
   - renews each before it expires.
4. **The Operator approves those requests.** It approves a serving request
   only when the requester is an Exec Agent's ServiceAccount, and the
   request names exactly the internal address of the Node in the
   requester's `authentication.kubernetes.io/node-name`. It approves a
   client request only for an Exec Agent's or battery's identity. Anything
   else is denied.
5. **The Exec Agent delivers `flintlockd`'s files on the Host.** It writes
   the serving certificate, its key and the client CA bundle to a fixed
   path on the Host. The Host Image starts `flintlockd` only once they are
   present, and restarts it when they change, with a systemd path unit. The
   Exec Agent needs no systemd access.
6. **The Exec Agent keeps its own client key in memory** and uses it only
   for its connection to its Host's `flintlockd`.

## Consequences

1. **A Host's serving key never leaves the Host**, and an Exec Agent can get
   a serving certificate only for its own Host's address. A compromised
   Host cannot obtain a certificate to impersonate another Host's
   `flintlockd` to battery.
2. **cert-manager must not approve these requests by itself.** Its built-in
   approver approves every request for its own issuer types, and it can
   only be turned off cluster-wide. Either the cluster runs cert-manager
   with that approver off and approver-policy for everything else, or the
   Exec Agent uses Kubernetes `CertificateSigningRequest`s with a signer
   name only the Operator approves and cert-manager signs. Which one is
   settled when this is built. It is the largest cost of this decision.
3. **Kubernetes 1.32 or later** is required, for the Node in the requester's
   token.
4. **Bootstrap is ordered by the Host checks.** `flintlockd` does not start
   until the Exec Agent has written its certificates. Until then the Exec
   Agent's prerequisite scan finds no `flintlockd` answering and reports the
   Host not ready (EA-030), so the Inventory Controller never gives battery
   a Host without certificates.
5. **Renewal restarts `flintlockd`.** Whether running MicroVMs survive a
   `flintlockd` restart has to be confirmed; if they don't, renewal waits
   until the Host has no Bound claims, like a drain.
6. **The Exec Agent's pod writes to a Host path**, which the Host Image has
   to label so the Agent's container may write it and `flintlockd` may read
   it, as flintlock-runner's HI-065 already does for other shared paths.
7. **The certificates can be short-lived**, since renewal is automatic. That
   also answers ADR 0002's point that Exec Agent client certificates should
   be short-lived.

## Open questions

1. cert-manager `CertificateRequest`s with approver-policy, or Kubernetes
   `CertificateSigningRequest`s (consequence 2).
2. Whether `flintlockd` can reload its certificates without a restart
   (consequence 5).
