# The Exec Agent

The Exec Agent runs on every Host. It lets a claim's Holder, and only the
Holder, run commands in the claim's MicroVM by relaying to `MicroVMExec` on
the local `flintlockd`, and it checks the Host and reports the result on the
Host's Node (ADR 0001, decisions 4 and 10). It comes from flintlock-runner,
whose `12-cluster-fleet.md` specified it as KF-170 to KF-182; the numbers
there map to the ones here in the order they appear.

## Serving {#serving}

- **EA-001** The Exec Agent SHALL reach `flintlockd` only through a local
  endpoint, a unix socket or a loopback address, and SHALL refuse to start
  with any other `flintlockd` endpoint.
- **EA-002** The Exec Agent SHALL serve its exec API over TLS on the Host's
  internal address, with a serving certificate that names that address.
- **EA-003** The Exec Agent's exec API SHALL be flintlock's
  `microvmexec.services.api.v1alpha1` service, together with the
  `ServerInfo` and `GetMicroVM` calls of its `microvm.services.api.v1alpha1`
  service.

EA-003 means a consumer that already speaks to `flintlockd`'s exec API
speaks to the Exec Agent unchanged, with a bearer token added.

## Authorization {#authorization}

- **EA-010** The Exec Agent SHALL authenticate every request with a
  TokenReview of the bearer token it carries, requesting the Exec Agent's
  audience, and SHALL refuse a request that does not authenticate.
- **EA-011** The Exec Agent SHALL run a command in a MicroVM only when the
  request's token belongs to the Holder of a claim whose namespace and
  `spec.serviceAccountName` it names.
- **EA-012** The Exec Agent SHALL run a command in a MicroVM only when the
  request's token is bound to that claim's Secret `<claim name>-exec`, by
  both the Secret's name and its uid.
- **EA-013** The Exec Agent SHALL run a command in a MicroVM only when that
  claim is Bound, its Lease has not expired, and it names that MicroVM's uid
  and this Host.
- **EA-014** If the TokenReview or the claim lookup of a request cannot be
  completed, then the Exec Agent SHALL refuse the request.

EA-010 to EA-013 are the four checks of the proposal on battery#46. Checking
the Holder (EA-011) and the binding (EA-012) are both needed: anyone who may
request tokens for some ServiceAccount in the namespace could bind a token
to the claim's Secret, and a Holder's ordinary token is not bound to it. A
token bound to one claim's Secret therefore opens that claim's MicroVM and
no other, and dies with the claim: deleting the claim garbage-collects the
Secret, and a token bound to a deleted object no longer passes a
TokenReview. The garbage collector takes a few seconds; EA-013 fails the
moment the claim is released, so the token opens nothing in that gap.

Whether TokenReview reports the bound object, or the Exec Agent has to read
it from the token's own claims once the API server has vouched for the
signature, is to be confirmed when EA-012 is built.

Consumers create the claim's Secret and request its token themselves
(ADR 0001, decision 8); the Exec Agent only checks.

## Relay {#relay}

- **EA-020** The Exec Agent SHALL relay a request's streams to
  `MicroVMExec.ExecCommand` on the local `flintlockd`, and SHALL end every
  response with an exit status frame, sent only after `flintlockd` has
  reported the command's exit.
- **EA-021** If `flintlockd` has not answered for the requested MicroVM and
  accepted the request's exec stream within the configured deadline, then
  the Exec Agent SHALL end the response as a stream failure.

A relay cut short, because the Exec Agent restarted, must never look like a
command that succeeded, which is why the exit status is a frame of its own
and its absence is a failure. Opening the stream counts only once
`flintlockd` has answered a `GetMicroVM` for the MicroVM: a gRPC stream and
its first message are accepted on the client's side alone, even by a server
that answers nothing.

## Host checks {#host-checks}

- **EA-030** The Exec Agent SHALL report its Host not ready while the local
  `flintlockd` does not answer `ServerInfo` with the exec service enabled.
- **EA-031** The Exec Agent SHALL report its Host not ready while `/dev/kvm`
  cannot be opened.
- **EA-032** The Exec Agent SHALL report its Host not ready while
  containerd's thin pool, as the configuration names it, is not present.
- **EA-033** The Exec Agent SHALL report its Host not ready while any file
  in the not ready reason directory names a reason, and SHALL report that
  reason.
- **EA-034** The Exec Agent SHALL publish its Node report on its Host's Node
  as the annotation `battery.liquidmetal-x.dev/exec-agent-ready` set to
  `true` or `false`, with the reason and message of the last check in
  `battery.liquidmetal-x.dev/exec-agent-reason` and
  `battery.liquidmetal-x.dev/exec-agent-message`, and its own address in
  `battery.liquidmetal-x.dev/exec-agent-address`.
- **EA-035** The Exec Agent SHALL publish in its Node report the address
  at which battery reaches the Host's `flintlockd`.

EA-030 to EA-032 are the Host prerequisites of the glossary, checked on the
Host itself, since this project ships no Host Image (decision 9). EA-033 is
flintlock-runner's not ready reason contract, which lets a Host Image report
reasons of its own, such as a Host Service that has not started. The Node
report is the contract the Inventory Controller (IN-001) and the Claim
Controller (CL-005) read. EA-035 depends on the open question in
`04-inventory.md#flintlockd-reachability`.

## Drain {#drain}

- **EA-040** While claims are Bound on its Host, the Exec Agent SHALL hold an
  eviction-based drain of the Host's Node open, and SHALL let the drain
  complete when none remain or when the configured drain timeout elapses.

## Identity {#identity}

- **EA-050** When the Exec Agent starts, the Exec Agent SHALL confirm that its identity
  names its own Host, and SHALL refuse to start otherwise.
- **EA-051** The Manifests SHALL include a ValidatingAdmissionPolicy that
  lets an Exec Agent's identity change only the annotations of its own
  Host's Node under the prefix `battery.liquidmetal-x.dev/`, and nothing
  else of any Node.

EA-051 is what makes a Node report trustworthy: an agent can speak only for
its own Host, so a compromised Host cannot mark another Host ready or point
claims at itself.
