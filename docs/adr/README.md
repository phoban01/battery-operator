# Architecture decision records

Each record states one decision, why it was made and what it costs. A record
is numbered, never renumbered, and never rewritten once accepted: a later
decision that changes it is a new record that says which one it supersedes.

| # | Decision | Status |
|---|----------|--------|
| [0001](0001-standalone-operator-over-battery-grpc.md) | A standalone operator in front of an unmodified battery | Accepted |
| [0002](0002-battery-reaches-flintlockd-over-mtls.md) | battery reaches flintlockd over mutual TLS | Accepted |
| [0003](0003-host-certificates-from-cert-manager.md) | Host certificates from cert-manager, requested by the Exec Agent | Proposed |

A new record copies the headings of the last one: status, date, context,
decision, consequences, open questions. Status is one of Proposed, Accepted,
Superseded by NNNN, or Rejected.
