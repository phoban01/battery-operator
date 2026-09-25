# The resources

The `Pool` and `MicroVMClaim` resources, as proposed on
[battery#46](https://github.com/liquidmetal-dev/battery/issues/46) and
decided in [ADR 0001](../adr/0001-standalone-operator-over-battery-grpc.md).
This document fixes their group, their fields and the validation the API
server applies through the CRDs. What the controllers do with them is in
`02-claims.md` and `03-pools.md`.

## API group {#api-group}

- **RS-001** The CRDs SHALL define `Pool` and `MicroVMClaim` as namespaced
  resources in the API group `battery.liquidmetal-x.dev` at version
  `v1alpha1`.
- **RS-002** The CRDs SHALL define no `MicroVM` resource.

The group is provisional (ADR 0001, decision 6). It becomes
`battery.liquidmetal.dev` if liquidmetal-dev adopts the project, which is a
breaking rename, taken while the version is still `v1alpha1`.

MicroVMs stay internal to battery, as they are today. The counts on a Pool's
status and the claims give a consumer the visibility it needs, and a
`MicroVM` resource can be added later without changing the claim contract.

## Pool {#pool}

- **RS-010** The `Pool` resource SHALL carry in its `spec` one field for
  each of the `PoolSpec` fields `microvm_template`, `size`,
  `replenishment_strategy`, `create_commands`, `pre_lease_commands`,
  `hook_failure_policy`, `heartbeat_interval` and
  `heartbeat_expiry_threshold` of battery v0.3.3.
- **RS-011** The `Pool` resource SHALL select the Hosts its MicroVMs may run
  on with a Node label selector in `spec.placement.nodeSelector`, in place of
  battery's `flintlock_hosts`.
- **RS-012** The CRDs SHALL reject a `Pool` whose `spec.size` is negative or
  whose enumerated fields hold a value that battery v0.3.3 does not define.
- **RS-013** The `Pool` resource SHALL have a status subresource that carries
  `observedGeneration`, the counts of available, leased, provisioning and
  quarantined MicroVMs, and the conditions `Ready` and `Exhausted`.
- **RS-014** The CRDs SHALL reject a `Pool` whose `spec.template.vcpu` is less
  than 1 or greater than 64.
- **RS-015** The CRDs SHALL reject a `Pool` whose `spec.template.memoryInMb` is
  less than 1024 or greater than 32768.
- **RS-016** The CRDs SHALL reject a `Pool` whose `spec.template.interfaces`
  does not hold at least one network interface.

A Pool's name and namespace in battery are the resource's own, so `PoolSpec`'s
`name` and `namespace` have no field in `spec`. The proposal on battery#46
also had a placement strategy; battery v0.3.3 has no such field and places
by the least number of MicroVMs on a Host, so it is left out.

RS-012 moves to admission the refusals battery would otherwise give only when
the Pool Controller calls it, so a bad Pool fails when it is applied rather
than later in a condition.

RS-014 to RS-016 do the same for what `flintlockd` refuses. flintlock
v0.15.2 validates a `MicroVMSpec` (`core/models/microvm.go`) with `vcpu`
from 1 to 64, `memory_in_mb` from 1024 to 32768, and at least one network
interface. A template outside those limits was accepted, declared to
battery, and then every MicroVM of its Pool failed in `flintlockd` (#141).
The limits are flintlock v0.15.2's: a later flintlock that relaxes them
changes these requirements and the markers on `MicroVMTemplate` together.

## MicroVMClaim {#microvmclaim}

- **RS-020** The `MicroVMClaim` resource SHALL carry `spec.poolRef.name`,
  which names a `Pool` in the claim's own namespace.
- **RS-021** The `MicroVMClaim` resource SHALL carry
  `spec.serviceAccountName`, which names the ServiceAccount in the claim's
  namespace that may use the claimed MicroVM.
- **RS-022** The CRDs SHALL reject an update that changes a claim's
  `spec.serviceAccountName` or `spec.poolRef`.
- **RS-023** The `MicroVMClaim` resource SHALL carry `spec.renewTime`, which
  the Holder sets to renew the claim's Lease.
- **RS-024** The `MicroVMClaim` resource SHALL have a status subresource that
  carries the phase, one of `Pending`, `Bound` and `Expired`,
  the lease id battery chose, the MicroVM's uid, the Host's node name, the
  Exec Agent's address, the time the claim was bound, the time its Lease
  expires, and the condition `Bound`.

RS-022 is what makes naming the Holder safe without an admission webhook.
Naming someone else's ServiceAccount grants nothing, because the MicroVM is
then usable only by that account. Changing the account on an existing claim
would hand another consumer's MicroVM to a new account, so the schema
forbids it with a CEL rule (`self == oldSelf`), which the API server
enforces itself.

Renewal lives in `spec`, as a `coordination.k8s.io` Lease holder renews: the
Holder writes `spec.renewTime` and the Operator alone writes `status`, so the
consumer needs no permission on the status subresource and the two writers
never collide.

The proposal on battery#46 made the claim's name its lease id. battery v0.3.3
chooses the lease id itself in `ClaimVM`, so the id is recorded in the
status instead (ADR 0001, consequence 2).

There is no `Released` phase. A released claim is deleted as soon as battery
has released its Lease (CL-020), so no consumer would ever see it (#46).
