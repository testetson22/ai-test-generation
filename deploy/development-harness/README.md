# SSC Development Harness

SSC supplies temporary development/execution-host material, not the source of
ATG test contracts or architecture. The ATG implementation replaces the SSC
demo's `agentic-test-generation` pipeline and `ai-generate-tests` task. The
availability of an SSC playground does not select Tekton as the production
provider or make that environment an integration prerequisite.

## Current Playground

As of 2026-11-17, `ssc-demo-fp` is available locally for experiments. It is
temporary; its location and availability are expected to change. Keep machine
paths and access details in ignored local context, never in build dependencies,
canonical contracts, fixtures, or committed credentials.

The inspected `ssc-demo-fp` checkout was clean at
`79cd122f776b6f85e14f5dd52ffc2318cd44e348`. The earlier SSC baseline was
`80615e380cac6fcfba8c4b92b882686d855e4fef`. These are source-inspection snapshots,
not evidence of live deployment health or proof that either snapshot is an
approved execution profile. Reinspect and record the new source/configuration
identity when using a replacement playground; do not depend on these snapshots
at service startup.

The fork adds `cluster/` operation helpers and a unit-generation CPU-request
adjustment; the inspected core task/pipeline/image flow retains the SSC design.
Its action helper launches the existing demo-compatible generator, not a
conformant ATG controller/worker flow. Historical cluster notes are observations,
not current readiness evidence.

## First Supported Application Profile

FLPATH-5048 will first exercise profile-driven execution against SSC's application,
`lzspangler/spring-boot-example-application` (Java 17/Spring Boot), independently
of where `ssc-demo-fp` is checked out or deployed. This is a first-class
development/integration target, not a disposable SSC-specific controller path.
The upstream Java 21/Quarkus target becomes a separate portability/coverage
milestone; retain both profiles once implemented.

SSC imports the application into GitLab and commonly selects moving `main`.
Resolve and pin the actual application commit and dependency states; the
playground infrastructure SHA is not the application SHA. VPE's checked-in
analysis profile names upstream commit
`01a18a1b40e151ffcc1d7bef86bf335f87999750`, but its equality with the selected
imported application still needs verification. Preparation metadata is not an
approved ATG execution profile.

Implement and authorize a versioned execution profile with pinned tool/image and
dependency identities, commands, focused test selection, application setup/reset,
reports, execution limits, credential/network policy and artifact requirements.
Keep Spring health endpoints and Maven report paths behind profile/framework
adapters, not controller lifecycle or provider logic. No such profile is created
or approved merely by this document.

Start with focused baseline tests and independent same-subject controls, then an
explicit immutable candidate and characterized comparison. Same-subject controls
prove execution relationships, not remediation. SSC's optional synthetic artifact
seed may publish identical bytes under a "fixed" version label; coordinates alone
cannot establish a behavioral fix. Quarkus coverage must later reuse the same
controller lifecycle/retry/recovery logic, though its profile/adapters may differ.

## Reuse From SSC

The replacement may reuse the harness infrastructure:

- OpenShift Pipelines/Tekton and its workspace/PVC provisioning.
- Git repository access and Maven settings/artifact repository configuration.
- Approved AI endpoint configuration and credential references.
- Worker image recipes and separate SCM publication credentials where approved;
  current recipes are not proof of adequate untrusted-code isolation.

These are deployment concerns. They do not define ATG request, run-state,
review, evidence, or outcome contracts.

## Inspected Reuse Boundaries

Paths in this table are relative to the inspected SSC/`ssc-demo-fp` checkout.
Use them as implementation references; do not import the checkout at runtime.

| Element | Useful implementation material | Required ATG change or limitation |
|---|---|---|
| `pipelines/tasks/git-clone.yaml` | Checkout/ref handling, resolved commit result, workspace and credential bindings | Require and verify immutable subject identities; support for a SHA is not enforcement of one |
| `pipelines/tasks/maven.yaml` and `pipelines/pipelines/agentic-test-generation*.yaml` | Explicit Maven execution, direct Failsafe goals, pipeline task ordering | Record actual focused test IDs/counts/reports, not exit status alone; require approved toolchain/command profiles |
| `pipelines/tasks/ai-generate-tests*.yaml` | Agent invocation, scratch-copy workflow, bounded generation attempts, test copy-back | Separate agent task execution from repository workloads and their credentials; do not copy a combined credential-bearing shell task into production |
| `pipelines/tasks/deploy-app-under-test.yaml`, `reset-app-under-test.yaml`, and `cleanup-app-under-test.yaml` | Run-derived application resources, service/readiness outputs, reset and teardown primitives | Keep Spring/actuator/in-memory-state behavior in profile/adapters; verify reset and cleanup; best-effort teardown is not a success guarantee |
| `pipelines/images/` | Build/agent image recipes and writable-home patterns | Pin and approve images/toolchains for the first Spring profile; Java 17 execution is not evidence of Java 21/Quarkus coverage |
| `cluster/ssc-demo-actions.sh` in the temporary fork | Local inspection and manual PipelineRun submission helpers | Playground operations only; not a durable task/attempt ledger, retry scheduler, or provider reconciliation contract |

The observed flow uses one checkout, copies it into agent scratch, copies accepted
changes back, and reruns Maven in the shared checkout. It does not materialize the
same approved tests independently against immutable base and candidate subjects.
Model credentials/Maven settings remain accessible in the generation step that
runs repository code. Test verification primarily trusts command exit status,
and there is no ATG report/log durability acknowledgement before cleanup.

Therefore the following still need implementation: controller-managed task and
attempt records; stable launch/reconciliation identities; separated credentials
and writable workspaces; enforced runtime limits; exact per-subject test evidence;
durable artifact collection; and observable, retryable cancellation/cleanup.
See [controller task requirements](../../docs/architecture/run-state.md#controller-task-ledger).

## Replacement Flow

The replacement entrypoint must implement the ATG lifecycle rather than wrap
the old task result fields:

```text
request/admission
  -> Analyze -> durable Plan review
  -> Code -> durable generated-change review
  -> independent base/candidate Execute
  -> quality/differential gates
  -> provenance/evidence publication
```

The existing SSC slash-command trigger and pipeline name can be retained only
as a deployment migration mechanism. The resulting run must emit the ATG v1
contracts, not `TESTS_ADDED`, `ATTEMPTS`, or `PR_URL` as its public result
model.

## Migration Boundary

If Tekton is selected, its adapter must accept the controller-authorized ATG task
intent and return attributable provider observations and durable artifact
references. The control component retains lifecycle/approval/retry authority;
the adapter/provider performs delegated job operations. SSC Maven/artifact and
SCM infrastructure may be reused only through explicit approved interfaces.
Wrapping the old generator task and projecting its result fields is not enough.

The FLPATH-5051 spike contributes responsibility/provider decision evidence, not
production code. Its incomplete status need not block implementation: record a
superseding decision and limitations if necessary, without skipping the required
isolation, recovery, and evidence behavior.

## Provisional Tekton Boundary

Tekton remains a candidate, but enough of the boundary is known to develop it
without treating a complete cluster as a prerequisite for controller work:

- The controller supplies an authorized task/attempt intent, immutable
  specification digest, stable submission token, and approved profile reference.
  The adapter maps these to pinned TaskRun/PipelineRun resources, workspace and
  service-account bindings, timeouts, and results; Tekton API objects stay outside
  the canonical lifecycle model.
- Test duplicate submission and create-response loss using deterministic resource
  identity and persisted attempt mapping. Reconcile existing resources only after
  checking task/attempt/specification identity and recording the resource UID;
  a matching label or name alone is not sufficient authority to adopt a job.
- Normalize provider observations without converting a successful PipelineRun
  into proof that the expected tests executed. Missing/pruned resources do not
  prove a job never ran. Cancellation requested is distinct from confirmed
  workload termination, and cleanup may include resources beyond the run object.
- Model required artifact durability separately from Tekton results and logs.
  Provider pruning/TTL/finally behavior must not discard required evidence before
  acknowledgement. Record platform-native retries within controller policy.
- Begin with a stateful provider simulator for controller tests, then pinned
  resource-rendering/API fixtures for the adapter. Exercise create conflicts,
  authorization rejection, pending/unschedulable work, interrupted status/watch,
  stale/duplicate observations, timeout, cancellation, and cleanup failure.

These tests do not establish live RBAC, admission behavior, workspace/credential
isolation, egress/resource enforcement, actual cancellation/teardown, or artifact
survival after pruning. Those require integration on a recorded platform/profile.
No TaskRun/PipelineRun template or Tekton adapter is implemented by this document.
The same provider conformance contract must remain usable by a non-Tekton adapter.

## Local Component

Until the production components are implemented, the repository provides a
Python/SQLite reference component. After installing the root package and its
dependencies, it can be run locally:

```bash
PYTHONPATH=src python3 -m atg.component.run_state_server --database /tmp/atg.sqlite3
```

It exposes `POST /runs`, `POST /runs/{run_id}/events`, `GET /runs/{run_id}`,
and `GET /runs/{run_id}/events`. Component tests use this process boundary to
verify persistence and replay behavior; it is not the production deployment.
