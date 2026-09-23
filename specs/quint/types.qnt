// -*- mode: Bluespec; -*-
//
// The shared types of the battery-operator models.
//
// These mirror the v1alpha1 resources (docs/requirements/01-resources.md,
// api/v1alpha1/), battery v0.1.0's Leases and MicroVMs, and the Hosts and
// Nodes of the inventory (docs/requirements/04-inventory.md). They keep
// only the fields a model reasons about, rather than every field of the Go
// types. The claim lifecycle model (claims.qnt) uses the claim, Lease and
// condition types; the Pool, battery Pool, Node and Host types are for the
// pools and inventory model (#47), which adds to them as it needs.
//
// Times are whole ticks of one clock, shared by battery and the Operator.
module types {

  // ---------------------------------------------------------------------
  // Basics

  type Option[a] = Some(a) | None

  // Whole ticks of the shared clock.
  type Time = int

  type ClaimName = str
  type PoolName = str
  type VMId = str
  type HostName = str
  type ServiceAccountName = str

  // battery chooses lease ids in ClaimVM (ADR 0001, consequence 2). The
  // models number them from 1 and never reuse one.
  type LeaseId = int

  pure def isSome(o: Option[a]): bool = match o {
    | Some(_) => true
    | None => false
  }

  pure def isNone(o: Option[a]): bool = not(isSome(o))

  pure def getOr(o: Option[a], dflt: a): a = match o {
    | Some(v) => v
    | None => dflt
  }

  pure def optToSet(o: Option[a]): Set[a] = match o {
    | Some(v) => Set(v)
    | None => Set()
  }

  // ---------------------------------------------------------------------
  // Conditions (metav1.Condition, reduced to what the models check)

  type ConditionStatus = CondTrue | CondFalse | CondUnknown

  // The reasons the requirements name for the conditions the models use.
  type Reason =
    | NoReason
    | ClaimBound       // Bound true
    | PoolExhausted    // CL-003
    | PoolNotFound     // CL-004
    | LeaseExpired     // CL-012
    | NoAgentAddress   // CL-006
    | Rejected         // PO-004
    | NoEligibleHost   // PO-012

  type Condition = { status: ConditionStatus, reason: Reason }

  pure val unknownCondition: Condition = { status: CondUnknown, reason: NoReason }

  // ---------------------------------------------------------------------
  // MicroVMClaim (RS-020..024)

  // RS-024. There is no Released phase: a released claim is deleted.
  type ClaimPhase = Pending | Bound | Expired

  type ClaimSpec = {
    poolRef: PoolName,
    serviceAccountName: ServiceAccountName,
    // spec.renewTime, as a counter: the Holder bumps it to renew (RS-023).
    // The models only need to know that it changed.
    renewTime: int,
  }

  type ClaimStatus = {
    phase: ClaimPhase,
    leaseId: Option[LeaseId],
    microVM: Option[VMId],
    nodeName: Option[HostName],
    boundTime: Option[Time],
    leaseExpiresAt: Option[Time],
    bound: Condition,
  }

  type MicroVMClaim = {
    spec: ClaimSpec,
    status: ClaimStatus,
    // metadata.finalizers holds battery.liquidmetal-x.dev/release (CL-001)
    hasFinalizer: bool,
    // metadata.deletionTimestamp is set
    deleting: bool,
    // The spec.renewTime the Claim Controller last relayed to battery.
    // CL-010 needs the controller to know that spec.renewTime changed; the
    // v1alpha1 status has no field for it, and where it lives is #10's
    // choice. The models keep it with the claim, so it survives a restart
    // of the controller.
    observedRenewTime: int,
  }

  // A claim's place in the API server. A claim is created once; once its
  // last finalizer is gone it is Gone, and the models keep its last state
  // so that invariants can look at what it recorded.
  type ClaimSlot = Unborn | Live(MicroVMClaim) | Gone(MicroVMClaim)

  pure val emptyClaimStatus: ClaimStatus = {
    phase: Pending,
    leaseId: None,
    microVM: None,
    nodeName: None,
    boundTime: None,
    leaseExpiresAt: None,
    bound: unknownCondition,
  }

  pure def newClaim(pool: PoolName, sa: ServiceAccountName): MicroVMClaim = {
    spec: { poolRef: pool, serviceAccountName: sa, renewTime: 0 },
    status: emptyClaimStatus,
    hasFinalizer: false,
    deleting: false,
    observedRenewTime: 0,
  }

  // The claim a slot holds, or holds last. Only meaningful for Live and
  // Gone.
  pure def claimIn(slot: ClaimSlot): MicroVMClaim = match slot {
    | Live(o) => o
    | Gone(o) => o
    | Unborn => newClaim("", "")
  }

  pure def isLive(slot: ClaimSlot): bool = match slot {
    | Live(_) => true
    | _ => false
  }

  pure def isGone(slot: ClaimSlot): bool = match slot {
    | Gone(_) => true
    | _ => false
  }

  pure def isUnborn(slot: ClaimSlot): bool = match slot {
    | Unborn => true
    | _ => false
  }

  // ---------------------------------------------------------------------
  // Pool (RS-010..013)

  type Label = (str, str)

  type PoolSpec = {
    size: int,
    heartbeatExpiryThreshold: Time,
    // spec.placement.nodeSelector as matchLabels (RS-011)
    nodeSelector: Set[Label],
  }

  type PoolStatus = {
    observedGeneration: int,
    available: int,
    leased: int,
    provisioning: int,
    quarantined: int,
    ready: Condition,
    exhausted: Condition,
  }

  type Pool = {
    name: PoolName,
    generation: int,
    spec: PoolSpec,
    status: PoolStatus,
    hasFinalizer: bool,
    deleting: bool,
  }

  // ---------------------------------------------------------------------
  // battery v0.1.0: Leases and MicroVMs

  type VMPhase = Provisioning | Available | Leased | Quarantined | Deleted

  type MicroVM = {
    uid: VMId,
    pool: PoolName,
    host: HostName,
    phase: VMPhase,
  }

  // A Lease as battery's database holds it.
  type Lease = {
    id: LeaseId,
    pool: PoolName,
    vm: VMId,
    host: HostName,
    claimedAt: Time,
    expiresAt: Time,
  }

  // battery's PoolSpec, as the Pool Controller declares it (PO-001, PO-010).
  type BatteryPool = {
    name: PoolName,
    size: int,
    heartbeatExpiryThreshold: Time,
    flintlockHosts: Set[HostName],
  }

  // ---------------------------------------------------------------------
  // Nodes and Hosts (04-inventory.md, the Node report of 05-exec-agent.md)

  type NodeReport = {
    ready: bool,
    agentAddress: Option[str],
    flintlockdAddress: Option[str],
  }

  type Node = {
    name: HostName,
    labels: Set[Label],
    schedulable: bool,
    report: NodeReport,
  }

  // A Host as battery's configuration lists it (IN-003).
  type Host = { name: HostName, address: str }
}
