# Deployment

How the Operator runs, how it reaches battery, and what the Manifests deploy
(ADR 0001, decision 7 and consequences 4 and 5).

## battery as a sidecar {#battery-sidecar}

- **DP-001** The Manifests SHALL run battery as a container in the
  Operator's pod, listening on a loopback address only.
- **DP-002** The Manifests SHALL run the Operator's pod as a single replica
  that is replaced with the `Recreate` strategy.
- **DP-003** The Manifests SHALL keep battery's database on a persistent
  volume.
- **DP-004** The Manifests SHALL supply battery's configuration, its Hosts
  included, from an object the Operator may write.
- **DP-005** The Manifests SHALL give battery, from a Secret, a client
  certificate and key for `flintlockd` and the certificate authority that
  verifies the Hosts' `flintlockd` serving certificates.
- **DP-006** The Operator SHALL restart battery through a single mechanism
  that the Inventory Controller invokes, and SHALL wait until battery answers
  again before any controller calls it.
- **DP-007** When the client certificate in the Secret of DP-005 changes,
  the Operator SHALL restart battery through the mechanism of DP-006, and
  SHALL signal battery only once the Operator's own mount of that Secret
  holds the new certificate.
- **DP-008** The Operator SHALL restart battery for a changed client
  certificate at the close of a restart window of IN-012, in the same
  single restart as every change to battery's Hosts that has settled by
  then.

battery's API must be private to the Operator, because a client that can
reach it directly can claim and use a MicroVM without a claim, bypassing the
Holder and the Exec Agent's checks. battery v0.3.3 listens on TCP only;
loopback in the Operator's pod, which only that pod's containers can reach,
keeps it private with battery unchanged (DP-001).

battery keeps its state in SQLite and is a single writer, so it runs as one
replica, and losing its database would orphan MicroVMs on the Hosts
(DP-002, DP-003). The Operator runs one replica with it, which couples the
Operator's lifecycle to battery's.

The configuration the Manifests supply (DP-004) sets no `sweep_interval`,
so battery uses its default, 10 seconds in battery v0.3.3
([10-battery.md](10-battery.md#expiry)). That interval is part of the
orphan bound (02-claims.md, Binding; ADR 0001, consequence 2): an orphan is
gone within the Pool's `heartbeat_expiry_threshold` plus one
`sweep_interval` of being claimed while battery runs (BA-020), and within
one `sweep_interval` of battery starting again if its expiry passed while
battery was down (BA-022).

DP-006 leaves the mechanism open: the Operator can signal battery through a
shared process namespace, or battery can be restarted by the kubelet when
its configuration changes. Whichever is chosen is written down where it is
built.

battery v0.3.3 reads its client certificate once, when it starts, and
presents it to every Host until it exits (BA-061). cert-manager renews the
certificate before it expires and the kubelet updates the mounted files,
but battery goes on presenting the old certificate, and once that expires
every call to a Host fails its TLS handshake. DP-007 closes the gap with
the one restart of DP-006: the Inventory Controller, which already restarts
battery, watches that Secret too, by name. The kubelet updates a Secret
volume some time after the Secret changes, so the Operator's container
mounts the volume battery reads its certificate from, and battery is
signalled only once that volume holds the new certificate. The Operator
may read the Secret anyway, so the mount gives it nothing new.

A renewal comes weeks before the old certificate expires, so it can wait
for a restart window. DP-008 puts it in one, so that a renewal and a change
to the Hosts close together restart battery once, and restarts stay at
least one restart window apart. battery reloading its certificate would
remove the restart; it is one of the upstream asks.

## The battery connection {#battery-connection}

- **DP-010** The Operator SHALL call battery over gRPC on the loopback
  address of DP-001, with a deadline on every unary call.
- **DP-011** The Operator SHALL map battery's gRPC status codes to errors
  that distinguish a missing object, an exhausted Pool and an unavailable
  battery.
- **DP-012** While battery is unavailable, the Operator SHALL report itself
  not ready on its readiness endpoint.
- **DP-013** The Operator SHALL give `ClaimVM` a deadline of its own,
  configurable apart from the deadline of the other unary calls of DP-010.

`ClaimVM` runs the Pool's pre-lease hooks in the MicroVM before battery
commits the Lease, so it can take much longer than any other unary call. If
its deadline passes during the hooks, battery v0.3.3 treats that as a hook
failure: it applies the Pool's hook failure policy, which deletes or
quarantines the MicroVM, and creates no Lease (`internal/api/lease.go`).
The Claim Controller then retries (CL-007), and a deadline that is always
too short costs the Pool a warm MicroVM on every attempt without ever
binding the claim. DP-013 lets the `ClaimVM` deadline be set above the time
the Pools' pre-lease hooks can take, while the other calls keep a short
one.

## Access {#access}

- **DP-020** The Manifests SHALL grant write access to the status
  subresources of `Pool` and `MicroVMClaim` to the Operator's identity and
  to no other identity they create.
- **DP-021** The Manifests SHALL grant the Operator's identity no access to
  Secrets outside its own namespace.

DP-021 follows from consumers creating their claims' Secrets themselves
(ADR 0001, decision 8).
