# Run-State Contract

The ATG run state is an append-only event projection. A state update is valid
only when the event's `expected_revision` equals the stored revision. The event
journal and state snapshot must be committed atomically.

## State Flow

```text
requested -> admitted -> analyzing -> analysis_review
analysis_review --approved--> planning -> plan_review
plan_review --approved--> coding -> code_review
code_review --approved--> executing -> execution_review
execution_review --approved--> quality_review -> quality_review
quality_review --quality--> completed | blocked | inconclusive | stated_gap

feedback may return a run to Analyze, Plan, Code, or Execute without silently
retaining the superseded active checkpoint.
```

Review checkpoints may be explicitly superseded or expired. Expiry returns the
run to the stage that created the checkpoint. `blocked` and `inconclusive` runs
may transition through an explicit `retry_requested` event; cancellation and
stated gaps are terminal for that run.

## Invariants

- Event IDs are unique per journal and are bound to a payload digest.
- Replaying an identical event is a successful no-op.
- Reusing an event ID with a different payload is rejected.
- An event based on an old revision is rejected.
- A checkpoint decision must identify the active checkpoint, decision ID,
  authenticated reviewer identity, reviewer role, and authorization reference.
- A replacement checkpoint must explicitly name the checkpoint it supersedes.
- Generated candidates require a patch digest before they can be verified.
- Execution evidence requires subject, worker, tool, command, command digest,
  environment, and timing identities.
- Differential evidence references both executions, quality evidence, complete
  provenance, and a signed canonical payload.

## Persistence Boundary

`src/atg/ports/run_state.py` is the service persistence port.
`SQLiteRunStateStore` is the initial development adapter used for component
tests and local execution. A later Go, Java/Quarkus, or other selected runtime
must preserve the same event, revision, replay, and checkpoint semantics.

## Controller Task Ledger

Implementation direction agreed on 2026-11-17: the control component authorizes
task-level retries and manages recovery using durable phase/task observations.
This section specifies missing implementation requirements, not fields already
available in the v1 run-state or execution schemas. The reference's run-level
`retry_requested` event and synchronous generation-repair loop are not a general
task/attempt scheduler.

Use a small versioned envelope around stage-specific payloads rather than a new
service per task or an unconstrained workflow engine:

| Record | Required information |
|---|---|
| Task | Stable task/run identity, task kind/phase, dependencies or parent/pair relationship, immutable specification/input/profile digests, approved artifact/decision references, current revision, and cancellation/supersession state |
| Attempt | Unique attempt identity/ordinal, bound task specification, retry scope/predecessor, stable submission token, provider/job identity, worker identity, and observed state including an unresolved/unknown submission outcome |
| Policy and retry decision | Controller-authorized action, policy identity, retry eligibility/reason, scoped and cumulative budgets, resource limits, deadlines, next eligible time, and retry decision identity |
| Claim and progress | Claim holder and fencing generation, lease expiry, authoritative observation times, provider progress or heartbeat, start/end times, failure category, and partial-result references |
| Completion and teardown | Result/artifact manifest references, collection completeness and durability acknowledgement, cancellation confirmation, stale-result disposition, cleanup eligibility, and observed cleanup status |

These are information requirements; finalize schema names, states, required
fields, and compatibility rules before implementing a consumer. An attempt
redelivery retains its submission token; a controller-authorized new attempt gets
a distinct identity. Changing approved tests, subjects, or other material inputs
is a new specification/revision requiring the applicable validation and review,
not a transparent retry of the old specification.

### Controller Responsibilities

- Persist launch intent and the complete recoverable specification before
  dispatch. Claim due work atomically and fence stale claimants; run revision
  checks alone cannot prevent two external jobs being launched.
- Classify input/policy rejection, model-output validation, transport failure,
  worker infrastructure failure, timeout, behavioral test failure, and evidence
  failure distinctly. A runner's retry suggestion is an observation, not
  authorization. A failed assertion is not automatically retryable.
- Persist retry eligibility, attempt budgets, backoff/next-due time, and deadlines
  so restart does not reset limits. Include provider-native retries explicitly;
  do not let controller, agent, and platform retry loops multiply unnoticed.
- Permit bounded safe transport retries within declared policy. Distinguish those
  from model repair and a new workload attempt, and account for consumed task/run
  budgets. The controller need not micromanage every tool call.
- After an interrupted submission, reconcile using the stable token before
  relaunch. If the provider cannot resolve whether work exists, retain uncertainty
  rather than inventing failure/success or blindly creating another job.
- Treat base and candidate as independent child tasks/attempts. Preserve completed
  work when the other side fails; join only attributable terminal observations.
- Record cancellation, propagate it to active work, and reconcile termination.
  Detect expired claims/lost workers through provider observation or heartbeats;
  lease expiry alone does not prove that a process stopped. Late or superseded
  results may be retained as evidence but cannot advance current lifecycle state.
- Recheck authorization, capabilities/profile compatibility, readiness freshness,
  and capacity before dispatch/retry. A stored authorization reference is not
  itself authentication or permission enforcement.
- Validate accepted results against the current task/attempt, exact inputs,
  approved artifact chain, and expected test/evidence identities. Missing or
  incomplete observations must not become passing results.
- Require durable artifact/result acknowledgement before normal workspace/job
  cleanup. Reconcile failed cleanup and retention separately; storage failure
  requires bounded recovery/teardown policy, not an indefinitely running worker.

Provider operations must support submission, observation/reconciliation,
cancellation, and cleanup with declared idempotency and retry behavior. Define
authenticated result delivery or provider polling; do not expose an untrusted
generic event route as completion authority. A controller can have coordinated
replicas while retaining one logical authority for attempts and run transitions.

### Validation Boundary

Existing journal replay proves consistency of recorded state, not the actual
state of external jobs or artifacts. Add conformance and native component tests
for crash windows, concurrent claims, duplicate delivery, partial pair completion,
budget/deadline persistence, cancellation races, stale results, storage failure,
and cleanup ordering. Test exact launch counts and persisted identities, not just
HTTP success. Real provider execution and isolation require separate integration
evidence.
