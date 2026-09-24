// -*- mode: Bluespec; -*-
//
// Scenario tests for the certificates model (certificates.qnt). Each run
// walks one interleaving step by step and checks the state it reaches;
// quint test runs every definition whose name ends in Test.
module certificates_test {
  import types.* from "./types"
  import certificates.* from "./certificates"

  pure val keepAll = { cn: false, ips: false, uris: false, dns: false, sig: false, usages: false }
  pure val noURI: SpiffeId = { trustDomain: "", kind: HostWorkload, node: "" }
  pure val admin: Requester = { username: "kubernetes-admin", nodeName: None }
  pure val unboundAgent: Requester = { username: EXEC_AGENT_SA, nodeName: None }

  // req submits the request the Exec Agent on base would make for s,
  // unchanged.
  run copyOf(req: Requester, base: HostName, s: SignerName, addr: Address): bool =
    forgedRequest(req, base, s, addr, keepAll, "", Set(), noURI, noURI, 0, Set())

  // req submits the request the Exec Agent on base would make for s, naming
  // the addresses ips instead.
  run withIPs(req: Requester, base: HostName, s: SignerName, addr: Address, ips: Set[Address]): bool =
    forgedRequest(req, base, s, addr, { ...keepAll, ips: true }, "", ips, noURI, noURI, 0, Set())

  pure val servingCN1: Certificate = {
    ca: ServingCA, commonName: "n1", ips: Set("10.0.0.1"),
    uris: Set({ trustDomain: TRUST_DOMAIN, kind: HostWorkload, node: "n1" }),
    dnsNames: Set(), usages: Set(DigitalSignature, KeyEncipherment, ServerAuth),
  }

  // EA-061, EA-062, EA-068, CT-010, CT-011, CT-014: an Exec Agent obtains
  // its three certificates, each from its signer name's CA.
  run agentCertifiedTest =
    init
      .then(agentRequest("n1", FlintlockdServing, "10.0.0.1"))
      .then(agentRequest("n1", FlintlockdClient, "10.0.0.1"))
      .then(agentRequest("n2", ExecAgentServing, "fd00::2"))
      .then(opReconcile(0))
      .then(opReconcile(1))
      .then(opReconcile(2))
      .expect(and {
        csr(0).decision == ApprovedBy(OperatorApprover),
        csr(0).certificate == Some(servingCN1),
        certOf(1).ca == ClientCA,
        certOf(1).ips == Set(),
        certOf(1).uris == Set({ trustDomain: TRUST_DOMAIN, kind: ExecAgentWorkload, node: "n1" }),
        certOf(2).ca == ServingCA,
        certOf(2).ips == Set("fd00::2"),
        certOf(2).uris == Set({ trustDomain: TRUST_DOMAIN, kind: ExecAgentWorkload, node: "n2" }),
        operatorIdle,
        safety,
      })

  // CT-010, CT-013, ADR 0003 consequence 1: a compromised Host's agent
  // asks for another Host's certificate, whole or for its address only, and
  // is denied for the subject alternative names.
  run anotherHostDeniedTest =
    init
      .then(compromise("n1"))
      .then(copyOf(agentOn("n1"), "n2", FlintlockdServing, "10.0.0.2"))
      .then(withIPs(agentOn("n1"), "n1", ExecAgentServing, "10.0.0.1", Set("10.0.0.2")))
      .then(withIPs(agentOn("n1"), "n1", FlintlockdServing, "10.0.0.1", Set("10.0.0.1", "10.0.0.2")))
      .then(opReconcile(0))
      .then(opReconcile(1))
      .then(opReconcile(2))
      .expect(and {
        Set(0, 1, 2).forall(id => csr(id).decision == DeniedBy({ approver: OperatorApprover, reason: CheckSubjectAltNames })),
        certified == Set(),
        witnessAnotherHostDenied,
        safety,
      })

  // CT-010, CT-011, CT-014: other identities are denied for the requester.
  run otherIdentitiesDeniedTest =
    init
      .then(copyOf(admin, "n1", FlintlockdClient, "10.0.0.1"))
      .then(copyOf(unboundAgent, "n1", FlintlockdClient, "10.0.0.1"))
      .then(opReconcile(0))
      .then(opReconcile(1))
      .expect(and {
        csr(0).decision == DeniedBy({ approver: OperatorApprover, reason: CheckRequester }),
        csr(1).decision == DeniedBy({ approver: OperatorApprover, reason: CheckNodeName }),
        safety,
      })

  // CT-006, CT-008: a request someone else approved is checked again, marked
  // Failed with the check it fails, and never signed.
  run foreignApprovalRecheckedTest =
    init
      .then(compromise("n1"))
      .then(copyOf(agentOn("n1"), "n2", FlintlockdServing, "10.0.0.2"))
      .then(foreignDecide(0, true, CheckRequester))
      .then(opReconcile(0))
      .expect(and {
        csr(0).decision == ApprovedBy(ForeignApprover),
        csr(0).failed == Some(CheckSubjectAltNames),
        isNone(csr(0).certificate),
        witnessForeignApprovalFailed,
        operatorIdle,
        safety,
      })

  // CT-006: a request someone else approved that passes is signed.
  run foreignApprovalSignedTest =
    init
      .then(agentRequest("n1", FlintlockdServing, "10.0.0.1"))
      .then(foreignDecide(0, true, CheckRequester))
      .then(opReconcile(0))
      .expect(and { csr(0).certificate == Some(servingCN1), safety })

  // CT-003: the Operator never signs a request someone else denied, even
  // one that passes its checks.
  run foreignDenialStandsTest =
    init
      .then(agentRequest("n1", FlintlockdServing, "10.0.0.1"))
      .then(foreignDecide(0, false, CheckRequester))
      .then(opReconcile(0))
      .fail()

  // CT-013 and CT-008: of two requests that fail the same check, the one
  // nobody approved is Denied, and the one someone else approved is marked
  // Failed, not Denied; both name the check (#76).
  run failedOrDeniedTest =
    init
      .then(compromise("n1"))
      .then(copyOf(agentOn("n1"), "n2", FlintlockdServing, "10.0.0.2"))
      .then(copyOf(agentOn("n1"), "n2", FlintlockdServing, "10.0.0.2"))
      .then(foreignDecide(0, true, CheckRequester))
      .then(opReconcile(0))
      .then(opReconcile(1))
      .expect(and {
        csr(0).decision == ApprovedBy(ForeignApprover),
        csr(0).failed == Some(CheckSubjectAltNames),
        csr(1).decision == DeniedBy({ approver: OperatorApprover, reason: CheckSubjectAltNames }),
        isNone(csr(1).failed),
        certified == Set(),
        operatorIdle,
        unapprovedFailingDenied,
        approvedFailingFailed,
        safety,
      })

  // CT-007: the subject the request asks for is ignored.
  run subjectIgnoredTest =
    init
      .then(compromise("n1"))
      .then(forgedRequest(agentOn("n1"), "n1", FlintlockdServing, "10.0.0.1", { ...keepAll, cn: true },
        "n2", Set(), noURI, noURI, 0, Set()))
      .then(opReconcile(0))
      .expect(and {
        csr(0).commonName == "n2",
        certOf(0).commonName == "n1",
        witnessSubjectIgnored,
        safety,
      })

  // CT-012: the wrong key usages are denied.
  run keyUsagesDeniedTest =
    init
      .then(compromise("n1"))
      .then(forgedRequest(agentOn("n1"), "n1", FlintlockdClient, "10.0.0.1", { ...keepAll, usages: true },
        "", Set(), noURI, noURI, 0, Set(DigitalSignature, KeyEncipherment, ClientAuth, CertSign)))
      .then(opReconcile(0))
      .expect(and {
        csr(0).decision == DeniedBy({ approver: OperatorApprover, reason: CheckKeyUsages }),
        safety,
      })

  // CT-020: a SPIFFE ID in another trust domain is denied.
  run otherTrustDomainDeniedTest =
    init
      .then(compromise("n1"))
      .then(forgedRequest(agentOn("n1"), "n1", FlintlockdClient, "10.0.0.1", { ...keepAll, uris: true },
        "", Set(), { trustDomain: OTHER_TRUST_DOMAIN, kind: ExecAgentWorkload, node: "n1" }, noURI, 1, Set()))
      .then(opReconcile(0))
      .expect(and {
        csr(0).decision == DeniedBy({ approver: OperatorApprover, reason: CheckSubjectAltNames }),
        safety,
      })

  // CT-001: the Operator leaves other signer names alone, approved or not.
  run otherSignerIgnoredTest =
    init
      .then(copyOf(admin, "n1", OtherSigner, "10.0.0.1"))
      .then(foreignDecide(0, true, CheckRequester))
      .then(opReconcile(0))
      .fail()

  // CT-010 and CT-011: a Host whose Node is gone gets no serving
  // certificate, but its client certificate needs no Node.
  run nodeNotFoundTest =
    init
      .then(agentRequest("n3", FlintlockdServing, "10.0.0.3"))
      .then(agentRequest("n3", FlintlockdClient, "10.0.0.3"))
      .then(opReconcile(0))
      .then(opReconcile(1))
      .expect(and {
        csr(0).decision == DeniedBy({ approver: OperatorApprover, reason: CheckNodeNotFound }),
        isSome(csr(1).certificate),
        safety,
      })

  // CT-002: an approved request waits while the CA Secret cannot be read,
  // and is signed once it can.
  run caUnreadableTest =
    init
      .then(caFlip)
      .then(agentRequest("n1", FlintlockdServing, "10.0.0.1"))
      .then(opReconcile(0))
      .expect(and {
        csr(0).decision == ApprovedBy(OperatorApprover),
        isNone(csr(0).certificate),
        operatorIdle,
        witnessApprovedAwaitingCA,
        safety,
      })
      .then(caFlip)
      .then(opReconcile(0))
      .expect(and { csr(0).certificate == Some(servingCN1), safety })

  // A compromised Host's kubelet lists another Host's address as its own
  // Node's internal address. Every check passes, and the Host's agent
  // obtains a flintlockd serving certificate for the other Host's address,
  // which battery v0.3.3 verifies flintlockd by (#75).
  run compromisedKubeletTest =
    init
      .then(compromise("n1"))
      .then(kubeletReportsAddresses("n1", Set("10.0.0.1", "10.0.0.2")))
      .then(withIPs(agentOn("n1"), "n1", FlintlockdServing, "10.0.0.1", Set("10.0.0.2")))
      .then(opReconcile(0))
      .expect(and {
        certOf(0).ips == Set("10.0.0.2"),
        signedOnlyAfterChecks,
        not(noCertForAnotherNode),
      })
}
