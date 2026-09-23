# Architecture decision records

Each record states one decision, why it was made and what it costs. A record
is numbered, never renumbered, and never rewritten once accepted: a later
decision that changes it is a new record that says which one it supersedes.

| # | Decision | Status |
|---|----------|--------|
| [0001](0001-standalone-operator-over-battery-grpc.md) | A standalone operator in front of an unmodified battery | Accepted |
| [0002](0002-battery-reaches-flintlockd-over-mtls.md) | battery reaches flintlockd over mutual TLS | Accepted |
| [0003](0003-host-certificates-through-kubernetes-csrs.md) | Host certificates through Kubernetes certificate signing requests | Accepted |
| [0004](0004-exec-agent-serving-certificate.md) | The Exec Agent's serving certificate through a third signer | Accepted |
| [0005](0005-images-built-by-dagger.md) | The images are defined in the Dagger module, not a Dockerfile | Accepted |
| [0006](0006-unit-tests-and-kind-e2e.md) | Unit tests with fakes, and end-to-end tests on kind | Accepted |

A new record copies the headings of the last one: status, date, context,
decision, consequences, open questions. Status is one of Proposed, Accepted,
Superseded by NNNN, or Rejected.
