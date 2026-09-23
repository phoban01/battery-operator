# 0006. Unit tests with fakes, and end-to-end tests on kind

- **Status:** Accepted
- **Date:** 2026-09-23

## Context

The first waves tested most things against envtest: a kube-apiserver and
etcd started in the test process, with this project's CRDs installed. It
answered real questions, such as whether a CEL rule is enforced and
whether a TokenReview behaves as documented. But envtest is neither a unit
test nor a real cluster:

- It is slow to start and heavy to run in parallel, and each worktree
  downloads its binaries.
- It runs no controllers, no kubelet, no garbage collector, no admission
  webhooks by default and no Pods. Tests have to fake a pod's token and a
  CSR's requester, and can't show the Manifests working.
- Logic tested through it drags an API server into what should be a fast,
  focused test.

The scope and subreconciler structure (CLAUDE.md, #58) puts a controller's
logic where it can be tested without any API server. What remains is the
behaviour of the system as deployed.

kubebuilder scaffolds an e2e suite on kind, written with Ginkgo. The
maintainer prefers
[sigs.k8s.io/e2e-framework](https://github.com/kubernetes-sigs/e2e-framework):
plain `go test`, features and assessments, and kind cluster lifecycle
built in.

battery publishes `poolmgrd` images (`ghcr.io/liquidmetal-dev/poolmgrd`), so
an e2e run can use the real battery as the Operator's sidecar.

## Decision

1. **Two layers of tests.**
   - **Unit tests** (`make test`): each subreconciler, and the rest of the
     logic, against the fake battery, the fake `flintlockd` and
     controller-runtime's fake client. No API server.
   - **The e2e suite** (`test/e2e`): e2e-framework creates a kind cluster,
     deploys the Manifests with the Operator, the real `poolmgrd` as its
     sidecar, the Exec Agent and the fake `flintlockd` as each Host's
     `flintlockd`, and tests the system as a user would. It covers
     everything that needs an API server: CRD validation, admission
     policies, TokenReview and TokenRequest, CSRs, garbage collection,
     RBAC as shipped.
2. **e2e-framework, not Ginkgo.** kubebuilder's Ginkgo e2e scaffold is
   replaced.
3. **envtest only where neither layer can do it,** with the reason written
   beside the use. The envtest suites that exist are moved onto the two
   layers.
4. **The e2e suite runs in CI** on every PR, as a required check once it is
   stable.

## Consequences

1. The fake `flintlockd` needs a binary and an image of its own, so that it
   can run as each kind node's Host, and the Host prerequisites (KVM, the
   thin pool) need test values the Exec Agent accepts in kind.
2. The e2e suite depends on the Manifests (#18), so the order is: deploy,
   then the e2e harness, then migrating the envtest suites.
3. CI needs kind, which runs in Docker. Running it inside a Dagger function
   needs Docker in Dagger; running it on the GitHub runner is simpler. The
   harness issue decides.
4. An e2e run takes minutes. Unit tests stay the fast feedback loop.
5. Requirements TD-020 to TD-022 are withdrawn and replaced by TD-023 to
   TD-027.
