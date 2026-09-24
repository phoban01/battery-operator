// -*- mode: Bluespec; -*-
//
// Certificate approval and signing: the Operator as the approver and signer
// of CertificateSigningRequests for its three signer names.
//
// What it models (docs/requirements/09-certificates.md, the Exec Agent's
// requests of 05-exec-agent.md#certificates, ADR 0003 and ADR 0004):
//
// - the requesters: each Exec Agent requests its three certificates under
//   its own Node's identity (EA-061, EA-062, EA-068); a compromised Host's
//   agent, or a confused one, asks for anything: another Node's address or
//   SPIFFE ID, another trust domain, the wrong signer name or key usages, a
//   subject of its choosing; and other identities (another ServiceAccount,
//   a cluster admin, the Exec Agent's ServiceAccount with a token bound to
//   no pod) ask for anything too;
// - the approvers: the Operator (CT-010 to CT-014), and anyone else granted
//   approve on the signer names, who approves or denies whatever they like;
// - the Operator's signer: it re-checks every approved request, whoever
//   approved it, signs it with the matching CA only when it passes (CT-002,
//   CT-003, CT-006), sets the subject itself (CT-007), and marks a request
//   someone else approved that fails a check Failed;
// - the environment: the CA Secrets can be unreadable for a while, so an
//   approved request waits to be signed.
//
// Each Operator step is one reconcile of internal/controller's
// CertificateSigningRequestReconciler: one review, then one decision, and
// the signature in the same reconcile.
//
// The API server records the requester's user info on the request, so a
// requester cannot choose its username or its Node. What it can choose is
// everything in the PKCS#10 request and spec.usages.
//
// Model citations use //@= and //@# (docs/requirements/README.md, "Model
// citations"). They record what is modelled; they never count as an
// implementation or a test.
module certificates {
  import types.* from "./types"

  // ---------------------------------------------------------------------
  // Parameters

  //@= docs/requirements/09-certificates.md#spiffe-identity
  //@# The Operator SHALL take its SPIFFE trust domain from its
  //@# configuration, and SHALL refuse to start without one.
  pure val TRUST_DOMAIN: str = "example.org"
  pure val OTHER_TRUST_DOMAIN: str = "evil.example"

  // The Exec Agent's ServiceAccount, as spec.username names it.
  pure val EXEC_AGENT_SA: str = "system:serviceaccount:battery-system:exec-agent"

  // The addresses each Host really holds: where battery and consumers dial
  // it. n2 is dual-stack. n3 is a Host whose Node object has been deleted,
  // while a token bound to a pod on it is still valid.
  pure val HOST_ADDRS: HostName -> Set[Address] = Map(
    "n1" -> Set("10.0.0.1"),
    "n2" -> Set("10.0.0.2", "fd00::2"),
    "n3" -> Set("10.0.0.3"),
  )
  pure val HOSTS: Set[HostName] = HOST_ADDRS.keys()
  // The Nodes that exist in the API server.
  pure val NODES: Set[HostName] = Set("n1", "n2")
  // Every address a request can name: the Hosts' and one nobody holds.
  pure val ADDRESSES: Set[Address] = HOSTS.map(h => HOST_ADDRS.get(h)).flatten().union(Set("10.0.0.9"))

  pure def agentOn(n: HostName): Requester = { username: EXEC_AGENT_SA, nodeName: Some(n) }
  pure val AGENTS: Set[Requester] = HOSTS.map(n => agentOn(n))
  // Identities other than the Exec Agents on their Nodes.
  pure val OTHERS: Set[Requester] = Set(
    // the Exec Agent's ServiceAccount, with a token bound to no pod
    { username: EXEC_AGENT_SA, nodeName: None },
    // another workload's ServiceAccount, on n1
    { username: "system:serviceaccount:default:other", nodeName: Some("n1") },
    // a cluster admin
    { username: "kubernetes-admin", nodeName: None },
  )
  pure val REQUESTERS: Set[Requester] = AGENTS.union(OTHERS)

  pure val SIGNERS: Set[SignerName] = Set(FlintlockdServing, FlintlockdClient, ExecAgentServing, OtherSigner)
  pure val USAGES: Set[KeyUsage] = Set(DigitalSignature, KeyEncipherment, ServerAuth, ClientAuth, CertSign)
  pure val SPIFFE_IDS: Set[SpiffeId] =
    tuples(Set(TRUST_DOMAIN, OTHER_TRUST_DOMAIN), Set(HostWorkload, ExecAgentWorkload), HOSTS)
      .map(((td, k, n)) => { trustDomain: td, kind: k, node: n })
      .union(Set({ trustDomain: TRUST_DOMAIN, kind: BatteryWorkload, node: "" }))
  pure val COMMON_NAMES: Set[str] = HOSTS.union(Set("kubernetes-admin"))
  pure val ADDRESS_SETS: Set[Set[Address]] = ADDRESSES.powerset()
  pure val USAGE_SETS: Set[Set[KeyUsage]] = USAGES.powerset()

  // How many requests a trace can create.
  pure val MAX_CSRS: int = 10

  // ---------------------------------------------------------------------
  // State

  // History, for the invariants only; nothing reads it to decide a step.
  type History = {
    // requests the Operator signed while they failed a check (CT-006)
    signedFailing: Set[int],
  }

  // The internal addresses each existing Node's status lists. A kubelet
  // reports its own Node's; the invariants assume it reports the truth
  // (see kubeletReportsAddresses).
  var nodeAddrs: HostName -> Set[Address]
  // Hosts an attacker controls: their Exec Agent's token and their
  // kubelet's credentials.
  var compromised: Set[HostName]
  var csrs: int -> CertificateSigningRequest
  // The Operator can read its CA Secrets.
  var caReadable: bool
  var hist: History

  // ---------------------------------------------------------------------
  // Views

  def csr(id: int): CertificateSigningRequest = csrs.get(id)
  def hasCSR(id: int): bool = csrs.keys().contains(id)

  pure def isApproved(d: Decision): bool = match d {
    | ApprovedBy(_) => true
    | _ => false
  }
  pure def isDenied(d: Decision): bool = match d {
    | DeniedBy(_) => true
    | _ => false
  }
  pure def approvedBy(d: Decision, a: Approver): bool = d == ApprovedBy(a)
  pure def deniedBy(d: Decision, a: Approver): bool = match d {
    | DeniedBy(x) => x.approver == a
    | _ => false
  }
  pure def deniedReason(d: Decision): Option[Check] = match d {
    | DeniedBy(x) => Some(x.reason)
    | _ => None
  }

  //@= docs/requirements/09-certificates.md#signing
  //@# The Operator SHALL approve and sign `CertificateSigningRequest`s
  //@# for the signer names `battery.liquidmetal-x.dev/flintlockd-serving`,
  //@# `battery.liquidmetal-x.dev/flintlockd-client` and
  //@# `battery.liquidmetal-x.dev/exec-agent-serving`, and for no other signer
  //@# name.
  pure def isOurSigner(s: SignerName): bool = s != OtherSigner

  //@= docs/requirements/09-certificates.md#approval
  //@# The Operator SHALL approve a request only when its key usages
  //@# are digital signature and key encipherment, with server auth for
  //@# `flintlockd-serving` and `exec-agent-serving`, or with client auth for
  //@# `flintlockd-client`.
  pure def signerUsages(s: SignerName): Set[KeyUsage] = match s {
    | FlintlockdClient => Set(DigitalSignature, KeyEncipherment, ClientAuth)
    | OtherSigner => Set()
    | _ => Set(DigitalSignature, KeyEncipherment, ServerAuth)
  }

  // The SPIFFE ID a signer name's certificate names for Node n.
  pure def signerId(s: SignerName, n: HostName): SpiffeId = {
    trustDomain: TRUST_DOMAIN,
    kind: if (s == FlintlockdServing) HostWorkload else ExecAgentWorkload,
    node: n,
  }

  //@= docs/requirements/09-certificates.md#signing
  //@# The Operator SHALL sign an approved
  //@# `battery.liquidmetal-x.dev/flintlockd-serving` or
  //@# `battery.liquidmetal-x.dev/exec-agent-serving` request with the serving
  //@# CA, and an approved `battery.liquidmetal-x.dev/flintlockd-client` request
  //@# with the `flintlockd` client CA, each read from its Secret in the
  //@# Operator's namespace.
  pure def signerCA(s: SignerName): CA = if (s == FlintlockdClient) ClientCA else ServingCA

  // The first check a request fails, against the Nodes as the API server
  // holds them; None when it passes every check. The same order as
  // CertificateSigningRequestReconciler.review.
  //
  //@= docs/requirements/09-certificates.md#approval
  //@# The Operator SHALL approve a
  //@# `battery.liquidmetal-x.dev/flintlockd-serving` request only when the
  //@# requester is the Exec Agent's ServiceAccount, the requester's
  //@# `authentication.kubernetes.io/node-name` names an existing Node, and the
  //@# request's subject alternative names are exactly one of that Node's
  //@# internal addresses and `spiffe://<trust domain>/flintlock/host/<node name>`.
  //
  //@= docs/requirements/09-certificates.md#approval
  //@# The Operator SHALL approve a
  //@# `battery.liquidmetal-x.dev/flintlockd-client` request only when the
  //@# requester is the Exec Agent's ServiceAccount and the request's only
  //@# subject alternative name is
  //@# `spiffe://<trust domain>/flintlock/client/exec-agent/<node name>` for the
  //@# Node in the requester's `authentication.kubernetes.io/node-name`.
  //
  //@= docs/requirements/09-certificates.md#approval
  //@# The Operator SHALL approve a
  //@# `battery.liquidmetal-x.dev/exec-agent-serving` request only when the
  //@# requester is the Exec Agent's ServiceAccount, the requester's
  //@# `authentication.kubernetes.io/node-name` names an existing Node, and the
  //@# request's subject alternative names are exactly one of that Node's
  //@# internal addresses and
  //@# `spiffe://<trust domain>/flintlock/client/exec-agent/<node name>`.
  pure def reviewAgainst(nodes: HostName -> Set[Address], r: CertificateSigningRequest): Option[Check] = {
    val n = getOr(r.requester.nodeName, "")
    val uriOk = r.dnsNames == Set() and r.uris == Set(signerId(r.signer, n))
    if (r.requester.username != EXEC_AGENT_SA) Some(CheckRequester)
    else if (isNone(r.requester.nodeName) or n == "") Some(CheckNodeName)
    else if (not(r.selfSigned)) Some(CheckInvalidRequest)
    else if (r.usages != signerUsages(r.signer)) Some(CheckKeyUsages)
    else if (r.signer == FlintlockdClient)
      (if (uriOk and r.ips == Set()) None else Some(CheckSubjectAltNames))
    else if (not(nodes.keys().contains(n))) Some(CheckNodeNotFound)
    else if (nodes.get(n) == Set()) Some(CheckSubjectAltNames)
    else if (uriOk and r.ips.size() == 1 and r.ips.subseteq(nodes.get(n))) None
    else Some(CheckSubjectAltNames)
  }

  def review(r: CertificateSigningRequest): Option[Check] = reviewAgainst(nodeAddrs, r)
  def passes(r: CertificateSigningRequest): bool = isNone(review(r))

  pure def newRequest(req: Requester, s: SignerName, cn: str, ips: Set[Address], uris: Set[SpiffeId],
                      dns: Set[str], selfSigned: bool, usages: Set[KeyUsage]): CertificateSigningRequest = {
    signer: s, requester: req, commonName: cn, ips: ips, uris: uris, dnsNames: dns,
    selfSigned: selfSigned, usages: usages,
    decision: Undecided, failed: None, certificate: None,
  }

  // The request the Exec Agent on Host n makes for signer s, naming address
  // a for a serving certificate (EA-061, EA-062, EA-068).
  pure def agentRequestFor(n: HostName, s: SignerName, a: Address): CertificateSigningRequest =
    newRequest(agentOn(n), s, n,
      if (s == FlintlockdClient) Set() else Set(a),
      Set(signerId(s, n)), Set(), true, signerUsages(s))

  // ---------------------------------------------------------------------
  // Init

  action init = all {
    nodeAddrs' = NODES.mapBy(n => HOST_ADDRS.get(n)),
    compromised' = Set(),
    csrs' = Map(),
    caReadable' = true,
    hist' = { signedFailing: Set() },
  }

  action addRequest(r: CertificateSigningRequest): bool = all {
    csrs.keys().size() < MAX_CSRS,
    csrs' = csrs.put(csrs.keys().size(), r),
  }

  // ---------------------------------------------------------------------
  // Requesters

  //@= docs/requirements/05-exec-agent.md#certificates
  //@# The Exec Agent SHALL obtain its Host's `flintlockd` serving
  //@# certificate with a `CertificateSigningRequest` for the signer
  //@# `battery.liquidmetal-x.dev/flintlockd-serving` that names the Host's
  //@# internal address and the SPIFFE ID
  //@# `spiffe://<trust domain>/flintlock/host/<node name>`, and nothing else.
  //
  //@= docs/requirements/05-exec-agent.md#certificates
  //@# The Exec Agent SHALL obtain its own client certificate with a
  //@# `CertificateSigningRequest` for the signer
  //@# `battery.liquidmetal-x.dev/flintlockd-client` that names the SPIFFE ID
  //@# `spiffe://<trust domain>/flintlock/client/exec-agent/<node name>`, and
  //@# nothing else.
  //
  //@= docs/requirements/05-exec-agent.md#certificates
  //@# The Exec Agent SHALL obtain the serving certificate of its exec
  //@# API with a `CertificateSigningRequest` for the signer
  //@# `battery.liquidmetal-x.dev/exec-agent-serving`, for a key generated on the
  //@# Host, that names the Host's internal address and
  //@# `spiffe://<trust domain>/flintlock/client/exec-agent/<node name>`, and
  //@# nothing else.
  //
  // It asks again whenever it likes, as for a replacement (EA-063).
  action agentRequest(n: HostName, s: SignerName, a: Address): bool = all {
    HOSTS.contains(n),
    not(compromised.contains(n)),
    isOurSigner(s),
    HOST_ADDRS.get(n).contains(a),
    addRequest(agentRequestFor(n, s, a)),
    nodeAddrs' = nodeAddrs, compromised' = compromised, caReadable' = caReadable, hist' = hist,
  }

  // An attacker takes over Host n: its Exec Agent's token, which still
  // names n, and its kubelet's credentials.
  action compromise(n: HostName): bool = all {
    not(compromised.contains(n)),
    compromised' = compromised.union(Set(n)),
    nodeAddrs' = nodeAddrs, csrs' = csrs, caReadable' = caReadable, hist' = hist,
  }

  // A request from anyone who is not an uncompromised Host's Exec Agent: a
  // compromised or confused agent, or another identity. It starts from
  // the request an Exec Agent on some Host would make for some signer
  // name, possibly another Host's, and changes any of its fields, so that
  // most forgeries are near misses.
  action forgedRequest(
    req: Requester, base: HostName, s: SignerName, baseAddr: Address,
    change: { cn: bool, ips: bool, uris: bool, dns: bool, sig: bool, usages: bool },
    cn: str, ips: Set[Address], uri1: SpiffeId, uri2: SpiffeId, nURIs: int,
    usages: Set[KeyUsage],
  ): bool = all {
    not(AGENTS.contains(req)) or compromised.contains(getOr(req.nodeName, "")),
    HOST_ADDRS.get(base).contains(baseAddr),
    val b = agentRequestFor(base, s, baseAddr)
    addRequest({
      ...b,
      requester: req,
      commonName: if (change.cn) cn else b.commonName,
      ips: if (change.ips) ips else b.ips,
      uris: if (not(change.uris)) b.uris else if (nURIs == 0) Set() else if (nURIs == 1) Set(uri1) else Set(uri1, uri2),
      dnsNames: if (change.dns) Set("flintlockd.example") else Set(),
      selfSigned: not(change.sig),
      usages: if (change.usages) usages else b.usages,
    }),
    nodeAddrs' = nodeAddrs, compromised' = compromised, caReadable' = caReadable, hist' = hist,
  }

  // ---------------------------------------------------------------------
  // Other approvers: anyone granted approve on a signer name, such as a
  // cluster admin or another controller. They approve or deny any request
  // nobody has decided yet, for any signer name.

  action foreignDecide(id: int, approve: bool, reason: Check): bool = all {
    hasCSR(id),
    csr(id).decision == Undecided,
    csrs' = csrs.set(id, { ...csr(id),
      decision: if (approve) ApprovedBy(ForeignApprover) else DeniedBy({ approver: ForeignApprover, reason: reason }) }),
    nodeAddrs' = nodeAddrs, compromised' = compromised, caReadable' = caReadable, hist' = hist,
  }

  // ---------------------------------------------------------------------
  // Environment

  // The CA Secrets become unreadable (being rotated, the cache not synced),
  // and readable again.
  action caFlip: bool = all {
    caReadable' = not(caReadable),
    nodeAddrs' = nodeAddrs, compromised' = compromised, csrs' = csrs, hist' = hist,
  }

  // A compromised Host's kubelet writes its own Node's status, including
  // its internal addresses. NodeRestriction lets a kubelet change only its
  // own Node, but any of the addresses on it. Not in step: the invariants
  // assume a Node's internal addresses are its Host's own. The scenario
  // test compromisedKubeletTest takes this step and breaks
  // noCertForAnotherNode (#75).
  action kubeletReportsAddresses(n: HostName, addrs: Set[Address]): bool = all {
    compromised.contains(n),
    nodeAddrs.keys().contains(n),
    nodeAddrs' = nodeAddrs.set(n, addrs),
    compromised' = compromised, csrs' = csrs, caReadable' = caReadable, hist' = hist,
  }

  // ---------------------------------------------------------------------
  // The Operator: CertificateSigningRequestReconciler.Reconcile

  // The certificate the Operator issues for a request that passed.
  //
  //@= docs/requirements/09-certificates.md#signing
  //@# The Operator SHALL set the subject of every certificate it
  //@# issues to a common name of the requester's Node name, ignoring the
  //@# subject the request asks for.
  pure def issue(r: CertificateSigningRequest): Certificate = {
    ca: signerCA(r.signer),
    commonName: getOr(r.requester.nodeName, ""),
    ips: r.ips,
    uris: r.uris,
    dnsNames: r.dnsNames,
    usages: r.usages,
  }

  // Whether a reconcile of the request changes anything. The Operator
  // skips other signer names, and requests already signed, Failed or
  // Denied. Signing an approved request that passes needs the CA Secret;
  // without it the reconcile returns an error and changes nothing.
  //
  //@= docs/requirements/09-certificates.md#signing
  //@# The Operator SHALL sign a request only while it carries the
  //@# condition `Approved` and not the condition `Denied`.
  def canReconcile(id: int): bool = and {
    hasCSR(id),
    isOurSigner(csr(id).signer),
    isNone(csr(id).certificate),
    isNone(csr(id).failed),
    not(isDenied(csr(id).decision)),
    (isApproved(csr(id).decision) and passes(csr(id))) implies caReadable,
  }

  // Signs r, recording in the history whether it passed its checks.
  action signRequest(id: int, r: CertificateSigningRequest): bool = all {
    csrs' = csrs.set(id, { ...r, certificate: Some(issue(r)) }),
    hist' = if (passes(r)) hist else { ...hist, signedFailing: hist.signedFailing.union(Set(id)) },
  }

  //@= docs/requirements/09-certificates.md#approval
  //@# If a request for any of the three signer names fails any check,
  //@# then the Operator SHALL deny it with a reason that names the check.
  //
  //@= docs/requirements/09-certificates.md#signing
  //@# The Operator SHALL sign a request only when it passes every
  //@# check of [Approval](#approval) for its signer name, whoever approved it.
  //
  // One reconcile. A request nobody has decided is denied with the check
  // it fails, or approved and, when the CA Secret can be read, signed. An
  // approved request, whoever approved it, is reviewed again: it is marked
  // Failed with the check it fails, since Approved cannot be withdrawn nor
  // Denied added, or signed.
  action opReconcile(id: int): bool = all {
    canReconcile(id),
    val r = csr(id)
    val d = review(r)
    if (r.decision == Undecided)
      match d {
        | Some(c) => all {
            csrs' = csrs.set(id, { ...r, decision: DeniedBy({ approver: OperatorApprover, reason: c }) }),
            hist' = hist,
          }
        | None =>
            val approved = { ...r, decision: ApprovedBy(OperatorApprover) }
            if (caReadable) signRequest(id, approved)
            else all { csrs' = csrs.set(id, approved), hist' = hist }
      }
    else
      match d {
        | Some(c) => all { csrs' = csrs.set(id, { ...r, failed: Some(c) }), hist' = hist }
        | None => signRequest(id, r)
      },
    nodeAddrs' = nodeAddrs, compromised' = compromised, caReadable' = caReadable,
  }

  def operatorIdle: bool = csrs.keys().forall(id => not(canReconcile(id)))

  // ---------------------------------------------------------------------
  // Step

  action step = {
    nondet id = 0.to(MAX_CSRS - 1).oneOf()
    nondet n = HOSTS.oneOf()
    nondet a = HOST_ADDRS.get(n).oneOf()
    nondet base = HOSTS.oneOf()
    nondet baseAddr = HOST_ADDRS.get(base).oneOf()
    nondet s = SIGNERS.oneOf()
    nondet req = REQUESTERS.oneOf()
    nondet cn = COMMON_NAMES.oneOf()
    nondet ips = ADDRESS_SETS.oneOf()
    nondet uri1 = SPIFFE_IDS.oneOf()
    nondet uri2 = SPIFFE_IDS.oneOf()
    nondet nURIs = 0.to(2).oneOf()
    nondet usages = USAGE_SETS.oneOf()
    nondet chCN = Bool.oneOf()
    nondet chIPs = Bool.oneOf()
    nondet chURIs = Bool.oneOf()
    nondet chDNS = Bool.oneOf()
    nondet chSig = Bool.oneOf()
    nondet chUsages = Bool.oneOf()
    nondet approve = Bool.oneOf()
    nondet reason = Set(CheckRequester, CheckSubjectAltNames).oneOf()
    val change = { cn: chCN, ips: chIPs, uris: chURIs, dns: chDNS, sig: chSig, usages: chUsages }
    any {
      // requesters
      agentRequest(n, s, a),
      compromise(n),
      forgedRequest(req, base, s, baseAddr, change, cn, ips, uri1, uri2, nURIs, usages),
      // other approvers
      foreignDecide(id, approve, reason),
      // environment
      caFlip,
      // the Operator
      opReconcile(id),
    }
  }

  // ---------------------------------------------------------------------
  // Invariants. Each names the requirement or ADR consequence it checks.

  def certified: Set[int] = csrs.keys().filter(id => isSome(csr(id).certificate))
  def certOf(id: int): Certificate = match csr(id).certificate {
    | Some(c) => c
    | None => issue(csr(id))
  }

  // CT-010, CT-011 and CT-014; ADR 0003, consequence 1: no requester
  // obtains a certificate that names another Host's address or SPIFFE ID.
  // Every certificate belongs to a requester on a Host, and names only that
  // Host's own addresses and SPIFFE IDs.
  val noCertForAnotherNode: bool =
    certified.forall(id => match csr(id).requester.nodeName {
      | Some(n) => and {
          HOSTS.contains(n),
          certOf(id).ips.subseteq(HOST_ADDRS.get(n)),
          certOf(id).uris.forall(u => u.node == n),
        }
      | None => false
    })

  // CT-010, CT-011 and CT-014: only the Exec Agent's ServiceAccount, with a
  // token bound to a pod, obtains a certificate.
  val onlyExecAgentsCertified: bool =
    certified.forall(id => AGENTS.contains(csr(id).requester))

  // CT-003: a certificate only for a request that carries Approved and not
  // Denied.
  val signedOnlyWhenApproved: bool =
    certified.forall(id => isApproved(csr(id).decision))

  // CT-006: nothing is signed without passing every check, whoever
  // approved it.
  val signedOnlyAfterChecks: bool = hist.signedFailing == Set()

  // CT-006: a request someone else approved that fails a check never gets
  // a certificate.
  val foreignApprovalNeverBypassesChecks: bool =
    csrs.keys().forall(id =>
      (approvedBy(csr(id).decision, ForeignApprover) and not(passes(csr(id)))) implies isNone(csr(id).certificate))

  // CT-007: every certificate's subject is the requester's Node, whatever
  // the request asked for.
  val subjectIsRequesterNode: bool =
    certified.forall(id => Some(certOf(id).commonName) == csr(id).requester.nodeName)

  // CT-020: every certificate's SPIFFE ID sits under the configured trust
  // domain, and names a Host's workload, never battery.
  val identitiesInTrustDomain: bool =
    certified.forall(id => certOf(id).uris.forall(u => u.trustDomain == TRUST_DOMAIN and u.kind != BatteryWorkload))

  // CT-002 and CT-012: each certificate comes from its signer name's CA,
  // with its signer name's key usages, and names no DNS name.
  val caAndUsagesMatchSigner: bool =
    certified.forall(id => and {
      certOf(id).ca == signerCA(csr(id).signer),
      certOf(id).usages == signerUsages(csr(id).signer),
      certOf(id).dnsNames == Set(),
    })

  // CT-001: the Operator approves, denies, fails and signs nothing for
  // another signer name.
  val onlyOurSignerNames: bool =
    csrs.keys().forall(id => not(isOurSigner(csr(id).signer)) implies and {
      not(approvedBy(csr(id).decision, OperatorApprover)),
      not(deniedBy(csr(id).decision, OperatorApprover)),
      isNone(csr(id).failed),
      isNone(csr(id).certificate),
    })

  // CT-013: the Operator denies a request, or marks it Failed, only for a
  // check it fails, and names that check.
  val refusalNamesTheCheck: bool =
    csrs.keys().forall(id => and {
      deniedBy(csr(id).decision, OperatorApprover) implies deniedReason(csr(id).decision) == review(csr(id)),
      isSome(csr(id).failed) implies csr(id).failed == review(csr(id)),
    })

  // CT-001, CT-006 and CT-013: once the Operator has nothing left to do and
  // can read its CAs, every request for its signer names is settled:
  // signed, Denied or Failed.
  val idleSettled: bool =
    (operatorIdle and caReadable) implies
      csrs.keys().forall(id => isOurSigner(csr(id).signer) implies or {
        isSome(csr(id).certificate),
        isDenied(csr(id).decision),
        isSome(csr(id).failed),
      })

  // All the invariants CI checks.
  val safety: bool = and {
    noCertForAnotherNode,
    onlyExecAgentsCertified,
    signedOnlyWhenApproved,
    signedOnlyAfterChecks,
    foreignApprovalNeverBypassesChecks,
    subjectIsRequesterNode,
    identitiesInTrustDomain,
    caAndUsagesMatchSigner,
    onlyOurSignerNames,
    refusalNamesTheCheck,
    idleSettled,
  }

  // CT-013 as written: once the Operator is idle, every request for its
  // signer names that fails a check is Denied. Not in safety: a request
  // someone else approved cannot be Denied, and the Operator marks it
  // Failed instead, which the requirements do not mention (#76).
  // certificates_test.qnt reaches the violation.
  val failingRequestsDenied: bool =
    operatorIdle implies
      csrs.keys().forall(id => (isOurSigner(csr(id).signer) and not(passes(csr(id)))) implies isDenied(csr(id).decision))

  // ---------------------------------------------------------------------
  // Witnesses: states quint run reports reaching, to show the simulation
  // gets to the interesting interleavings.

  // an Exec Agent holds all three of its certificates
  val witnessAgentFullyCertified: bool =
    HOSTS.exists(n => Set(FlintlockdServing, FlintlockdClient, ExecAgentServing).forall(s =>
      certified.exists(id => csr(id).requester == agentOn(n) and csr(id).signer == s)))
  // a request someone else approved failed its checks and was marked Failed
  val witnessForeignApprovalFailed: bool =
    csrs.keys().exists(id => approvedBy(csr(id).decision, ForeignApprover) and isSome(csr(id).failed))
  // a request someone else approved passed its checks and was signed
  val witnessForeignApprovalSigned: bool =
    csrs.keys().exists(id => approvedBy(csr(id).decision, ForeignApprover) and isSome(csr(id).certificate))
  // a compromised Host's agent asked for another Host's address or SPIFFE
  // ID, with everything else right, and was denied
  pure def namesAnotherHost(r: CertificateSigningRequest): bool = {
    val own = getOr(r.requester.nodeName, "")
    HOSTS.contains(own) and or {
      r.ips.exists(ip => not(HOST_ADDRS.get(own).contains(ip))),
      r.uris.exists(u => u.node != own),
    }
  }
  val witnessAnotherHostDenied: bool =
    csrs.keys().exists(id => and {
      AGENTS.contains(csr(id).requester),
      compromised.contains(getOr(csr(id).requester.nodeName, "")),
      namesAnotherHost(csr(id)),
      deniedReason(csr(id).decision) == Some(CheckSubjectAltNames),
      deniedBy(csr(id).decision, OperatorApprover),
    })
  // a certificate whose request asked for another subject
  val witnessSubjectIgnored: bool =
    certified.exists(id => csr(id).commonName != certOf(id).commonName)
  // the Operator approved a request and waits for its CA to sign it
  val witnessApprovedAwaitingCA: bool =
    csrs.keys().exists(id => approvedBy(csr(id).decision, OperatorApprover) and isNone(csr(id).certificate))
}
