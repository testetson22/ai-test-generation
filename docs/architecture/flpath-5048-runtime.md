# FLPATH-5048 Initial Runtime Boundary

Status: Initial implementation decision; not a production deployment claim.
Date: 2026-11-17

## Decision

- Implement the first production **control component in Go**. Keep it in this
  repository under `services/control/` with its own module, native tests,
  packaging, and CI. The runtime owns task/attempt lifecycle, dispatch
  authorization, retry scheduling, reconciliation, cancellation, and evidence
  acceptance. It does not execute repository build/test commands itself.
- Use **Go 1.26.2 or newer** as the minimum compiler/runtime baseline. This
  matches the patched-toolchain floor already required by adjacent Lightpath
  source-acquisition code. CI and the module must pin a patched exact release;
  the current local Go 1.25.0 installation is insufficient for the component.
- Use **PostgreSQL** for durable controller metadata, transactional work claims,
  fencing, task/attempt state, and outbox/reconciliation records. Production
  migrations and supported PostgreSQL versions belong to the component module.
  The Python reference SQLite schema is not a production storage interface.
- Define an **S3-compatible object-storage boundary** for immutable,
  content-addressed execution artifacts and explicit durable-write acknowledgement.
  The deployed implementation/bucket policy is a deployment decision. Local
  filesystem storage may implement the same port for development tests only;
  it is not a production fallback.
- Do not add an agent runner to the initial execution service. The first vertical
  slice consumes already-approved generation artifacts. If Pydantic AI is later
  selected for Analyze/Plan/Code, its Python runner remains a separate task
  adapter and cannot own controller lifecycle or worker credentials.
- Run repository builds/tests in separate profile-driven workers. First onboard
  SSC's Java 17/Spring application; later add Java 21/Quarkus as a second profile
  using unchanged controller state/retry/recovery semantics.

## Rationale

The control workload is a long-lived, I/O-oriented coordinator with concurrent
claims, durable retry budgets, and external side effects requiring reconciliation.
Go provides one native production build for the controller without making the
Python reference a hidden runtime dependency. PostgreSQL supplies transactional
claims and coordination across replicas. Immutable object storage separates
artifact retention from database state and worker lifetime. The framework/tool
chain belongs to the selected workload profile, not the controller process.

## Explicit Non-Goals

- This does not select Tekton/OpenShift, an agent framework/model, a public
  deployment topology, or the production object's bucket/vendor.
- This does not require a separate service/repository for the agent runner,
  worker, provider adapter, database, or artifact storage.
- This does not claim that the first SSC profile or second Quarkus profile is
  already implemented/approved, or that either target has a valid differential
  base/candidate pair.
- This does not make local filesystem or fake-provider composition a production
  fallback.

## Provisional Provider Candidate

No FLPATH-5051 responsibility decision is present in this implementation
checkout. To continue adapter work without treating SSC reuse as approval,
Tekton's `tekton.dev/v1` TaskRun/PipelineRun API is a provisional candidate only
for offline resource-rendering, identity checks, and observation fixtures in
`services/control/internal/provider/tekton`. This does not select a production
provider, authorize cluster access, or establish that an installed cluster accepts
these resources. The provider API port has no production Tekton API client or
service wiring.

The control component remains the sole authority for task/attempt creation,
retry, deadlines, cancellation intent, reconciliation, and evidence acceptance.
The candidate provider maps each authorized attempt to one deterministic run
resource and reports attributed observations; it cannot approve profiles or
declare tests successful. The workload image executes the approved profile.
Cluster operators remain responsible for API/RBAC configuration, admission,
isolation enforcement, and operational support; no team/on-call assignment is
made here. Non-Tekton providers remain possible without changing the lifecycle.

Promotion beyond offline fixtures requires a concrete FLPATH-5051 decision (or a
superseding approval), the supported cluster/CRD version, task/pipeline and worker
image identities, namespace/service-account policy, artifact retention behavior,
and observed cancellation/termination and teardown guarantees. These requirements
must be verified before live adapter composition or workload execution.

## Evidence And Replay Gates

The Python reference's generic event endpoint cannot authorize execution or
substitute caller-supplied artifact metadata for accepted execution results.
New execution start/completion events, execution/quality review checkpoints,
approvals of those reviews, and positive quality completion fail closed until a
trusted execution-ledger bridge is implemented. Historical journals retain their
original replay semantics and exact duplicate protection; replay compatibility
does not authorize new advancement through those checkpoints.

The provisional provider carries the persisted job UID into reconciliation and
checks it before cancellation or deletion. Resource absence alone is neither
termination nor cleanup confirmation: cleanup requires an independent API check
that the identified run and its owned workload resources are gone, even after
the run object disappears or the adapter restarts. These offline port guarantees
still require validation in any future concrete platform client.

## Reconsideration Conditions

Revisit a choice only with concrete evidence that it blocks a required supported
deployment or violates an operational/security requirement. Record migration and
compatibility impact before changing controller runtime, durable state authority,
or artifact identity semantics. Agent/provider choices remain independently
replaceable behind their contracts.
