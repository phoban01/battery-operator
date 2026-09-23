# 0004. The Exec Agent's serving certificate through a third signer

- **Status:** Accepted
- **Date:** 2026-09-23
- **Follows:** [0003](0003-host-certificates-through-kubernetes-csrs.md)

## Context

The Exec Agent serves its exec API over TLS on its Host's internal address
(EA-002). Consumers verify that certificate before they send a claim token.
ADR 0003 covers two per-Host certificates, `flintlockd`'s serving
certificate and the Exec Agent's client certificate, but not this one.
Until now the Exec Agent reads it from a file on the Host.

The certificate has the same constraints as `flintlockd`'s: it must name the
Host's own address, and nothing on one Host may obtain one for another.
flintlock-runner issued it with cert-manager's csi-driver, which ADR 0003
ruled out because the csi-driver cannot name the Host's address.

## Decision

1. **A third signer name, `battery.liquidmetal-x.dev/exec-agent-serving`,**
   in the same flow as ADR 0003's two. The Exec Agent generates the key on
   the Host and requests the certificate. The Operator approves it only
   for the requester's own Node's address, and signs it with the serving
   CA.
2. **The certificate names the Exec Agent's existing SPIFFE ID,**
   `spiffe://<trust domain>/flintlock/client/exec-agent/<node name>`, as
   well as the address. A SPIFFE ID names a workload, not the role it plays
   in a connection, so the agent has one identity for both directions.
3. **The serving CA signs it,** so consumers verify the Exec Agent against
   the same published CA as battery verifies `flintlockd` against.

Two hardening rules come with the signer. They apply to all three signer
names:

4. **The Operator re-checks every request before signing,** whoever
   approved it. Anyone granted `approve` on a signer name could otherwise
   have the Operator sign whatever they approved.
5. **The Operator sets the certificate's subject itself,** to the Node's
   name, and ignores the subject the request asks for.

## Consequences

1. The Exec Agent needs no certificate files on the Host, and nothing about
   it is set up by hand.
2. Consumers configure one CA, the serving CA, for every Host.
3. The approval code is shared: `exec-agent-serving` runs the same checks
   as `flintlockd-serving`, apart from the SPIFFE ID it expects.
4. The Exec Agent's serving certificate and `flintlockd`'s come from one
   CA. That is acceptable because both name a specific
   Host's address and identity. A verifier that checks the SPIFFE ID can
   tell them apart.
