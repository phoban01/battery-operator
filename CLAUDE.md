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

## Tooling

- Work inside devbox: `devbox shell`, or `devbox run -- <cmd>`. It pins Go,
  kubebuilder, kubectl, kind and duvet.
- `GOPATH` is the project-local `.gopath/`, and `GOTOOLCHAIN=local`.
- Kubebuilder's Makefile pins controller-gen, setup-envtest, golangci-lint
  and kustomize, and installs them into `bin/` on first use.
- Main targets: `make build`, `make test`, `make lint`, `make duvet`.
- CI and images are becoming Dagger functions (#3, #33). Until #3 lands, CI
  is the duvet workflow in `.github/workflows/requirements.yml`.

## Tests

- There is no KVM and no battery daemon in development.
- Controllers are tested with envtest (`make test`) and the fake battery;
  the Exec Agent with envtest and the fake `flintlockd`. The fakes arrive
  with #7 and #15. See
  [08-test-doubles.md](docs/requirements/08-test-doubles.md).
- `make test-e2e` needs a throwaway kind cluster; it is not part of the
  normal loop.

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
| `internal/battery/` | The Operator's battery gRPC client (#8); pins battery v0.1.0 |
| `config/` | Kustomize Manifests, from kubebuilder |
| `test/e2e/`, `test/utils/` | kubebuilder's e2e tests, against kind |
| `docs/adr/` | Architecture decision records |
| `docs/requirements/` | EARS requirements, `.duvet/` their config and snapshot |
| `hack/` | Boilerplate header, `duvet-coverage.sh` |

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
- Log messages follow the Kubernetes style: capitalised, no full stop, past
  tense, object type named, balanced key-value pairs.
- Reference: the [Kubebuilder Book](https://book.kubebuilder.io) and the
  [API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md).
