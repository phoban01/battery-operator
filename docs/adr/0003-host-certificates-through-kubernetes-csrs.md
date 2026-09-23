# 0003. Host certificates through Kubernetes certificate signing requests

- **Status:** Accepted
- **Date:** 2026-09-23
- **Follows:** [0002](0002-battery-reaches-flintlockd-over-mtls.md)

## Context

ADR 0002 has every Host's `flintlockd` serve on the Host's internal address
with mutual TLS. That needs three kinds of certificate:

| Certificate | Held by | Signed by |
|---|---|---|
| `flintlockd` serving | each Host's `flintlockd` | the serving CA, which battery and the Exec Agents trust |
| battery's client | battery, in the Operator's pod | the `flintlockd` client CA |
| Exec Agent client | each Host's Exec Agent | the `flintlockd` client CA |

Something has to issue them, deliver the Host's to `flintlockd`, which runs
outside Kubernetes as a systemd service, and renew them. The Exec Agent
already runs on every Host as a DaemonSet pod.

What rules the options out is the per-Host certificates. A serving
certificate must name its own Host's address, and nothing that runs on one
Host may obtain a certificate for another.

1. **cert-manager's csi-driver** in the Exec Agent's pod. The key never
   leaves the Host, but the driver templates only the pod's name, namespace,
   uid and ServiceAccount, and `ip-sans` takes no variables at all. It
   cannot issue a certificate for the Host's address.
2. **A cert-manager `Certificate` per Node**, created by the Operator. cert-manager
   writes each into a Secret, and a DaemonSet mounts the same Secret on
   every Node, so each Exec Agent would read its Secret through the API.
   RBAC cannot limit that to the Agent's own Node, so every Exec Agent could
   read every Host's serving key.
3. **The Exec Agent requests its own certificates through cert-manager's
   `CertificateRequest`.** Keys stay on the Host, but cert-manager's
   built-in approver approves every request for its own issuers, and can
   only be turned off for the whole cluster. The alternative approver,
   approver-policy, sees the requester's name and groups but not its Node,
   and cannot look up a Node's address, so it cannot make the check either.
4. **The Exec Agent requests its own certificates through Kubernetes'
   `CertificateSigningRequest` API**, under signer names of this project's
   own. The API server records the requester's user info on the request,
   including, since Kubernetes 1.32, the Node of the pod its token is bound
   to (`authentication.kubernetes.io/node-name`). Nothing else in a cluster
   approves or signs a signer name it does not know.

flintlock is also adopting SPIFFE as its identity vocabulary
([flintlock#1242](https://github.com/liquidmetal-dev/flintlock/pull/1242),
proposed): callers become principals from the SPIFFE ID in their
certificate's URI SAN, a Cedar policy decides what each may do, and clients
verify a server's SPIFFE ID rather than its address. SPIRE is optional there,
and this project will not use it: it is more to operate than the fleet
warrants.

## Decision

1. **Per-Host certificates use Kubernetes `CertificateSigningRequest`s
   (option 4)**, with two signer names:
   `battery.liquidmetal-x.dev/flintlockd-serving` and
   `battery.liquidmetal-x.dev/flintlockd-client`.
2. **The Operator is the approver and the signer for both.** It approves a
   request only when it names the requester's own Node, signs approved
   requests with the matching CA, and denies everything else. It publishes
   both CA certificates, without their keys, for the Exec Agents to read.
3. **The Exec Agent requests its Host's certificates itself.** It generates
   the `flintlockd` serving key and its own client key on the Host, sends
   only the requests, and renews before expiry. It writes `flintlockd`'s
   certificate, key and client CA bundle to a fixed path on the Host, and
   keeps its own client key in memory.
4. **cert-manager creates the two CAs and battery's client certificate.**
   Each CA is a cert-manager CA certificate in the Operator's namespace,
   whose Secret only the Operator may read. battery's client certificate is
   an ordinary cert-manager `Certificate` from the client CA, mounted into
   the battery sidecar. None of this needs cert-manager's approval settings
   changed.
5. **Every certificate carries a SPIFFE ID**, in flintlock's naming scheme,
   under a trust domain the Operator is configured with:
   - `spiffe://<trust domain>/flintlock/host/<node name>` for a Host's
     `flintlockd`, alongside the Host's internal address, which battery
     v0.1.0 verifies;
   - `spiffe://<trust domain>/flintlock/client/battery` for battery;
   - `spiffe://<trust domain>/flintlock/client/exec-agent/<node name>` for
     an Exec Agent.
6. **The Host Image restarts `flintlockd` when its files change**, with a
   systemd path unit, until `flintlockd` reloads certificates itself
   ([flintlock#1235](https://github.com/liquidmetal-dev/flintlock/issues/1235)).
   The Exec Agent needs no systemd access.
7. **No SPIRE.** Certificates come from the Operator and cert-manager for as
   long as this design stands.

## Consequences

1. **A Host's keys never leave the Host**, and an Exec Agent can get a
   certificate only for its own Host. A compromised Host cannot obtain one
   to impersonate another Host's `flintlockd` to battery.
2. **The cluster's cert-manager is untouched.** Its built-in approver keeps
   working for everyone else, because these requests never go through
   cert-manager.
3. **The Operator holds two CA keys**, which makes it a more valuable
   target. Only the Operator's identity may read their Secrets, and the
   Operator signs only for its two signer names. CA rotation is not
   designed yet.
4. **Kubernetes 1.32 or later** is required, for the Node in the requester's
   token.
5. **Bootstrap is ordered by the Host checks.** `flintlockd` does not start
   until the Exec Agent has written its certificates. Until then the Exec
   Agent's prerequisite scan finds no `flintlockd` answering and reports the
   Host not ready (EA-030), so the Inventory Controller never gives battery
   a Host without certificates.
6. **Renewal restarts `flintlockd`** until flintlock#1235 lands. Whether
   running MicroVMs survive a `flintlockd` restart has to be confirmed; if
   they don't, renewal waits until the Host has no Bound claims.
7. **The Exec Agent's pod writes to a Host path**, which the Host Image has
   to label so the Agent's container may write it and `flintlockd` may read
   it, as flintlock-runner's HI-065 already does for other shared paths.
8. **The SPIFFE IDs are ready for flintlock's authorization.** Once
   flintlock#1242 lands and battery presents its certificate's identity, a
   Cedar policy can give battery the MicroVM lifecycle and the Exec Agents
   only exec, which removes most of the shared client CA's exposure (ADR
   0002, consequence 2). Nothing here depends on it.
9. **The certificates can be short-lived**, since renewal is automatic.
