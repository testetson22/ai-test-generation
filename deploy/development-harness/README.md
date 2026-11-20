# Test-Generation Development Harness

The legacy demo supplies temporary development/execution-host material, not the
source of ATG test contracts or architecture. The ATG implementation replaces
the demo's existing test-generation pipeline and generation task. The available
playground does not select Tekton as the production provider or make that
environment an integration prerequisite.

The immediate goal is a development-only modularity proof against the existing
demo flow, not the VPE-driven ATG MVP. VPE/VEX context is still in active
development and is not a prerequisite for this demo. The current development
composition is Go control plus Tekton-backed workers, with Tekton confined to the
provider boundary and subject to FLPATH-5051 findings. Do not deploy it to the
live cluster until the new demo is aligned with the original flow and the
target environment is confirmed.

## Current Playground

As of 2026-11-17, a temporary demo fork was available locally for experiments. It is
temporary; its location and availability are expected to change. Keep machine
paths and access details in ignored local context, never in build dependencies,
canonical contracts, fixtures, or committed credentials.

The inspected temporary fork was clean at
`79cd122f776b6f85e14f5dd52ffc2318cd44e348`. The earlier demo baseline was
`80615e380cac6fcfba8c4b92b882686d855e4fef`. These are source-inspection snapshots,
not evidence of live deployment health or proof that either snapshot is an
approved execution profile. Reinspect and record the new source/configuration
identity when using a replacement playground; do not depend on these snapshots
at service startup.

The fork adds `cluster/` operation helpers and a unit-generation CPU-request
adjustment; the inspected core task/pipeline/image flow retains the demo design.
Its action helper launches the existing demo-compatible generator, not a
conformant ATG controller/worker flow. Historical cluster notes are observations,
not current readiness evidence.

## First Supported Application Profile

FLPATH-5048 will first exercise profile-driven execution against the sample
application, `lzspangler/spring-boot-example-application` (Java 17/Spring Boot),
independently of where the temporary demo fork is checked out or deployed. This
is a first-class development/integration target, not a demo-specific controller
path.
The upstream Java 21/Quarkus target becomes a separate portability/coverage
milestone; retain both profiles once implemented.

The demo imports the application into GitLab and commonly selects moving `main`.
Resolve and pin the actual application commit and dependency states; the
playground infrastructure SHA is not the application SHA. VPE's checked-in
analysis profile names upstream commit
`01a18a1b40e151ffcc1d7bef86bf335f87999750`; equality with the imported application
was confirmed on 2026-11-18. By 2026-11-20 the re-seeded GitLab main had advanced
to `83b6f931...`, so the initial profile remains pinned independently of that
branch. The [runtime evidence](../../docs/architecture/flpath-5048-runtime.md#bounded-baseline-validation-2026-11-20)
now includes actual execution of the initial pin. Preparation metadata and this
baseline probe are not an approved ATG execution profile.

Implement and authorize a versioned execution profile with pinned tool/image and
dependency identities, commands, focused test selection, application setup/reset,
reports, execution limits, credential/network policy and artifact requirements.
Keep Spring health endpoints and Maven report paths behind profile/framework
adapters, not controller lifecycle or provider logic. Each run binds the
profile ID, version, and SHA-256 digest from server configuration; callers cannot
select or alter the profile. The development API remains disabled until that
immutable identity is configured. No concrete profile is created or approved
merely by this document.

Start with focused baseline tests and independent same-subject controls, then an
explicit immutable candidate and characterized comparison. Same-subject controls
prove execution relationships, not remediation. The demo's optional synthetic artifact
seed may publish identical bytes under a "fixed" version label; coordinates alone
cannot establish a behavioral fix. Quarkus coverage must later reuse the same
controller lifecycle/retry/recovery logic, though its profile/adapters may differ.

## Reuse From Demo Environment

The replacement may reuse the harness infrastructure:

- OpenShift Pipelines/Tekton and its workspace/PVC provisioning.
- Git repository access and Maven settings/artifact repository configuration.
- Approved AI endpoint configuration and credential references.
- Worker image recipes and separate SCM publication credentials where approved;
  current recipes are not proof of adequate untrusted-code isolation.

These are deployment concerns. They do not define ATG request, run-state,
review, evidence, or outcome contracts.

## Inspected Reuse Boundaries

Paths in this table are relative to the inspected demo checkout.
Use them as implementation references; do not import the checkout at runtime.

| Element | Useful implementation material | Required ATG change or limitation |
|---|---|---|
| `pipelines/tasks/git-clone.yaml` | Checkout/ref handling, resolved commit result, workspace and credential bindings | Require and verify immutable subject identities; support for a SHA is not enforcement of one |
| `pipelines/tasks/maven.yaml` and `pipelines/pipelines/agentic-test-generation*.yaml` | Explicit Maven execution, direct Failsafe goals, pipeline task ordering | Record actual focused test IDs/counts/reports, not exit status alone; require approved toolchain/command profiles |
| `pipelines/tasks/ai-generate-tests*.yaml` | Agent invocation, scratch-copy workflow, bounded generation attempts, test copy-back | Separate agent task execution from repository workloads and their credentials; do not copy a combined credential-bearing shell task into production |
| `pipelines/tasks/deploy-app-under-test.yaml`, `reset-app-under-test.yaml`, and `cleanup-app-under-test.yaml` | Run-derived application resources, service/readiness outputs, reset and teardown primitives | Keep Spring/actuator/in-memory-state behavior in profile/adapters; verify reset and cleanup; best-effort teardown is not a success guarantee |
| `pipelines/images/` | Build/agent image recipes and writable-home patterns | Pin and approve images/toolchains for the first Spring profile; Java 17 execution is not evidence of Java 21/Quarkus coverage |
| Temporary fork's `cluster/` helpers | Local inspection and manual PipelineRun submission helpers | Playground operations only; not a durable task/attempt ledger, retry scheduler, or provider reconciliation contract |

The observed flow uses one checkout, copies it into agent scratch, copies accepted
changes back, and reruns Maven in the shared checkout. It does not materialize the
same approved tests independently against immutable base and candidate subjects.
Model credentials/Maven settings remain accessible in the generation step that
runs repository code. Test verification primarily trusts command exit status,
and there is no ATG report/log durability acknowledgement before cleanup. These
are known limits of the compatibility baseline. Development-only credentials are
permitted for the initial proof; credential isolation and production hardening
are not prerequisites for it.

## Current Modularity-Proof Flow

The first replacement path should preserve the demo's purpose while separating
the modules being demonstrated:

```text
Demo trigger/request
  -> Go control component
  -> generation worker adapter (existing demo agent behavior, replaceable)
  -> generated patch/test artifacts
  -> Go-controlled execution dispatch
  -> Tekton test worker using the Java 17/Spring profile
  -> demo evidence and pipeline compatibility result projection
```

The existing demo agent may be reused behind a generation-worker boundary; merely
wrapping the old combined Task is not the proof. Maven test execution must be a
separate profile-driven worker, and controller lifecycle/results must not depend
on host-specific Task or PipelineRun objects. Preserve legacy result fields
where needed (`TESTS_ADDED`, `PR_URL`, `RUN_REF`, `EVIDENCE_REF`) without making
them the canonical ATG result model. Keep the Python reference out of the runtime
composition.

This development run demonstrates generation/test integration and modular
handoffs only. It does not require VPE/VEX, Analyze/Plan durable approval,
base/candidate differential execution, production credential isolation, or MVP
sign-off evidence, and it must not claim those outcomes. Record the exact source,
commands, selected tests, worker/profile identities, and available test reports
so the proof can be reproduced and compared with the original demo behavior.

The compatibility target is the original standalone unit-generation path, not
the separate functional or integration pipelines:

```text
clone pinned source
  -> optional signature verification
  -> baseline Maven package
  -> generation worker (scratch copy; bounded generate/verify attempts)
  -> copy only changed src/test files to the shared workspace
  -> Maven verify when TESTS_ADDED is nonzero
  -> tests-only PR/MR when TESTS_ADDED is nonzero
```

The generation worker's per-candidate check is Maven `test` with a Surefire
selector for changed test classes; the later pipeline check is a separate Maven
`verify`. Preserve that distinction rather than replacing both with an
unqualified command.

The 2026-11-20 cluster's `agentic-test-generation-unit` pipeline instead uses
`test` for its later rerun. The flow above describes the intended compatibility
target, not a claim that the current deployed pipeline already matches it.

The original Pipeline results are `CHAINS-GIT_URL`, `CHAINS-GIT_COMMIT`,
`TESTS_ADDED`, and `PR_URL`. `TESTS_ADDED` is the count of changed test files,
not their path list; `PR_URL` is empty when no PR/MR is created. The generation
Task separately returns `ATTEMPTS` and `LAST_ERROR`; clone metadata also includes
the resolved commit identity. A replacement may add `RUN_REF`, `EVIDENCE_REF`,
and `terminal_status`, but those are ATG compatibility extensions, not original
pipeline results. Canonical ATG manifests remain authoritative for paths,
digests, test evidence, and lifecycle status.

## Later MVP Flow

The later conformant MVP entrypoint implements the full ATG lifecycle rather
than treating the compatibility projection as its public result model:

```text
request/admission
  -> Analyze -> durable Plan review
  -> Code -> durable generated-change review
  -> independent base/candidate Execute
  -> quality/differential gates
  -> provenance/evidence publication
```

The existing demo slash-command trigger and pipeline name can be retained only
as a deployment migration mechanism. The resulting run must emit the ATG v1
contracts, not `TESTS_ADDED`, `ATTEMPTS`, or `PR_URL` as its public result
model.

## Migration Boundary

For the current development proof, the Tekton adapter accepts the Go control
component's task/attempt intent and runs separate generation or test workers
behind stable provider interfaces. It returns attributable observations and
artifact references; it must not absorb controller lifecycle/retry authority.
Existing Maven/artifact and SCM infrastructure may be reused through the demo adapter.
The old generator task is a generation-worker implementation candidate, not the
entire replacement path. Production/MVP migration still requires the later full
ATG contracts and evidence gates.

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

The Go control module now contains a provisional `tekton.dev/v1` TaskRun/PipelineRun
renderer, identity-bound observation normalizer, and a provider adapter tested
against deterministic API fakes and status fixtures. This is not a cluster API
client or production provider selection. These tests do not establish live RBAC,
admission behavior, workspace/credential isolation, egress/resource enforcement,
actual cancellation/teardown, or artifact survival after pruning. Those require
integration on a recorded platform/profile. The same provider conformance contract
must remain usable by a non-Tekton adapter.

## Bounded Spring Baseline Probe

`spring-profile-validation.yaml` is an explicitly launched development Pod, not
an execution profile, production worker, or Go-provider integration. It checks
out the pinned application with a digest-pinned Git image, then uses the
digest-pinned Java 17/Maven image for clean package, focused test, verify, and a
resolved dependency inventory. It makes no model calls, pushes, or PRs and mounts
no Secrets or API token. This probe resolves from public Maven Central; it does
not validate the demo's credential-bearing Artifactory settings.

The 2026-11-20 run passed 121 tests in package, 7 focused tests, and the same 121
tests in verify, with no failures, errors, or skips. The full inventories, timing,
worker identity, failed probe attempts, and retained archive digest are recorded
in the [runtime evidence](../../docs/architecture/flpath-5048-runtime.md#bounded-baseline-validation-2026-11-20).

The Pod requests only 25m CPU because the development workers were almost fully
reserved despite low measured utilization. This is a scheduling workaround, not
production sizing. The Maven container still has a 2-core/4Gi limit, a 2Gi memory
request, a 4-GiB ephemeral-storage limit, and a 900-second overall deadline. No
existing workloads are resized by the manifest. Review the manifest and cluster
capacity before launching it elsewhere; it does not enforce egress/PID policy.
The observed run was admitted with the existing `artifactory-scc` and `default`
service account. Its fixed UID/fsGroup must be reviewed for another namespace's
SCC policy; this manifest does not grant permissions or prove restricted-SCC
compatibility.

After authenticating and checking development-cluster health, use an explicit
context/namespace and a new archive destination:

```sh
set -euo pipefail
umask 077
: "${ATG_KUBE_CONTEXT:?Set the development cluster context}"
: "${ATG_NAMESPACE:?Set the development namespace}"
: "${ATG_EVIDENCE_ARCHIVE:?Set a new local evidence archive path}"
test ! -e "$ATG_EVIDENCE_ARCHIVE"
POD="$(oc --context="$ATG_KUBE_CONTEXT" -n "$ATG_NAMESPACE" create -f deploy/development-harness/spring-profile-validation.yaml -o name)"
oc --context="$ATG_KUBE_CONTEXT" -n "$ATG_NAMESPACE" wait --for=condition=Initialized "$POD" --timeout=90s
oc --context="$ATG_KUBE_CONTEXT" -n "$ATG_NAMESPACE" wait --for=condition=Ready "$POD" --timeout=780s
oc --context="$ATG_KUBE_CONTEXT" -n "$ATG_NAMESPACE" exec "$POD" -c maven -- tar -C /workspace -cf - evidence > "$ATG_EVIDENCE_ARCHIVE"
```

Readiness means evidence is collectible, **not that validation passed**. Retain
the Pod UID, resolved image IDs, and effective resource/security settings too.
Check the archive's digest, every phase exit code, nonempty XML report inventories,
test IDs/counts and outcomes before acknowledging collection. Missing reports or
timed-out commands are not passing tests. Keep phase directories separate: the
focused reports must not include stale full-suite XML. These tests include eight
integration classes under Surefire and must not be described as unit-only.

Only after verified retention, release the collector barrier. For a successful
run, confirm termination and delete only the recorded Pod:

```sh
oc --context="$ATG_KUBE_CONTEXT" -n "$ATG_NAMESPACE" exec "$POD" -c maven -- /bin/sh -c 'printf "collected\n" > /workspace/release'
oc --context="$ATG_KUBE_CONTEXT" -n "$ATG_NAMESPACE" wait --for=jsonpath='{.status.phase}'=Succeeded "$POD" --timeout=60s
oc --context="$ATG_KUBE_CONTEXT" -n "$ATG_NAMESPACE" delete "$POD" --wait=true --timeout=30s
```

For failures, preserve the failed or `not_reached` outcomes and confirm failed
termination instead; do not reinterpret them as success to perform cleanup.
The overall deadline includes collection. A hung collector is bounded, but the
ephemeral workspace can disappear without a retained archive. Local retention
does not satisfy production-durable artifact publication or profile approval.

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
New generic execution/quality advancement is intentionally rejected until a
trusted execution-ledger bridge is available. Existing historical journals can
still be replayed, but replay does not make their old checkpoint transitions
available to new event submissions.

## Offline Test-Generation Simulator

With Go 1.26.2 and module dependencies available locally, run the simulator from
the Go module directory without a database, repository checkout, Maven, Tekton,
or a cluster:

```bash
cd services/control
go run ./cmd/atg-testgen-sim -scenario success
go run ./cmd/atg-testgen-sim -scenario retry
go run ./cmd/atg-testgen-sim -scenario no-changes
go run ./cmd/atg-testgen-sim -scenario unknown-outcome
```

The embedded `internal/testgensim/offline-profile.json` is synthetic metadata,
not an executable Maven profile. The simulator hashes those exact embedded bytes
and binds that digest to the simulated run. Deterministic in-memory workers return
scripted outcomes to the real Go coordinator; Maven phase names are labels only,
and the repository and change-request URLs use the reserved `example.invalid`
domain. The simulator process makes no network or subprocess calls. Go's initial
toolchain/module bootstrap is separate and must already be satisfied for a fully
air-gapped build. All report evidence and results are synthetic and must not be
treated as test, build, source, or provider evidence. This exercises
orchestration and retry semantics only; it does not validate a real application
profile or worker.

The same simulator can be exposed through the Go control HTTP server in an
explicit loopback-only mode:

```bash
cd services/control
ATG_MODE=offline-simulation go run ./cmd/atg-control
```

Submit one of `success`, `retry`, `no-changes`, or `unknown-outcome` to
`POST http://127.0.0.1:8080/development/simulations/test-generation` with a JSON
body such as `{"scenario":"retry"}`. The endpoint returns the same synthetic
report as the CLI. This mode binds only to an IPv4 or IPv6 loopback IP address;
it does not configure run persistence, authentication, execution-provider
dispatch, or production readiness. `/health/ready` and execution routes remain
unavailable. The default control-service mode does not expose the simulator.

## Analyze-Plan-Code Profile Candidate Simulation

Run the Python reference phases against a temporary Java/Maven fixture and emit
a partial execution-profile candidate with field-level artifact references and
separate profile-approval and external-integration gaps:

```bash
PYTHONPATH=src PYTHONDONTWRITEBYTECODE=1 .venv/bin/python -m atg.component.profile_candidate_simulator
```

This invokes the actual local Analyze, Plan, and Code services, but uses a
temporary repository, a fixture Maven metadata analyzer, a replayed generated
test, and synthetic checkpoint decisions. The server is bound to loopback; it
does not call a model, execute Maven, clone a repository, or contact an external
provider. The phase decisions only exercise the phase gates and are explicitly
not profile authorization. The output candidate is intentionally incomplete,
fails the approved execution-profile schema, and is never dispatchable. Its
facts describe the synthetic fixture, not the pinned sample application.
