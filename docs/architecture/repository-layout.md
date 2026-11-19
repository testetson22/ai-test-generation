# Repository Layout and Integration Map

The product repository is organized around contracts, ports, adapters, tests, experiments, and deployment composition. See the root `README.md` and `CONTRIBUTING.md` for the contributor workflow.

## Repository Decision

Keep the reference implementation and production components in this repository
for now. Repository boundaries, runtime responsibilities, deployment units, and
build modules are separate decisions. Do not create one repository or service per
lifecycle stage by default, or make a repository split a prerequisite for delivery.

Language, platform, provider, and agent-harness choices must preserve the
boundaries below. No production runtime or physical service topology is selected
by this layout.

## Component Boundaries

| Path | Responsibility | Boundary |
|---|---|---|
| `contracts/` | Canonical schemas, compatibility data, and shared fixtures | Language-neutral payloads and semantics; no provider SDK or framework dependency |
| `src/atg/`, root `pyproject.toml` | Python reference and development components | Executable conformance baseline, not a mandatory production runtime dependency |
| `services/<component>/` | Production service or runner implementation | Explicit public interfaces; component-local source, dependency/build manifests, tests, and packaging |
| `workers/<profile>/` | Workload image recipes and approved execution profiles | Toolchain, commands, limits, and permissions; worker protocol remains independent of the host platform |
| `tests/` | Existing reference tests and shared cross-component/conformance tests | Native component tests also run in their own modules; reference tests alone do not prove production conformance |
| `experiments/` | Provider, platform, and harness comparisons | No hidden production import or runtime dependency |
| `deploy/<environment>/` | Deployment and environment composition | Wire explicit components/profiles; do not redefine lifecycle semantics or bake environment settings into service code |

`services/` and `workers/` are reserved conventions, not currently implemented
directories. Add concrete modules when implementing them, not placeholder trees.
Reusable worker executable code belongs in an implementation module; workload
profiles and image recipes reference it rather than copying its source.

Components exchange versioned documents/artifact references or explicit library
interfaces, not another component's private database tables or source internals.
Shared libraries require an explicit package/build boundary; common repository
location does not make private implementation code a supported interface.
Provider SDKs and harness types stay inside their adapters. Consumers pin/bundle
the supported contract assets and preserve shared digest and replay semantics.

## Build Boundaries

- Each independently buildable module owns its dependency manifest, version pins
  or lockfile as appropriate, native build/test commands, and output artifacts.
  Document its working directory and commands when the module is introduced.
- Build/test a component without installing unrelated service toolchains or
  relying on unbuilt sibling source. Explicit shared build dependencies must be
  declared and reproducible; no accidental cross-directory packaging.
- Multiple runtime roles may share a module or image when their dependencies and
  build/release boundary genuinely match. Different runtime configurations do
  not automatically require duplicate source, manifests, or services.
- Service configuration and worker profiles must declare their role, input/output
  contracts, required capabilities, credential scope, and resource limits. A
  shared image or configuration switch is not a security isolation boundary.
- Keep the Python reference checks runnable independently. Go, Java, or other
  modules must run their own native tests and contract-conformance checks;
  Python test discovery does not exercise them.
- Add native build/test/package CI jobs and path/dependency triggers with every
  new module. Shared contract or fixture changes must run checks for affected
  consumers, not just the Python reference. Include code-review routing for new
  paths; existing `CODEOWNERS` patterns do not cover reserved directories.
- Keep native/component validation separate from real platform integration.
  Building an image or passing a fake provider test does not prove worker
  isolation, execution, or evidence retention on the selected platform.

The current root `pyproject.toml` and
`.github/workflows/atg-contracts-and-components.yml` build/validate the Python
reference and contract assets only. No Go/Java production build is implied.

## Provisional Phase Runtime Boundaries

For a production path expected to use Go, default to Go for first-party phase
orchestration and lifecycle rules. This is a provisional implementation
direction, not a commitment to one service or binary per phase. Keep the Python
implementation under `src/atg/` as a reference/conformance implementation; do
not make production Go code import it or invoke it as a hidden subprocess.

| Phase | Production responsibility | Language guidance |
|---|---|---|
| Analyze | Bind findings to the admitted repository revision, preserve evidence and uncertainty, and produce repository/test facts and objective candidates | Go owns request, provenance, capability, and result semantics. Use Go analyzers by default; isolate a Python or tool-specific analyzer behind a versioned adapter only when a concrete library/tool advantage justifies it. |
| Plan | Turn approved analysis into a complete objective disposition, test intent, and checkpoint | Keep deterministic coverage, policy, persistence, and reviewer gates in Go. Any model-generated proposal is advisory input, not approval or executable authority. |
| Code | Bound generation attempts and retain model/tool/worker identity, usage, patch, validation, and failure evidence | Go owns lifecycle and output validation. A model/agent harness may be a separately deployed provider in any language. |
| Execute | Bind an approved profile to immutable source and artifacts, authorize attempts, reconcile providers, and accept evidence | Keep controller state and retry/cancellation authority in Go. Workload images use the runtime/toolchain required by the profile and remain isolated from model/controller credentials. |

An AI harness such as Pydantic AI can be used by Go only across a process
boundary; Go cannot use its Python package as a native library. If selected, run
it as an independently built service/worker implementing an explicit
task-runner adapter. The boundary should carry a bounded task/attempt identity,
input artifact references and digests, allowed tool capabilities, and budgets;
responses should identify model/tool/worker versions, usage, status/failure,
and output artifact references/digests. For work that can outlive an API request
or controller process, expose idempotent submit, observe/reconcile, and cancel
operations; bind submission identity to the task, attempt, and input digest.
Unknown submission outcomes must be observable/reconcilable rather than silently
re-launched. A synchronous call is suitable only for bounded operations whose
timeout/recovery semantics are explicit. Choose HTTP, gRPC, or a durable queue
only after the harness spike establishes its asynchronous, cancellation, and
recovery needs.

The harness must not own ATG lifecycle retries, reviewer approval, execution
profile approval, or canonical run state. Do not expose its graph/checkpoint or
SDK objects as shared contracts. Keep model credentials in the harness boundary;
never pass them to a worker executing untrusted repository code. A selected
Python harness must have its own pinned dependencies, build/package, tests,
deployment, authentication, observability, and failure/reconciliation contract.
It is an optional provider, not a reason to keep Analyze, Plan, Code, or
controller semantics in Python.

The remaining language question is per concrete adapter, not whether the whole
product must be ported at once. Compare the actual library/tool support,
isolation model, operational burden, dependency lifecycle, and conformance results
for each exception. Pydantic AI remains a candidate until its spike meets those
criteria; this guidance does not select it or a transport.

## Future Repository Separation Points

Keep one repository now, but preserve the following extraction seams. They are
possible source/build boundaries, not a commitment to one repository or network
service per row. A component can move repositories while remaining a library or
sharing an existing deployment.

| Extractable unit | Supported seam | What must not cross implicitly |
|---|---|---|
| Contracts and conformance assets | Versioned schema/fixture bundles, digest vectors, and documented lifecycle semantics | Python imports, live `main` resolution, or provider-specific payload types |
| Python reference | Its installable package/CLI and public reference interfaces, consuming declared contract assets | Production reliance on the reference source tree, development database, or process |
| Control component | Versioned task/attempt, approval, provider-operation, and result/artifact interfaces; explicit persistence adapter | Agent framework state or another component directly writing controller tables |
| Agent runtime/harness adapters | Authorized task specification and normalized outputs/failures/usage/artifact references | Framework graphs/checkpoints/SDK types, hidden retries, or model secrets in workload execution |
| Repository worker and profiles | Task intent, approved profile/image identity, independent subject inputs, and execution/evidence manifests | Host paths, shared writable workspaces, agent/controller credentials, or a demo checkout dependency |
| Provider adapters | Declared submit/observe/reconcile/cancel/cleanup capabilities and conformance cases | Platform resource objects becoming canonical lifecycle state; a separate service is not required |
| Deployment composition | Released images/packages/profile artifacts, configuration and secret references, supported-version matrix | Source-tree mounts, undeclared root build steps, or contract/state semantics embedded only in deployment scripts |

Keep quality gates as an explicit application/package boundary. They can be
extracted if warranted without making an additional service a prerequisite.

Every new buildable module must identify its public inputs/outputs, supported
contract versions, declared dependencies, native build/test/package commands,
runtime configuration, release artifact, and maintainer review routing. Record
compatibility and retry/cancellation guarantees at the interface, not only in a
root-level integration test.

### Extraction Checks

- Build, test, and package from a clean component checkout/staging tree containing
  only that module and declared dependencies. Root orchestration may be a
  convenience, not a hidden requirement. Container build contexts must not depend
  on undeclared sibling source or workstation paths.
- Development workspaces, editable installs, or local module overrides must not
  be the only dependency resolution path. Release builds consume pinned packages,
  images, profiles, or contract bundles and work without sibling repositories.
- Any deliberately shared source becomes an explicit versioned package with its
  own build/test contract before extraction; do not copy private code into each
  repository or introduce an unbounded shared utility layer.
- Share portable conformance scenarios, fixtures, digest vectors, and simulator
  protocols through declared artifacts/packages. Keep native tests in their
  component and avoid importing another implementation's private test helpers.
- Bundle/register supported schemas locally. Preserve existing schema `$id`,
  artifact identities, and protocol versions when source moves; changing a Git
  remote is not authorization to rewrite public identities. See the
  [contract distribution boundary](contract-distribution-and-api.md#repository-independent-consumption).
- Preserve one logical writer for lifecycle state. A repository move does not
  itself require moving data or changing service topology. If authority/storage
  changes, explicitly plan migration/cutover, fencing, and replay verification;
  never create a second lifecycle writer to bridge repositories.
- Carry native build/package and contract-conformance CI with the component.
  Contract releases require checks for affected supported consumers, and deployment
  integration pins compatible artifact versions rather than assuming atomic
  cross-repository commits. Configure this release/CI coordination when the
  first independent consumer or repository is introduced.
- Update maintainer/operational accountability, review routing, release permissions,
  source/provenance references, licenses/notices, and deployment references during
  extraction. Do not infer these assignments from the new repository name.

These are implementation acceptance criteria, not extraction checks already
running in CI: production modules do not exist yet. Add the clean-component build
and conformance jobs with each module. No repository split, placeholder package,
or remote service is needed merely to establish these seams.

## Current Runtime Inventory

Snapshot: 2026-11-17. The runtime responsibilities below are the implementation
direction, not a claim that production modules or deployments already exist.

| Component or capability | What must be built or integrated | Deployment boundary |
|---|---|---|
| Control component | API/application layer, durable task/attempt ledger, approval and result acceptance, dispatch, retry scheduling, cancellation, and reconciliation | Initial production service boundary; API and reconciler may share a module/deployment |
| Agent task runner | Bounded model/tool execution for applicable Analyze, Plan, and Code tasks, with attributable outputs and usage | Separate runtime when dependency, credential, scaling, or language needs justify it; Pydantic AI is a candidate, not selected |
| Repository workload worker | Immutable source preparation, exact approved-test materialization, build/collection/execution, bounded evidence collection, and teardown | Ephemeral isolated workers with task-specific profiles; independent base and candidate instances |
| Execution-provider adapter | Submit, observe/reconcile, cancel, and clean up jobs through the selected platform | Normally an adapter, not another required standalone service; Tekton remains a candidate informed by prior demo patterns |
| State and artifact storage | Durable task/event/result records plus content-addressed artifacts, access controls, and retention | Integrate backing stores; do not invent bespoke storage services or assume reference SQLite is the production database |
| Quality/differential gates | Deterministic evaluation of generated tests and accepted execution evidence | Application/library boundary for FLPATH-5049, not a required extra service |

The Python reference already implements admission/readiness, Analyze, Plan,
generation, review/state projections, local persistence, and artifact-integrity
checks. The new Go control module now contains PostgreSQL schema/migration,
pair/task/attempt persistence, fenced claims, provider reconciliation,
controller-authorized retry scheduling, and cancellation operations. These are
not yet wired into the runnable HTTP composition; artifact durability, real
worker/provider integration, and evidence collection remain unimplemented.
PostgreSQL integration tests are configured but require a running test database.

Initial FLPATH-5048 runtime/storage choices are recorded in
[`flpath-5048-runtime.md`](flpath-5048-runtime.md): Go control service, PostgreSQL
coordination store, and S3-compatible artifact boundary. They do not select the
worker platform, agent harness, storage vendor, or require one deployable per role.

Do not create one service for each lifecycle phase by default. An agent runner can
handle several task kinds; a workload worker can use different approved profiles
for preparation, validation, and execution. Repository build/test code remains
untrusted even when an agent requests it, and must not inherit model or controller
credentials. A runtime role is not itself an isolation guarantee.

## Immediate Implementation Direction

The control component is authoritative for task-level retry eligibility, attempt
creation, budgets, backoff, and deadlines. Runners/providers report observations
and carry out authorized attempts; their local retry mechanics must be bounded,
accounted for, and consistent with controller policy. The task/attempt model and
recovery requirements are detailed in [run-state.md](run-state.md#controller-task-ledger).

Implement the smallest execution vertical slice in this order:

1. Define shared task/attempt and provider-operation contracts plus digest,
   approval, failure, and replay conformance fixtures.
2. Build the control service's durable ledger, exclusive claims, dispatch,
   reconciliation, retry scheduling, and cancellation, using fakes only in tests.
3. Implement the repository workload worker and one explicitly chosen provider
   adapter, reusing demo patterns only after verifying the required safety and
   evidence relationships. Consume the approved generation handoff, not the
   Python reference's private storage.
4. Integrate the selected agent harness behind its task boundary rather than
   making the controller depend on framework-specific types. The execution slice
   can start from persisted approved artifacts without selecting a live generator.
5. Onboard the Java 17/Spring Boot sample application as the first supported execution
   profile. Pin the application independently of its playground; demonstrate
   focused tests, isolated subject controls, evidence/recovery and cleanup, then
   characterize a distinct base/candidate comparison without inventing a fix.
6. Add the upstream Java 21/Quarkus profile when ready, reusing controller lifecycle
   and recovery logic unchanged. Retain both profiles as regression coverage and
   hand attributable observations/limitations to FLPATH-5049.

Application identity, build/test commands, toolchains, test/report selection and
application setup belong in approved profiles/framework adapters. Controller
state/retry logic and provider job semantics remain target-independent. The first
real profile establishes the mechanism; a second framework demonstrates portability.
Quarkus availability gates that coverage, not useful real execution on the sample app.
No approved ATG execution profile for either application is supplied by this plan.

Service/runner language, persistence technology, platform, agent harness, and
physical packaging remain explicit open decisions. FLPATH-5051 supplies decision
evidence, not production code; record a superseding responsibility decision during
implementation if the spike is unavailable rather than blocking indefinitely.
No missing spike output permits skipping isolation or recovery requirements.

Stub replaceable harness/provider decisions behind the task and job-operation
boundaries rather than waiting for FLPATH-5050/5051 to select implementations.
Use deterministic/replay agent substitutes, stateful job-provider simulation,
and adapter-level platform fixtures as described in the
[pending-spike strategy](../../experiments/README.md#working-around-pending-spike-decisions).
These are planned test/development substitutes, not production selections or
implemented production modules, and do not validate either spike's recommendation.

The legacy demo and its temporary playground inform implementation, not these
interfaces. The selected application profile is a supported integration target,
not a dependency on that playground or a claim of upstream Quarkus coverage. See the
[development-harness assessment](../../deploy/development-harness/README.md)
for inspected snapshots, reuse candidates, and limitations.

## Ownership Terms

Use explicit terms rather than an unqualified "owner":

| Term | Meaning | Example |
|---|---|---|
| Runtime responsibility | Which logical component is authoritative for behavior/state | The control role validates a worker result and advances the run revision |
| Resource lifecycle responsibility | Which component creates, monitors, cancels, retains, or deletes a resource | An execution provider tears down a workspace after the required evidence acknowledgement |
| Maintainer accountability | Which person/team reviews and maintains code or contracts | `.github/CODEOWNERS` routes review requests for existing paths |
| Operational accountability | Which person/team deploys, supports, and handles incidents | An operator responds to a stuck job or artifact-store outage |

A responsibility can be delegated without moving authority. For example, the
control role may request cancellation and reconcile its result, while the
provider performs process termination and cleanup. Assign one logical lifecycle
authority and an explicit retry policy; do not let both independently advance a
run or multiply retries. One logical authority may have multiple coordinated
replicas; it does not require a singleton process.

Proposed runtime roles are control, agent task execution, and isolated repository
workload execution. These describe responsibilities, not three selected services.
Agents return bounded outputs and providers return execution observations; neither
may approve its own output or bypass authoritative lifecycle checks. Co-located
roles may use internal interfaces; separate deployables need explicit transports.
Untrusted workloads still need actual isolation, not just role configuration.

Runtime diagrams and FLPATH-5051's responsibility split primarily address the
first two ownership terms. They do not assign people. `CODEOWNERS` identifies
current code-review routing, not on-call coverage, deployment authority, or a
runtime human-review authorization policy. Record human/team assignments
explicitly; do not infer them from architecture or invent unassigned operators.

## Parallel Paths

| Workstream | Primary paths | Integration point |
|---|---|---|
| VI: VEX and policy input | `contracts/`, future production adapters, input fixtures | Versioned VPE/VEX and policy contracts |
| TF: target and framework | future production adapters, target metadata | Per-application profile/framework boundary; Java 17/Spring sample first integration, Java 21/Quarkus portability coverage |
| TG: core pipeline | `src/atg/domain/`, `src/atg/application/`, `src/atg/workflow/`, `src/atg/gates/`, `src/atg/provenance/` | Driver-neutral lifecycle and evidence contracts |
| TG: production runtime | `services/<component>/`, `workers/<profile>/` when implemented | Explicit role interfaces, native builds, worker profiles, and shared conformance |
| TG-S1/S2/S3 | `experiments/`, replay tests, architecture docs | Spike reports and port/ownership recommendations |
| Final demo | `deploy/final-demo/`, runbooks | ATG public interfaces only |

## Change Rule

Cross-workstream changes update the contract or port first, add compatibility
tests, identify affected runtime responsibilities and maintainers, and link the
Jira dependency. Do not import another workstream's internal implementation to
avoid defining an interface.
