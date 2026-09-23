# 0002. battery reaches flintlockd over mutual TLS

- **Status:** Accepted
- **Date:** 2026-09-23
- **Follows:** [0001](0001-standalone-operator-over-battery-grpc.md)

## Context

battery creates and deletes MicroVMs by calling `flintlockd` on each Host,
so battery, in the Operator's pod, has to reach every Host's `flintlockd`
over the network. ADR 0001 did not say how.

flintlock-runner's Host Image, the one Host Image that exists, does the
opposite on purpose. `flintlockd` listens only on loopback (its HI-042), is
exposed on no address reachable from outside the Host (HI-044), and admits
only the Exec Agent's user id (HI-063).

Two ways were considered:

1. **`flintlockd` serves on the Host's internal address with mutual TLS.**
   battery connects to it directly, presenting a client certificate.
2. **The Exec Agent also relays battery's MicroVM lifecycle calls** to a
   `flintlockd` that stays on loopback, for the Operator's identity only.

What the two support, as of battery v0.1.0 and flintlock's current flags:

- battery's per-Host configuration already has TLS towards `flintlockd`,
  with a CA to verify the server and an optional client certificate and key
  (`HostConfig.TLS`).
- `flintlockd` has one listening endpoint (`--grpc-endpoint`) and one TLS
  configuration, with client certificate validation against one client CA
  (`--tls-client-validate`, `--tls-client-ca`). It admits any certificate
  that CA signed, and does not distinguish one client from another.

## Decision

**Option 1.** `flintlockd` serves its whole API on the Host's internal
address with mutual TLS, and battery reaches it directly with a client
certificate. battery is not changed, and option 2's second relay and its
authentication are not built.

## Consequences

1. **The Exec Agent uses mutual TLS too.** `flintlockd` has one endpoint, so
   once it is on the internal address with client validation, the Exec
   Agent's own connection presents a client certificate from the same client
   CA. The Exec Agent still connects only to its own Host's `flintlockd`.
2. **The client CA is a key to every Host.** `flintlockd` admits any
   certificate the client CA signed, with the whole API, on every Host. The
   CA therefore signs only for battery and for Exec Agents, and is used for
   nothing else. A compromised Host's Exec Agent certificate could reach the
   other Hosts' `flintlockd`. Two things narrow that:
   - the Hosts admit connections to `flintlockd`'s port only from the
     Operator's pod network and from the local Exec Agent, by firewall;
   - Exec Agent certificates are short-lived and rotated.

   A `flintlockd` that authorized clients by certificate identity would
   remove this; it is worth raising with flintlock.
3. **The Host prerequisites change.** A Host's `flintlockd` serves on its
   internal address with a serving certificate that names that address, and
   validates client certificates against the client CA. flintlock-runner's
   Host Image has to change to meet this: HI-042, HI-044 and HI-063 are
   replaced by it.
4. **battery never talks to `flintlockd` without TLS.** The Inventory
   Controller writes every Host into battery's configuration with TLS on,
   the server verified, and the client certificate presented.
5. **The Node report carries `flintlockd`'s address** as well as the Exec
   Agent's, so the Inventory Controller can give battery the right address
   for each Host.
