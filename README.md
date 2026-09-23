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
run commands in its MicroVM.

The design was discussed in
[liquidmetal-dev/battery#46](https://github.com/liquidmetal-dev/battery/issues/46).
This project starts outside battery so it can be proven without disturbing
battery itself, with the aim of being adopted by liquidmetal-dev if it works.

**Status:** design. Nothing is built yet. Start with the
[architecture decision records](docs/adr/), then the
[requirements](docs/requirements/), which every change is traced to.

## License

Apache License 2.0, as battery and flintlock are. See [LICENSE](LICENSE).
