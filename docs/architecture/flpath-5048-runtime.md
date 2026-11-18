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
  Java 17/Spring sample application; later add Java 21/Quarkus as a second profile
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
- This does not claim that the first application profile or second Quarkus profile is
  already implemented/approved, or that either target has a valid differential
  base/candidate pair.
- This does not make local filesystem or fake-provider composition a production
  fallback.

## Provisional Provider Candidate

No FLPATH-5051 responsibility decision is present in this implementation
checkout. To continue adapter work without treating demo reuse as approval,
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

## First Sample Target Profile (Source Pinned; Execution Profile Pending)

The target is the original sample-application use case, not a target selected by
the later VPE analysis work. The demo's GitLab seed configuration imports
`https://github.com/lzspangler/spring-boot-example-application.git` as
`development/spring-boot-example-application` with default branch `main`.
`01a18a1b40e151ffcc1d7bef86bf335f87999750` was the public upstream `main` head
on both 2026-10-30 and 2026-11-02, based on GitHub history queries. Read-only
GitLab API inspection on 2026-11-18 confirmed the imported project's current
default branch is also `main` at that exact SHA. Three successful
`agentic-test-generation` runs on 2026-11-02 requested `main` and recorded the
same resolved SHA. This confirms the date-aligned candidate as the sample app
source, independent of VPE's later use of that commit. At offline verification,
current public upstream `main` was `83b6f931c08071133678284548ff19b13f6f8db0`,
eight commits beyond the imported source; it is a comparison point, not the
profile subject.

The imported POM identifies Java 17, Maven, and Spring Boot 2.7.18. The source
tree has eight `*IntegrationTest.java` classes, along with unit/regression test
classes. This imported POM does **not** declare Failsafe or `*FT` tests. The
newer upstream `83b6f931...` POM adds a Failsafe `*FT` suite; do not apply that
newer test plan to the pinned source. The imported README claims 121 total
tests, but that count has not been independently confirmed from retained test
reports.

The successful Nov. 2 package TaskRun used the `maven` Task with subdirectory
`source` and its default `package` goal. This is the strongest available
baseline command evidence; the run completed against the exact imported SHA.
The Task's OpenJDK 17 Maven-goals image was
`registry.redhat.io/ubi9/openjdk-17@sha256:5d619e202695acf76a5f32c07ace6709f5ee5d3999bf977e1bf95ad34197f44a`;
its settings-generation step used a digest-pinned UBI9 image. These are observed
demo task inputs, not approved ATG worker identities or resource limits.

The cluster also has a `test-image-integration` Pipeline installed, with
`clone-repository -> create-test-namespace -> deploy-app-image -> run-tests ->
comment-test-evidence` and unconditional namespace cleanup. It has no recorded
PipelineRun. Its test command uses `test-compile`, direct Failsafe
`integration-test`/`verify`, and `-Dit.test=*IT,*IntegrationTest`. That selector
selects the eight imported integration classes. The old POM does not pin the
Failsafe plugin, so a profile choosing this separate Failsafe route must pin a
reviewed plugin version and prove nonzero, expected test counts. Failsafe is not
required merely to discover these class names: they end in `Test.java`, match
Surefire's default `**/*Test.java` include, and the pinned application POM
declares no Surefire include/exclude override or Failsafe binding. Maven
`package` runs through Surefire's `test` phase, so it is expected to select the
`*IntegrationTest` classes as well as the regression `*Test` classes; retained
reports are still needed to confirm the actual class count and outcomes. A
separately invoked Failsafe command would run the selected integration classes
under Failsafe reports, so avoid running both routes in one profile unless
duplicate execution is intentional. Record exactly which tests each command
executes rather than label `package` as unit-only. One historical package
TaskRun succeeded; the cluster also retains five failed generation PipelineRuns
whose logs were not inspected.

The current legacy-cluster `test-image-integration` path is a reference, not an ATG profile
or execution evidence. The sample application's Argo Application currently has
`sync=Unknown`, and its `spring-boot-example-dev` destination namespace does not
exist; no running app image digest was available. The demo platform's CI
PipelineRuns are historical evidence, not a current app deployment. Functional
endpoint tests are not present at the imported SHA.

Candidate command shapes to verify in an isolated, pinned profile are:

```sh
mvn -B -s "$MAVEN_SETTINGS" clean package
mvn -B -s "$MAVEN_SETTINGS" test-compile org.apache.maven.plugins:maven-failsafe-plugin:<pinned-version>:integration-test org.apache.maven.plugins:maven-failsafe-plugin:<pinned-version>:verify '-Dit.test=*IT,*IntegrationTest'
```

The first follows the observed Maven `package` behavior. The second is only a
candidate for separately selecting the integration classes and must pin the
plugin rather than inherit Maven prefix/version resolution. Do not run both
commands as one combined suite without accounting for duplicate execution of
`*IntegrationTest` classes. Validate the chosen route against the captured
source and worker. Retain Surefire/Failsafe JUnit XML, command exit/timing,
resolved dependency inventory, and immutable source/profile/worker identities.
The README's 121-test count is not a profile oracle; discover and execute the
selected suites under the approved worker.

Maven settings are not part of the public application source and the README
documents Lightwell repository credentials. The existing cluster Artifactory is
the dependency-mirror candidate; do not provision another mirror unless its
configured virtual repository cannot serve the pinned dependency graph.
The temporary demo fork shows the intended route: all Maven resolution goes through the
Artifactory `maven` virtual repository, with Maven Central configured as an
upstream. Its example/settings helper uses the Artifactory `admin` credential
because OSS anonymous access is disabled, and explicitly treats this as
development-only. The user accepts existing development credentials for the
current compatibility work; credential hardening is a follow-on and does not
block that implementation. Keep credential values out of source/profile data.
The current demo agent path combines model credentials, repository edits, and
Maven execution; that is a known compatibility-mode limitation, not an approved
ATG execution boundary. Conformant or higher-trust use requires credential
separation and a least-privilege read credential or approved anonymous-read
policy.

Functional `*FT` execution is excluded from this initial candidate because no
such tests exist at the pinned SHA. If a later target revision adds endpoint
tests, its app lifecycle, credentials, isolation, and reset/cleanup policy need
separate approval.

### Test-Generation Modularity-Proof Scope

The current objective is to replace the legacy demo's test-generation step with an
ATG integration that demonstrates the modular design. This is development
integration work, not an attempt to satisfy the VPE-driven MVP. VPE context and
its delivery contract are still in active development; their absence does not
block this proof, and this repository must not invent their schema.

The proof should keep the existing demo flow as the host while routing test
generation and execution through explicit ATG boundaries: the demo adapter
translates the demo request/results, the controller owns target-independent
lifecycle, the Java 17/Spring profile owns Maven commands and test/report
conventions, and the provider/worker executes that profile. Existing development
credentials are permitted for this work. The demo should preserve its expected
test artifact and result handoff without placing demo-specific behavior in the
controller or treating the local playground as a runtime dependency.

Success means the legacy demo can invoke the replacement path for the pinned
application, produce or accept generated tests, execute the selected Maven test
flow through the profile/provider seams, and return attributable test results.
A same-tree run proves integration compatibility only; do not describe it as a
VPE-policy result, a base/candidate differential, or fix validation.

For this development proof, the selected composition is the Go control
component with Tekton-backed workers. Keep Tekton objects and API details behind
the provider boundary because the active responsibility spike may refine that
seam. The existing Python `GenerationService` remains the reference, not the
production generator; the full path still needs a model-backed generation worker
and an explicit handoff into the Go-controlled test execution. Align the local
demo integration with the original flow before deployment. No cluster
deployment or teardown is part of this implementation step.

The VPE/VEX input contract, policy conformance, durable Analyze/Plan review,
immutable base/candidate differential execution, and owner-reviewed signed
evidence belong to the later MVP integration. They are future extensions, not
gates for this modularity proof. The execution profile, worker limits, selected
test command, and evidence handoff still need to be made concrete for the demo
run, but they do not need the full MVP approval framework.

### Read-Only Cluster Snapshot (2026-11-18)

The cluster health check showed OpenShift 4.20.12, all six nodes Ready, and all
cluster operators Available with none progressing or degraded. The imported
GitLab project `development/spring-boot-example-application` currently has
default branch `main` at `01a18a1b40e151ffcc1d7bef86bf335f87999750`. Three
successful legacy-demo `agentic-test-generation` runs on Nov. 2 requested that branch
and recorded the same resolved source SHA. This directly confirms the
date-aligned source candidate.

The `spring-boot-example-dev` Argo Application points at the imported
`development/spring-boot-example-gitops` repository, chart path
`app/spring-boot-example`, and branch `main`, but reported `sync=Unknown`; its
destination namespace `spring-boot-example-dev` did not exist. No deployed
sample-application image digest was therefore available. Demo platform services
are deployed; that does not mean the sample application itself is currently
running.

The demo's CI namespace has the `maven` Task and the `test-image-integration`
and `test-image-functional` Pipelines installed, but no PipelineRuns for either
test-image pipeline. The successful Nov. 2 package TaskRun used the Maven Task's
default `package` goal and
`registry.redhat.io/ubi9/openjdk-17@sha256:5d619e202695acf76a5f32c07ace6709f5ee5d3999bf977e1bf95ad34197f44a`.
Read-only inspection of that pinned image reports Red Hat Maven 3.8.5 and
OpenJDK 17.0.15. The
namespace currently has no LimitRange or ResourceQuota. The current `maven` Task
steps have empty `computeResources`, and the retained package TaskRun pods have
no CPU/memory requests or limits. The current `ai-generate-tests-unit` Task
declares 500m CPU and 2Gi memory requests with a 4Gi memory limit. Retained Nov. 2
TaskRuns instead reference the older `ai-generate-tests` Task; their embedded
resolved TaskSpec explicitly declared 100m CPU, 2Gi memory, and a 4Gi memory
limit. That 100m was a Task setting, not a namespace LimitRange default. No
retained TaskRun shows execution with the newer 500m setting, so its runtime
effect has not been observed. The
Argo Application managing `pipelines/tasks` is `OutOfSync` (health reports
Healthy) and points at the legacy demo repository revision
`8655bc84d850f9c2559be69231019b8ba21cc480`; do not assume the live task set is
identical to the local validated deployment-pattern checkout without reconciling that state.

Artifactory is deployed in the `artifactory` namespace. Its Argo Application is
`Synced/Healthy` at chart revision `107.161.26`; its route/service exist and all
four StatefulSet/Deployment workloads were ready in this snapshot. The
Artifactory and router containers had recent restarts (the router's last
termination exit code was 137) despite being Ready; no logs were read and the
restart cause is undiagnosed. The successful package TaskRun confirms Maven
resolution worked at that time. That historical run bound `maven-settings` to a
PVC of that name, but the PVC no longer exists. The later local temporary demo fork
helper instead binds `maven-settings` to a Secret; that binding is not assumed
active because the task GitOps Application is OutOfSync. No settings/PVC/Secret
contents were read.

This snapshot used read-only `oc get` and unauthenticated GitLab API GET
requests with the cluster's public ingress CA. No Secret values were read and no
cluster resources or PipelineRuns were created or changed. The app source SHA is
now established, and Artifactory is available as the mirror candidate. The
resolved dependency inventory, live Maven settings/credential binding, active
app image, selected worker image/limits, and profile approval remain
outstanding.

The local validated deployment-pattern checkout is the deployment-pattern reference
for this workstream; its checked-out revision was `1f9d2842b9f4763e1da7a9b490e5b837889a4437`.
Its Maven task currently references digest-pinned UBI9/OpenJDK 17 images, but
those are observations for evaluation, not selected ATG worker images; the task
does not define explicit CPU/memory resource limits. Its bootstrap defaults
currently point GitOps at the legacy demo repository's configured branch, not at this local
pattern checkout, so the rendered/deployed source must be verified before
relying on those values. The local temporary demo fork snapshot
(`79cd122f776b6f85e14f5dd52ffc2318cd44e348`) has a top-level `cluster/`
preparation directory absent from the validated deployment-pattern checkout; the user identifies
FLPATH-5132 as the work to close that gap. Keep those cluster scripts and local
configuration out of the ATG runtime/profile dependency. Its helper also has a
developer-specific checkout default and references additional bootstrap files
outside the inspected checkouts, so do not treat it as a portable bootstrap
without FLPATH-5132's completion. The snapshot above used the approved login and
read-only inventory workflow; no cluster bootstrap scripts were run.

### Temporary Demo-Fork Workspace And Image Pattern

The local demo action helper binds a per-run `workspace` using a fresh
5-GiB `ReadWriteOnce` `volumeClaimTemplate`. It binds `maven-settings` as a
Secret-backed workspace, not a PVC, and uses a separate Git-auth Secret. This
does address the persistent Maven-settings-PVC concern in the fork: source/build
files are isolated in an ephemeral per-run workspace, while Maven settings are
mounted as a Secret. The historical Nov. 2 run used a PVC-backed
`maven-settings` workspace; the fork's later helper is not proof that this
binding is deployed in the active cluster.

The fork provides different images for test execution and generation:

- **Build/test task candidate:** `pipelines/tasks/maven.yaml` uses the
  digest-pinned `registry.redhat.io/ubi9/openjdk-17` image observed in a
  successful cluster TaskRun. It runs as UID 65532 and has empty
  `computeResources`; this is a useful Java 17/Maven baseline, but CPU/memory
  requests and limits still need definition.
- **Generation-agent candidate:** `agentic-test-generation` defaults to
  the demo's Aider generation image at version `v1.0.1` (or a Claude flavor). The Aider
  Dockerfile pins Maven 3.9.9 and Aider 0.86.2 but uses mutable
  `ubi9/ubi-minimal:latest`, and the pipeline reference is a tag, not a digest.
  The `ai-generate-tests` task injects `ai-agent-secret` into its environment
  while the agent also runs Maven verification. Treat this as a generation
  runtime reference, not the isolated worker for untrusted tests. Keep model
  credentials out of the test-execution worker and pin an adopted agent image by
  digest.

The resource history explains the 100m/500m discrepancy: the retained Nov. 2
`ai-generate-tests` TaskRun resolved to 100m CPU, while the current local fork
Task and currently installed `ai-generate-tests-unit` Task declare 500m. The
historical `maven` TaskRun has no CPU/memory requests or limits, matching its
empty `computeResources`; it should receive explicit profile sizing rather than
relying on cluster defaults. The 500m setting is present in current Task
configuration but has not yet been verified by a corresponding run.

There is a credential-leak hazard in the fork's Maven Task: its
`maven-generate` helper prints the copied/generated `settings.xml` to step
stdout. The sample Secret contains an Artifactory `admin` credential, so Secret
mounting alone does not prevent disclosure through Tekton logs. The checked-in
example uses a placeholder, but its instructions generate the real settings
with the `admin` credential. The user accepts development-only credentials for
the current compatibility work; retain the settings-log exposure as a follow-on
hardening item and do not use broad credentials outside development. Remove or
redact settings logging before production or higher-trust use.
No Secret values or TaskRun logs were read. The out-of-sync live Task Application
means this local fork behavior has not been assumed to match the installed Task.

The application target profile is the pinned sample source at
`01a18a1b40e151ffcc1d7bef86bf335f87999750`. The worker/execution policy is still
pending review: record the exact dependency state, approved Maven mirror and
credential reference, immutable worker image/toolchain, command and test
selectors, report handling, resource/deadline limits, egress policy, workspace
isolation, and reviewer/authorization identity. A same-subject control can
validate the mechanism; no behavioral differential is claimed until a
characterized candidate and expected comparison are attributable.

## Reconsideration Conditions

Revisit a choice only with concrete evidence that it blocks a required supported
deployment or violates an operational/security requirement. Record migration and
compatibility impact before changing controller runtime, durable state authority,
or artifact identity semantics. Agent/provider choices remain independently
replaceable behind their contracts.
