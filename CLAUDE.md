# battery-operator: working conventions

A Kubernetes operator in front of an unmodified
[battery](https://github.com/liquidmetal-dev/battery). Work happens one issue
per fresh session. This file is everything a session needs beyond the issue.
`AGENTS.md` and `CONTRIBUTING.md` point here.

## Start from the issue

1. Read the issue: `gh issue view N -R phoban01/battery-operator`.
2. Read the ADRs it cites in [docs/adr/](docs/adr/), and
   [docs/requirements/README.md](docs/requirements/README.md).
3. Read the requirement documents for the IDs on the issue's `Owns:` line
   (under `## Requirements`), and only the files the issue names.
4. Don't read or change code beyond what the issue needs.

The roadmap and the order of the work are in the pinned issue #24.

## One issue, one branch, one PR

- Branch from `main`, named `<issue>-<slug>`, for example `5-pool-type`.
- One PR against `main`. Its body has, each on a line of its own:
  - `Closes #N`
  - the issue's `Owns:` line, exactly as the issue gives it (`Owns: none`
    for tooling and docs).
- Too big for one PR? Open new issues for the rest, add them to the roadmap
  #24, and keep the PR to what the issue asked.
- Never merge your own PR. The maintainer merges, with squash.
- Wait for CI: `gh pr checks <n> -R phoban01/battery-operator --watch`.

## Commits

- Commits are signed. The git config already does this: don't change git
  config.
- `main` is protected: a PR, signed commits and linear history are required.
- End each commit message with the attribution trailer your harness
  specifies, and the PR body with the attribution line it specifies.

## Requirements and duvet

Every behaviour is specified as an EARS requirement in
[docs/requirements/](docs/requirements/), and traced to code with duvet. The
README there has the rules; the short version:

- Each owned ID is cited in the implementation (`//=` and `//#` lines) and in
  a test (the same, plus a `//= type=test` line). YAML under `config/` uses
  `#=` and `#/`.
- Keep a blank line between a citation and the doc comment of the `func` or
  type below it. Otherwise `gofmt` rewrites `//=` to `// =`, and duvet ignores
  the citation without an error.
- duvet reads only files git knows about: `git add` new files before
  checking locally.
- CI's coverage gate fails the PR unless every ID on its `Owns:` line has
  both. `type=implication` does not count.
- `make duvet` refreshes `.duvet/snapshot.txt`. Don't commit that change
  unless the PR is a milestone that says `Snapshot: milestone` on its own
  line. Restore it with `git checkout origin/main -- .duvet/snapshot.txt`.
- Check your IDs locally: `make coverage-gate IDS="CL-001 CL-002"`.
- CI's citation ratchet also fails the PR if any requirement, owned or not,
  that had both citations at the merge base with `main` loses either one.
  Check it locally with `make coverage-ratchet`. A requirement is retired
  by marking it `(withdrawn)`, never by deleting it.
- The Quint models under `specs/quint/` cite requirements with model
  citations: `//@=` and `//@#`, and no `type=` line. They show a
  requirement as *modelled* and never count for the coverage gate, so a
  modelled ID still needs its Go implementation and test citations. Never
  use `type=test` or `type=implication` for a model. `make duvet` fails if
  the list of modelled requirements in `docs/requirements/README.md` is out
  of date; `make duvet-models-write` rewrites it.

## Models

- `specs/quint/` holds [Quint](https://quint-lang.org) models of the
  Operator's systems, one module per system, with the shared types in
  `types.qnt`. Its README says what each model covers.
- `make quint` (CI: `dagger call quint`) typechecks and tests them, and
  simulates them against their invariants.
- A PR that changes a behaviour a model covers changes the model too.
- If a model disagrees with the requirements or the code, file an issue.
  Don't change the model to hide the disagreement.

## Tooling

- Work inside devbox: `devbox shell`, or `devbox run -- <cmd>`. It pins Go,
  kubebuilder, kubectl, kind, duvet and quint.
- `GOPATH` is the project-local `.gopath/`, and `GOTOOLCHAIN=local`.
- Kubebuilder's Makefile pins controller-gen, setup-envtest, golangci-lint
  and kustomize, and installs them into `bin/` on first use.
- Main targets: `make build`, `make test`, `make lint`, `make duvet`,
  `make quint`.
- CI is a Dagger module (`.dagger/`): run the same checks locally with
  `devbox run -- dagger call <lint|test|build|check-generated|requirements|quint>`.

## Tests

Two layers ([ADR 0006](docs/adr/0006-unit-tests-and-kind-e2e.md),
`08-test-doubles.md#test-environments`):

- **Unit tests** (`make test`): subreconcilers and other logic, against the
  fake battery (`internal/fakebattery`), the fake `flintlockd`
  (`internal/fakeflintlock`) and controller-runtime's fake client. No API
  server.
- **The e2e suite** (`test/e2e`), written with
  [sigs.k8s.io/e2e-framework](https://github.com/kubernetes-sigs/e2e-framework),
  not Ginkgo: a kind cluster running the Manifests, with battery's real
  `poolmgrd` as the Operator's sidecar and the fake `flintlockd` on each
  Host. Anything that needs an API server is tested here: CRD validation,
  admission policies, TokenReview, CSRs, RBAC as shipped.
- **Avoid envtest.** Use it only where neither layer can exercise a
  behaviour, and say why beside the use (TD-027). No test uses it now, and
  `test/layers` fails on a use that does not say why.
- There is no KVM and no battery daemon in development; the fakes and kind
  stand in.

## Hard constraints

From the ADRs; read them for the reasons.

- battery is not changed. It runs as a sidecar in the Operator's pod, on
  loopback, and the Operator is a client of its gRPC API
  ([0001](docs/adr/0001-standalone-operator-over-battery-grpc.md)).
- The API group is `battery.liquidmetal-x.dev`, version `v1alpha1`. The
  resources are `Pool` and `MicroVMClaim`. There is no `MicroVM` resource.
- `flintlockd` is reached over mutual TLS
  ([0002](docs/adr/0002-battery-reaches-flintlockd-over-mtls.md)).
- Host certificates come through Kubernetes `CertificateSigningRequest`s
  that the Operator approves and signs. No SPIRE
  ([0003](docs/adr/0003-host-certificates-through-kubernetes-csrs.md)).
- A decision that changes any of this is a new ADR, not an edit.

## Layout

Now:

| Path | What |
|------|------|
| `cmd/main.go` | The Operator's manager entry point |
| `internal/battery/` | The Operator's battery gRPC client (#8); pins battery v0.3.3 |
| `internal/batterysidecar/` | battery as the Operator's sidecar: its configuration file, and restarting it (DP-006) |
| `config/` | Kustomize Manifests, from kubebuilder; `config/default` is the entry point, with battery as a sidecar and cert-manager's certificates (`config/certificates`) |
| `internal/manifests/` | Tests of the Manifests as `kustomize build` renders them |
| `test/e2e/`, `test/utils/` | kubebuilder's e2e tests, against kind |
| `test/layers/` | Checks that the tests keep to their two layers: envtest only where a file cites TD-027 and says why |
| `docs/adr/` | Architecture decision records |
| `docs/requirements/` | EARS requirements, `.duvet/` their configs (code, and models) and snapshot |
| `hack/` | Boilerplate header, `duvet-coverage.sh`, `duvet-models.sh`, `quint.sh` |
| `specs/quint/` | Quint models and their shared types |

Where the issues put new things:

| Path | What | Issue |
|------|------|-------|
| `api/v1alpha1/` | `Pool`, `MicroVMClaim` types | #5, #6 |
| `internal/controller/` | Controllers | #9, #13, #19, #30 |
| `internal/clock/`, `internal/fakebattery/`, `internal/fakeflintlock/` | Clock and fakes | #7 |
| `cmd/exec-agent/`, `internal/execagent/`, `internal/hostcheck/`, `config/exec-agent/` | Exec Agent binary, package, Host checks and its manifests | #15 |
| `pkg/claimclient/` (proposed) | Client Library | #22 |

## Kubebuilder

- New APIs and controllers start from the CLI, never by hand:
  - `kubebuilder create api --group battery --version v1alpha1 --kind <Kind>`
  - a controller for a type from elsewhere: add
    `--controller=true --resource=false`.
  - webhooks: `kubebuilder create webhook`.
- Never edit generated files: `config/crd/bases/`, `config/rbac/role.yaml`,
  `config/webhook/manifests.yaml`, `**/zz_generated.*.go`, `PROJECT`.
- Never remove `// +kubebuilder:scaffold:*` markers; the CLI inserts code
  there.
- After changing `*_types.go` or markers: `make manifests generate`.
- After changing Go: `make lint-fix test`.
- API: `metav1.Condition` for status, `metav1.Time` for times, standard
  Kubernetes API conventions.
- Controllers: idempotent reconciles; RBAC through `+kubebuilder:rbac`
  markers; finalizers for external cleanup; watch secondary resources
  rather than polling.
- Controllers are thin: see [Controller structure](#controller-structure).
- Log messages follow the Kubernetes style: capitalised, no full stop, past
  tense, object type named, balanced key-value pairs.
- Reference: the [Kubebuilder Book](https://book.kubebuilder.io) and the
  [API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md).

## Controller structure

A controller is a thin integration layer. It fetches the object, builds a
**scope**, runs a chain of **subreconcilers**, and writes the object and
its status once at the end. The logic lives in the subreconcilers, which
are small, testable on their own, and composable across controllers (#58).

- **Scope:** the per-reconcile context. It holds the object and a copy of it
  as fetched, the clients (Kubernetes, battery), the logger, the clock, and
  the conditions and result so far. The scope owns the single patch at the
  end, so a subreconciler changes the scope and never writes to the API
  server itself.
- **Subreconciler:** one concern, `Reconcile(ctx, *Scope[T]) (Result,
  error)`. It returns whether the chain continues, requeues or stops.
  - Generic ones work for any `T client.Object`: finalizers,
    `observedGeneration`, conditions, deletion.
  - Domain ones hold a controller's steps: a claim's bind, renew, expire and
    release (`02-claims.md`); a Pool's declare, place and status
    (`03-pools.md`).
- **Controller:** builds the scope, runs the chain in order, patches once,
  and declares its watches and `+kubebuilder:rbac` markers. Nothing else.
- **Tests:** unit-test each subreconciler against a scope with fakes (the
  fake battery, a fake client); no API server is needed for the logic. The
  e2e suite covers the controller's wiring and what the API server
  enforces.
- **Citations** go on the subreconciler that implements the requirement,
  and on its test.

The shared building blocks (`Scope[T]`, the subreconciler interface, a way
to chain them, and the generic subreconcilers) arrive with #58. Until then,
a new controller keeps the same shape with local types, so that moving it
onto the shared ones later changes no logic.
