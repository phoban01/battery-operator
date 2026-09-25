// -*- mode: Bluespec; -*-
//
// The shared types of the battery-operator models.
//
// These mirror the v1alpha1 resources (docs/requirements/01-resources.md,
// api/v1alpha1/), battery v0.3.3's Leases and MicroVMs, and the Hosts and
// Nodes of the inventory (docs/requirements/04-inventory.md), and the
// CertificateSigningRequests and certificates of 09-certificates.md. They
// keep only the fields a model reasons about, rather than every field of
// the Go types. The claim lifecycle model (claims.qnt) uses the claim,
// Lease and condition types; the pools and inventory model (pools.qnt)
// uses the Pool, battery Pool and Node types; the certificates model
// (certificates.qnt) uses the last section.
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
    | BatteryUnavailable // CL-007, CL-040
    | BatteryAnswered  // Synced true (CL-042)
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
    // Whether the status reflects battery's answer to the last call the
    // Claim Controller made for the claim (CL-040, CL-042).
    synced: Condition,
  }

  type MicroVMClaim = {
    spec: ClaimSpec,
    status: ClaimStatus,
    // metadata.finalizers holds battery.liquidmetal-x.dev/release (CL-001)
    hasFinalizer: bool,
    // metadata.deletionTimestamp is set
    deleting: bool,
    // status.observedRenewTime: the spec.renewTime the Claim Controller
    // last relayed to battery, or recorded when it bound the claim. A
    // claim has a pending renewal while spec.renewTime differs from it
    // (CL-010). It is in the claim's status, written with battery's expiry
    // in one write, so it survives a restart of the controller.
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
    synced: unknownCondition,
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

  // A Pool's place in the API server, as ClaimSlot is a claim's. Once a
  // Pool is gone, a Pool of the same name can be created again.
  type PoolSlot = PoolUnborn | PoolLive(Pool) | PoolGone(Pool)

  pure val emptyPoolStatus: PoolStatus = {
    observedGeneration: 0,
    available: 0,
    leased: 0,
    provisioning: 0,
    quarantined: 0,
    ready: unknownCondition,
    exhausted: unknownCondition,
  }

  // A new Pool; metadata.generation starts at 1.
  pure def newPool(name: PoolName, spec: PoolSpec): Pool = {
    name: name,
    generation: 1,
    spec: spec,
    status: emptyPoolStatus,
    hasFinalizer: false,
    deleting: false,
  }

  // The Pool a slot holds, or holds last. Only meaningful for PoolLive and
  // PoolGone.
  pure def poolIn(slot: PoolSlot): Pool = match slot {
    | PoolLive(o) => o
    | PoolGone(o) => o
    | PoolUnborn => newPool("", { size: 0, heartbeatExpiryThreshold: 0, nodeSelector: Set() })
  }

  pure def isLivePool(slot: PoolSlot): bool = match slot {
    | PoolLive(_) => true
    | _ => false
  }

  pure def isGonePool(slot: PoolSlot): bool = match slot {
    | PoolGone(_) => true
    | _ => false
  }

  // ---------------------------------------------------------------------
  // battery v0.3.3: Leases and MicroVMs

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

  // ---------------------------------------------------------------------
  // Certificates (09-certificates.md, 05-exec-agent.md#certificates)

  // An IP address, as a Node's status lists it or a request names it.
  type Address = str

  // spec.signerName. OtherSigner stands for every signer name that is not
  // one of the Operator's three (CT-001).
  type SignerName = FlintlockdServing | FlintlockdClient | ExecAgentServing | OtherSigner

  // spec.usages, reduced to the four the signers issue with and one that
  // none does.
  type KeyUsage = DigitalSignature | KeyEncipherment | ServerAuth | ClientAuth | CertSign

  // A SPIFFE ID in flintlock's naming scheme (ADR 0003, decision 5):
  // spiffe://<trustDomain>/flintlock/host/<node> for a Host's flintlockd,
  // .../flintlock/client/exec-agent/<node> for an Exec Agent, and
  // .../flintlock/client/battery for battery (whose node is "").
  type SpiffeKind = HostWorkload | ExecAgentWorkload | BatteryWorkload
  type SpiffeId = { trustDomain: str, kind: SpiffeKind, node: HostName }

  // The requester's user info, which the API server records on the
  // request: spec.username, and the authentication.kubernetes.io/node-name
  // extra that a ServiceAccount token bound to a pod carries (Kubernetes
  // 1.32). The requester chooses neither.
  type Requester = { username: str, nodeName: Option[HostName] }

  // Each check of 09-certificates.md#approval, in the order the Operator
  // makes them (internal/controller/csr_review.go). The Operator denies a
  // request, or marks it Failed, with the first check it fails.
  type Check =
    | CheckRequester       // RequesterNotExecAgent: CT-010, CT-011, CT-014
    | CheckNodeName        // RequesterNodeMissing: the same
    | CheckInvalidRequest  // InvalidRequest: not a self-signed PKCS#10 request
    | CheckKeyUsages       // KeyUsagesMismatch: CT-012
    | CheckNodeNotFound    // NodeNotFound: CT-010, CT-014
    | CheckSubjectAltNames // SubjectAltNamesMismatch: CT-010, CT-011, CT-014
    | CheckAddressPinned   // AddressPinned: CT-015

  // Who put the Approved or Denied condition on a request: the Operator,
  // or anyone else granted approve on the signer name.
  type Approver = OperatorApprover | ForeignApprover

  // The Approved and Denied conditions. The API server allows one of them,
  // once, and never withdraws it.
  type Decision =
    | Undecided
    | ApprovedBy(Approver)
    | DeniedBy({ approver: Approver, reason: Check })

  // The CA that signed a certificate (CT-002).
  type CA = ServingCA | ClientCA

  // An issued certificate: its subject, subject alternative names and key
  // usages, and the CA that signed it.
  type Certificate = {
    ca: CA,
    commonName: str,
    ips: Set[Address],
    uris: Set[SpiffeId],
    dnsNames: Set[str],
    usages: Set[KeyUsage],
  }

  type CertificateSigningRequest = {
    signer: SignerName,
    requester: Requester,
    // What the PKCS#10 request asks for.
    commonName: str,
    ips: Set[Address],
    uris: Set[SpiffeId],
    dnsNames: Set[str],
    // The request is well formed and its signature verifies with its own
    // key.
    selfSigned: bool,
    usages: Set[KeyUsage],
    // status.conditions: Approved or Denied
    decision: Decision,
    // status.conditions: Failed, with the check that failed
    failed: Option[Check],
    // status.certificate
    certificate: Option[Certificate],
  }
}
