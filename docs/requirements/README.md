# Writing requirements for battery-operator

This directory holds the normative specification for battery-operator. Every
requirement is written in EARS (Easy Approach to Requirements Syntax) and is
traced to code and tests with [duvet](https://github.com/awslabs/duvet).
Because duvet extracts requirements mechanically, the authoring rules below
are not stylistic preferences: breaking them silently drops requirements
from the coverage report.

The design these requirements specify is recorded in
[ADR 0001](../adr/0001-standalone-operator-over-battery-grpc.md).

## Documents

| File | Prefix | Scope |
|------|--------|-------|
| `00-glossary.md` | – | Defined terms used by every other document (non-normative) |
| `01-resources.md` | `RS` | The `Pool` and `MicroVMClaim` resources: group, fields, validation |
| `02-claims.md` | `CL` | The Claim Controller: binding, renewal, expiry, release and recovery of claims |
| `03-pools.md` | `PO` | The Pool Controller: declaring Pools to battery, placement and status |
| `04-inventory.md` | `IN` | The Inventory Controller: which Nodes are Hosts, and telling battery |
| `05-exec-agent.md` | `EA` | The Exec Agent: exec relay, authorization, Host checks and the Node report |
| `06-deployment.md` | `DP` | The Operator process, battery as its sidecar, the battery connection and the Manifests |
| `07-client.md` | `CC` | The Client Library that consumers use to claim and use a MicroVM |
| `08-test-doubles.md` | `TD` | The fake battery and fake `flintlockd` that stand in for KVM and battery during development |
| `09-certificates.md` | `CT` | The Operator as approver and signer of the Hosts' certificates, and the SPIFFE identities they carry |

## EARS patterns

Use exactly one of these shapes per requirement. The subject is always one of
the system names defined in the glossary: the Operator, the CRDs, the Claim
Controller, the Pool Controller, the Inventory Controller, the Exec Agent, the
Manifests, the Client Library, the fake battery, the fake `flintlockd`, the
unit tests, the e2e suite.

| Pattern | Shape |
|---------|-------|
| Ubiquitous | The `<system>` SHALL `<response>`. |
| Event-driven | When `<trigger>`, the `<system>` SHALL `<response>`. |
| State-driven | While `<state>`, the `<system>` SHALL `<response>`. |
| Unwanted behaviour | If `<condition>`, then the `<system>` SHALL `<response>`. |
| Optional feature | Where `<feature is configured>`, the `<system>` SHALL `<response>`. |
| Complex | Any combination of the above clauses before a single SHALL. |

## Rules imposed by duvet

1. **Keywords are uppercase.** duvet only recognises `MUST`, `MUST NOT`,
   `SHALL`, `SHALL NOT`, `SHOULD`, `SHOULD NOT`, `MAY`, `REQUIRED`,
   `RECOMMENDED`, `OPTIONAL`. `SHALL` maps to level MUST. Lowercase `shall`
   is not extracted. Never use these words in uppercase in explanatory prose.
2. **One sentence is one requirement.** duvet cuts requirements at a full
   stop followed by whitespace. Write each requirement as a single sentence,
   and never use `e.g. `, `i.e. ` or `etc. ` inside one. Dots not followed by
   whitespace (`config.toml`, `v0.1.0`) are safe.
3. **One requirement per bullet.** Each requirement is a list item that
   begins with its bold identifier, for example `- **CL-004** When ...`. A
   bullet without a keyword is not a requirement, so a lead-in sentence
   followed by sub-bullets does not make one requirement per sub-bullet;
   write the alternatives inline instead.
4. **Sections have stable identifiers.** Every heading carries an explicit
   pandoc-style attribute such as `## Binding {#binding}`. Annotations target
   `docs/requirements/<file>.md#<id>`, so renaming a heading without keeping
   its id breaks every citation under it.
5. **Identifiers are never reused once cited.** Until the first code citation
   lands, a document may be renumbered wholesale. After that, a retired
   requirement is marked `(withdrawn)` and keeps its number, and the next
   requirement takes the next free number in its prefix.
6. **Explanatory prose is separate.** Rationale, examples and background go
   in paragraphs outside the bullet lists and contain no uppercase keywords.

## Annotating code

Implementation:

```go
//= docs/requirements/02-claims.md#release
//# When a claim that has no lease is deleted, the Claim Controller SHALL
//# remove its finalizer without calling battery.

// releasePending lets a claim that never bound go.
func (r *ClaimReconciler) releasePending(ctx context.Context, c *v1alpha1.MicroVMClaim) error {
```

Test. Always give `type=test` explicitly, because Go tests live next to the
code they test and are matched by the same source pattern:

```go
//= docs/requirements/02-claims.md#release
//= type=test
//# When a claim that has no lease is deleted, the Claim Controller SHALL
//# remove its finalizer without calling battery.

// TestDeletingAPendingClaimDoesNotCallBattery covers CL-021.
func TestDeletingAPendingClaimDoesNotCallBattery(t *testing.T) {
```

**Keep a blank line between the citation and the declaration's doc comment.**
A citation directly above a `func` or type becomes its doc comment, and
`gofmt` (which `make test` runs) rewrites `//=` and `//#` there to `// =` and
`// #`. duvet then ignores them without an error, and the coverage gate
reports the citation as missing. Citations on the first lines inside a
function body are safe too.

YAML under `config/` cites requirements the same way, with `#=` and `#/` in
place of `//=` and `//#`.

Other annotation types: `type=exception` with a `reason=` line for a
requirement deliberately not met, `type=implication` for one satisfied by
construction, and `type=todo` with `tracking-issue=` for planned work. An
implication does not count as a citation for the coverage gate. The quoted
text after `//#` has to be a contiguous substring of the section; whitespace
differences are tolerated.

## Model citations

The Quint models in [specs/quint/](../../specs/quint/README.md) cite the
requirements they model. A model citation is its own annotation type:
it shows a requirement as *modelled*, and it never counts as an
implementation citation or a test citation. Go tests stay required
whatever a model checks.

A model citation uses `//@=` and `//@#` in place of `//=` and `//#`, and
takes no `type=` line:

```quint
//@= docs/requirements/02-claims.md#release
//@# When a claim that has a lease id is deleted, the Claim
//@# Controller SHALL call battery's `ReleaseVM` for the Lease, and SHALL remove
//@# the finalizer only once battery has released the Lease or reported it
//@# unknown.
def canRelease(c: ClaimName): bool = and {
```

Put it on the step or invariant that models the requirement. The quote
follows the same rules as any other citation.

duvet 0.4.3 has a fixed set of annotation types (`citation`, `test`,
`implication`, `exception`, `todo`) and no custom ones, so the model type
is built from a second duvet configuration:

- [.duvet/models.toml](../../.duvet/models.toml) scans only
  `specs/**/*.qnt`, for the `//@=` prefix, and writes its own report,
  `.duvet/reports/models.json` and `models.html`.
- `.duvet/config.toml` never scans the models, and the coverage gate reads
  only its report. So a model citation can't satisfy the gate, and it
  doesn't show in the main report or the snapshot.
- [hack/duvet-models.sh](../../hack/duvet-models.sh) fails on a model
  citation that has a `type=` line. It also prints each requirement as
  implemented, tested and modelled (`make duvet-models`), and keeps the
  list below in step with the models.

`make duvet` runs both configurations, and fails if the list below is out
of date. `make duvet-models-write` rewrites it. `type=test` and
`type=implication` are never used for models.

### Modelled requirements

<!-- BEGIN modelled: written by hack/duvet-models.sh --write; do not edit -->
| ID | Model |
|----|-------|
| CL-001 | `specs/quint/claims.qnt` |
| CL-002 | `specs/quint/claims.qnt` |
| CL-003 | `specs/quint/claims.qnt` |
| CL-010 | `specs/quint/claims.qnt` |
| CL-011 | `specs/quint/claims.qnt` |
| CL-012 | `specs/quint/claims.qnt` |
| CL-013 | `specs/quint/claims.qnt` |
| CL-014 | `specs/quint/claims.qnt` |
| CL-020 | `specs/quint/claims.qnt` |
| CL-021 | `specs/quint/claims.qnt` |
| CL-030 | `specs/quint/claims.qnt` |
| CL-031 | `specs/quint/claims.qnt` |
<!-- END modelled -->

## The coverage gate

Every PR says which requirements it implements, on a line of its own in the
PR body:

```
Owns: CL-001..004, RS-020
```

CI then fails unless each owned ID has both an implementation citation and a
test citation. A PR that implements no requirement, such as tooling or
documentation, says `Owns: none`. Ranges expand to every ID that exists
between the two numbers.

## Running duvet

```sh
cargo install duvet --locked       # once
make duvet                         # writes .duvet/reports/report.html and refreshes .duvet/snapshot.txt
make coverage-gate IDS="CL-001"    # the gate CI runs, for chosen IDs
make duvet-ci                      # fails if the snapshot differs from the committed one
```

duvet reads only the files git knows about, so `git add` a new file before
checking its citations locally.

Extracted requirements land in `.duvet/requirements/` and are regenerated on
every run; they are never edited by hand. Renaming a section leaves a stale
file behind that makes `duvet report` fail with a missing-section error,
which is why the Makefile clears that directory before each run.

`.duvet/snapshot.txt` is committed only at milestones. A PR that changes it
has to say `Snapshot: milestone` on a line of its own in its body; otherwise
restore it with `git checkout origin/main -- .duvet/snapshot.txt`.
