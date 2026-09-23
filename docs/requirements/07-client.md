# The Client Library

The Go package a Consumer uses to claim a MicroVM and run commands in it.
flintlock-runner's claim backend is its first user (ADR 0001, "Code moving
from flintlock-runner").

## Claiming {#claiming}

- **CC-001** The Client Library SHALL create a claim with the Pool and
  Holder the Consumer names, and then an empty Secret `<claim name>-exec`
  whose owner reference is the claim.
- **CC-002** The Client Library SHALL request each claim token with
  `TokenRequest` for the Holder, bound to the claim's Secret by name and uid,
  with the Exec Agent's audience.
- **CC-003** The Client Library SHALL return a claim to the Consumer only once
  the claim's phase is `Bound`, together with the MicroVM's uid, the Host's
  node name and the Exec Agent's address from the claim's status.
- **CC-004** While a claim waits with the reason `PoolExhausted`, the Client
  Library SHALL report the Pool as exhausted to the Consumer.
- **CC-005** When the Consumer stops waiting for a claim to bind, the Client
  Library SHALL delete the claim.

The Secret holds no data; it exists only to anchor the claim token, so the
token dies with the claim (see `05-exec-agent.md#authorization`).

## Holding {#holding}

- **CC-010** While the Consumer holds a Bound claim, the Client Library SHALL
  set the claim's `spec.renewTime` at the Pool's heartbeat interval.
- **CC-011** While the Consumer holds a Bound claim, the Client Library SHALL
  request a new claim token before the current one expires.
- **CC-012** If a held claim's phase becomes `Expired`, or the claim is
  deleted by anyone but the Client Library, then the Client Library SHALL
  report the Lease as lost to the Consumer.

## Using and releasing {#using}

- **CC-020** The Client Library SHALL connect to the Exec Agent at the
  address in the claim's status, verifying its serving certificate against
  the certificate authority the Consumer configures, and SHALL send the
  current claim token as the bearer token of every request.
- **CC-021** When the Consumer releases a claim, the Client Library SHALL
  delete the claim.
