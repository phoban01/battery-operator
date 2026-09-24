# battery-operator

A Kubernetes operator for [battery](https://github.com/liquidmetal-dev/battery),
the warm-pool manager for [flintlock](https://github.com/liquidmetal-dev/flintlock)
MicroVMs.

battery keeps pools of MicroVMs warm and leases them to clients over gRPC.
This operator puts that behind two Kubernetes resources, so a pool is
declared and a MicroVM is claimed the way anything else in a cluster is:

- **`Pool`**: what battery keeps warm.
- **`MicroVMClaim`**: one client's hold on one warm MicroVM from a Pool.

A per-Host **Exec Agent** lets the holder of a claim, and only the holder,
run commands in its MicroVM. What a Node needs before it can be a Host, and
how the Exec Agent checks it, is in [Host prerequisites](docs/host-prerequisites.md).

The design was discussed in
[liquidmetal-dev/battery#46](https://github.com/liquidmetal-dev/battery/issues/46).
This project starts outside battery so it can be proven without disturbing
battery itself, with the aim of being adopted by liquidmetal-dev if it works.

**Status:** design. The project is scaffolded with kubebuilder, with no
APIs or controllers yet. Start with the
[architecture decision records](docs/adr/), then the
[requirements](docs/requirements/), which every change is traced to.

## Development

Everything runs in a [devbox](https://www.jetify.com/devbox) shell, which
pins Go, kubebuilder and the other tools this project uses:

```sh
devbox shell            # or: devbox run <script>
```

Inside it:

- `GOPATH` is `.gopath/` in the checkout, with its `bin` on `PATH`, and
  `GOTOOLCHAIN=local`, so the pinned Go is the one that runs.
- The Makefile pins controller-gen, setup-envtest, golangci-lint and
  kustomize, as kubebuilder scaffolds it, and installs them into `bin/` on
  first use.
- duvet, which traces requirements to code, is built once per machine into
  `~/.cache/battery-operator/cargo` (or `$BATTERY_OPERATOR_CARGO_HOME`) on the
  first shell entry, and shared by every checkout and worktree. The first
  build takes a few minutes.
- `devbox run build`, `test`, `lint` and `duvet` run the make targets, and
  `devbox run coverage-gate CL-001 CL-002` runs the requirements gate for
  those IDs.
- The Dagger CLI is downloaded into `.devbox/bin` on shell entry. CI is the
  Dagger module in `.dagger/`, and its jobs run the same functions you can
  run locally from the repository root (the engine needs Docker):

  ```sh
  dagger call lint
  dagger call test
  dagger call build export --path=bin/manager
  dagger call check-generated    # fails if make generate manifests changes the tree
  dagger call requirements --owns="CL-001 CL-002"  # duvet report and coverage gate
  dagger call requirements --owns=none             # the report only
  dagger call duvet-report export --path=.duvet/reports
  ```

- The images are defined in the same module, and nowhere else
  ([ADR 0005](docs/adr/0005-images-built-by-dagger.md)). CI publishes them
  on every push to `main` and every `v*` tag, to
  `ghcr.io/phoban01/battery-operator` (the Operator) and
  `ghcr.io/phoban01/battery-operator/exec-agent` (the Exec Agent), for
  linux/amd64 and linux/arm64, tagged with the commit SHA and `latest` or
  the version:

  ```sh
  dagger call images export --path=dist/images  # both images, both platforms, as OCI tarballs
  make docker-build IMG=battery-operator:dev    # into the local Docker, e.g. for kind
  make docker-push IMAGE_REPO=ghcr.io/you/battery-operator IMAGE_TAG=dev
  ```

## License

Apache License 2.0, as battery and flintlock are. See [LICENSE](LICENSE).
