# Glossary

This document defines the terms the normative documents in this directory
use. It contains no requirements.

## System names {#system-names}

- **Operator**: the `battery-operator` process. It runs the Claim
  Controller, the Pool Controller and the Inventory Controller, and is
  battery's only client.
- **CRDs**: the CustomResourceDefinitions this project ships for `Pool` and
  `MicroVMClaim`, including their schema validation.
- **Claim Controller**: the controller in the Operator that turns each
  `MicroVMClaim` into battery's `ClaimVM`, `Heartbeat` and `ReleaseVM` calls
  and mirrors battery's answers into the claim's status.
- **Pool Controller**: the controller in the Operator that declares each
  `Pool` to battery with `CreatePool`, `UpdatePool` and `DeletePool`, and
  mirrors battery's view of the Pool into its status.
- **Inventory Controller**: the controller in the Operator that decides
  which Nodes are Hosts and gives battery that list.
- **Exec Agent**: the process on every Host that relays a claim holder's
  exec requests to `MicroVMExec` on the local `flintlockd`, after checking
  that the caller holds the claim, and that checks the Host and reports the
  result on the Host's Node.
- **Manifests**: the Kubernetes manifests in this repository that deploy the
  Operator with battery, and the Exec Agent.
- **Client Library**: the Go package in this repository that a consumer uses
  to claim a MicroVM, obtain the claim's token, wait for the claim to bind,
  renew it, reach the Exec Agent and release the claim.
- **fake battery**: an in-process gRPC server on battery's protos that
  stands in for battery in tests.
- **fake `flintlockd`**: a gRPC server on flintlock's protos that stands in
  for `flintlockd` and its MicroVMs: in process in the unit tests, and as
  each Host's `flintlockd` in the e2e suite.
- **unit tests**: the Go tests run by `make test`, which use fakes and no
  Kubernetes API server.
- **e2e suite**: the end-to-end tests in `test/e2e/`, written with
  [sigs.k8s.io/e2e-framework](https://github.com/kubernetes-sigs/e2e-framework),
  which create a kind cluster, deploy the Manifests into it and test the
  system as a user would.

## Other terms {#terms}

- **battery**: the flintlock warm-pool manager
  ([liquidmetal-dev/battery](https://github.com/liquidmetal-dev/battery)),
  v0.1.0, run unchanged as a sidecar of the Operator. Its gRPC services are
  `PoolAdmin`, `Lease` and `Events`.
- **local `flintlockd`**: the `flintlockd` on the Exec Agent's own Host,
  which it reaches on one of that Host's addresses over mutual TLS
  (ADR 0002).
- **Host**: a Kubernetes Node that runs `flintlockd` and an Exec Agent and
  that the Inventory Controller has given to battery. The Host's name is the
  Node's name.
- **Pool**: the `Pool` resource, and the battery pool it declares. A Pool is
  identified in battery by the resource's namespace and name.
- **Claim**: a `MicroVMClaim` resource: one consumer's hold on one warm
  MicroVM from a Pool.
- **Lease**: battery's record of a claimed MicroVM, identified by the lease
  id battery chooses in `ClaimVM`. A Bound claim has exactly one Lease.
- **Fails in transit**: said of a call to battery that ends without an
  answer saying what battery did: the connection to battery is down, or the
  call's deadline (DP-010) passes first. DP-011 reports both as an
  unavailable battery, and battery's own `UNAVAILABLE` answer the same way;
  battery v0.3.3 gives that answer when `ReleaseVM` could not yet delete the
  MicroVM. battery may or may not have acted on a call that fails in
  transit.
- **Holder**: the ServiceAccount a claim names in `spec.serviceAccountName`,
  the only identity the Exec Agent lets use the claim's MicroVM.
- **Claim token**: a ServiceAccount token for the Holder, requested with
  `TokenRequest` bound to the claim's Secret `<claim name>-exec` and with the
  Exec Agent's audience.
- **Node report**: the annotations the Exec Agent writes on its Host's Node:
  whether the Host is ready, why not, and where the Exec Agent listens.
- **Host prerequisites**: what a Node has to provide before it can be a
  Host: `flintlockd` with its exec API enabled, serving on the Host's
  internal address with mutual TLS against the `flintlockd` client CA; KVM;
  and containerd's thin pool. This project ships no Host Image.
  flintlock-runner has one, which has to change to serve `flintlockd` this
  way (ADR 0002).
- **`flintlockd` client CA**: the certificate authority whose certificates
  `flintlockd` admits. It signs only for battery and for Exec Agents.
- **serving CA**: the certificate authority that signs every Host's `flintlockd`
  serving certificate, which battery and the Exec Agents trust.
- **trust domain**: the SPIFFE trust domain the Operator is configured with.
  Every certificate this project issues names a SPIFFE ID under it, in
  flintlock's naming scheme (`spiffe://<trust domain>/flintlock/...`).
- **Consumer**: a program that claims MicroVMs through the Client Library,
  flintlock-runner being the first.
