# Epic Technical Guidance

Date: Oct 27, 2026

## Purpose

This document translates the proposed AI Test Generation ADR into technical guidance for upcoming Epics. It identifies work that can begin before a formal implementation research spike, proposes contracts that preserve optionality, and narrows the harness candidates that the spike should evaluate.

This is implementation guidance, not an additional ADR. Recommendations about specific frameworks remain provisional until the spike verifies licensing, operational fit, maintenance, security, and Lightwell integration constraints.

## Architectural Starting Point

The Epics should assume the following established boundaries:

- The service is driver-agnostic, but vulnerability prioritization is the initial driver.
- The user-facing lifecycle is `Analyze -> Plan -> Code -> Execute`.
- Model calls assist bounded semantic tasks; they do not own authoritative evidence, workflow policy, or execution classification.
- Deterministic code owns schema validation, evidence lookup, repository inspection, command selection, policy enforcement, execution, and result normalization wherever practical.
- Generated tests use the repository's native framework.
- Repository content, external descriptions, and generated code are untrusted inputs.
- Test execution occurs outside the controller on approved ephemeral workers.
- The service produces tests and evidence. It does not modify production code, merge changes, publish releases, or rewrite upstream vulnerability records.
- Blocked, inconclusive, not-reached, and stated-gap outcomes are successful workflow outcomes when they accurately represent the evidence.

These constraints are more important than the choice of agent framework.

## Updated Business-Case Planning Context

The updated AI Test Harness Blueprint Business Case adds implementation direction that should shape Epic planning without changing the static ADR. Some positions are strong planning assumptions; others still require sponsor or cross-ADR alignment.

| Business-case position | Planning treatment | Epic implication |
|---|---|---|
| Red Hat OpenShift AI is named as the landing BU for model serving and the agent runtime | Preferred control-plane landing area, subject to sponsor confirmation and runtime capability validation | Design deployment, identity, model access, and observability for RHOAI first, while keeping domain contracts runtime-neutral |
| Trusted Software Supply Chain is named as a landing BU for the evidence chain | Preferred evidence/signing integration direction, not final record ownership | Keep `EvidencePublisher`, artifact, signing, and release-gate contracts explicit |
| The harness runs where the customer's pipeline runs | Established portability requirement for build/test execution | Separate the agent control plane from pluggable Jenkins, Tekton, GitHub Actions, or other execution providers |
| The initial path is human-reviewed | Established Phase 1 behavior | Use event-driven checkpoints; do not hold execution workers while waiting for approval |
| Generated tests enter through a pull request | Established publication boundary | Support an update to an existing remediation PR and a separate test-only PR as policy-selected publication modes; neither mode merges automatically |
| Candidates run on both sides of a change | Required where a valid base/candidate pair exists | Treat base/candidate identity and differential execution as first-class contracts, not optional narrative |
| Signed records are verified at a release gate | Target evidence outcome | Make content identity, attestation, and verification results part of the evidence Epic |
| Every reached behavior is validated, handed off, or a stated gap | Established completeness rule | Require a coverage/disposition map in Plan and final evidence |
| Failure-register entries run at the start of every release/reference job | Harness release requirement | Start failure-register and golden-reference work with the first Epic, not after implementation |
| Risk controls model budget and average inference targets | Pilot hypothesis, not yet a hard architectural gate | Capture risk/budget inputs and measurements; baseline before enforcing the 15-second and $0.05 targets |
| First-party PRs become unattended in a later phase | Future operating mode | Keep unattended operation out of the initial Epic acceptance criteria; require a later policy decision |
| CVE-targeted tests, VEX drafts, and stronger supply-chain attestations follow direct-dependency support | Later milestone | Preserve extension points but do not make VEX publication or SLSA claims part of the first vertical slice |

### Planning conflicts to resolve

The current planning sources are not fully aligned:

- The Blueprint Business Case names Python and Go pilot projects; the Secure Software Supply Chain SDLC ADR limits Phase 1 to Java and Python.
- The AI Test Generation ADR prioritizes vulnerability evidence as the initial driver; the Blueprint pilot also describes general first-party pull requests.
- The Blueprint describes a platform-agnostic harness, while also naming RHOAI as the model-serving and agent-runtime landing BU.
- The Secure Software Supply Chain flow updates an existing remediation PR; other documentation describes a test-only PR as the standard delivery path.

These are resolvable planning choices rather than architecture contradictions. Before committing language-adapter and SCM-publication Epics, confirm:

1. The first two pilot repositories and therefore the first native framework adapters.
2. Whether the first demonstration begins with a vulnerability/remediation PR, a general PR, or one example of each.
3. Whether RHOAI is a required deployment target for the pilot or the preferred production landing area after a portable local spike.
4. Whether generated tests update the existing remediation PR, open a linked test-only PR, or support both under repository policy.

### Recommended MVP slice

Unless stakeholders select otherwise, use the overlap between the documents as the first end-to-end planning scenario:

```text
Actionable vulnerability evidence
  -> remediation PR with base and candidate revisions
  -> explicit /generate-tests trigger
  -> RHOAI-hosted or locally equivalent agent control plane
  -> evidence-backed native regression/fix-pinning test
  -> execution in the repository's existing pipeline environment
  -> base/candidate differential result
  -> signed review evidence attached to the human-reviewed PR
```

This slice exercises the prioritized vulnerability driver, PR workflow, differential requirement, evidence chain, agent runtime, and customer-pipeline portability without requiring autonomous remediation or merge.

Where the Blueprint Business Case describes resolving dependencies, looking up advisories, and scoring risk, the Epic plan should preserve the ADR boundary: Vulnerability Prioritization or another authoritative source supplies applicability and reachability evidence. AI Test Generation may apply a test-depth or model-budget policy to that evidence, but should not create a second vulnerability scanner or authoritative risk score.

## Recommended Harness Strategy

Do not select one framework to own orchestration, model access, repository tools, execution, evaluation, and persistence. Separate the system into replaceable layers:

```text
Lightwell or API trigger
  -> AI Test Generation control plane
       -> RHOAI model serving and candidate agent runtime
       -> durable workflow/controller
       -> typed model-task interface
       -> domain agents and deterministic gates
  -> customer pipeline execution plane
       -> framework and execution adapters
       -> ephemeral build/test workers
  -> trusted evidence and artifact interfaces
```

The control plane plans and evaluates; the execution plane runs untrusted repository code. Neither RHOAI nor the workflow controller should need Jenkins-, Tekton-, or GitHub-specific domain logic.

### Recommended shortlist

| Layer | Leading candidate | Alternative | Guidance before the spike |
|---|---|---|---|
| Deployment/model runtime | RHOAI model serving and supported agent runtime | Portable container plus provider gateway | Treat RHOAI as the preferred integration target, not as a domain-contract dependency |
| Stage orchestration | LangGraph | Explicit application state machine | Prototype only behind a `WorkflowController` interface |
| Durable distributed workflow | Lightwell-native workflow, if available | Temporal | Do not introduce Temporal until Lightwell durability and callback capabilities are known |
| Typed model tasks | RHOAI-served models through `ModelTaskRunner`; Pydantic AI or direct SDK implementation | LangChain model/tool abstractions | Keep serving endpoint, model, and SDK choices outside domain contracts |
| Domain test-generation harness | Adapt selected `ai-test-harness` components | Implement equivalent gates; learn from `fullsend-ai/test-creator` | Evaluate by capability, not as an all-or-nothing adoption |
| Evaluation | Internal golden-reference runner plus native test tools | Pydantic Evals or LangSmith as optional accelerators | Keep benchmark definitions and pass/fail policy in service-owned data |
| Observability | OpenTelemetry | Framework-specific tracing exported to OpenTelemetry | Traces must remain portable and redact prompts, source, credentials, and vulnerability data by policy |

For the spike, use two deliberate implementations of the same thin slice and deploy the candidate to an RHOAI-compatible environment when one is available:

- **Control:** explicit application state machine, direct provider SDK behind `ModelTaskRunner`, and fake/local execution provider.
- **Candidate:** LangGraph for stage routing and Pydantic AI for typed model tasks, using the same domain contracts and fake/local execution provider.

This comparison establishes whether the frameworks reduce meaningful lifecycle code without becoming the source of domain state. It also separates model-serving/runtime integration from framework selection. Defer Temporal and other distributed workflow infrastructure until the Lightwell workflow boundary is understood.

### LangGraph

LangGraph is the strongest agent-orchestration candidate because its documented strengths match the lifecycle: explicit graphs, persistence, interrupts, human-in-the-loop checkpoints, and a mixture of deterministic and model-driven nodes. It is appropriate for routing between Analyze, Plan, Code, Execute, bounded repair, and review.

Use it only as an internal controller implementation. Domain state must use service-owned schemas, and externally visible checkpoint IDs and statuses must not be LangGraph-specific. A Lightwell or Temporal workflow should be able to invoke the same stage services later.

The spike should verify:

- Resume behavior across process restarts and schema changes.
- Idempotent re-entry after callbacks, retries, and duplicate messages.
- Human interrupt and revision semantics.
- Whether checkpoint storage satisfies tenancy, retention, and encryption requirements.
- Whether model/tool traces can be redacted and exported through OpenTelemetry.
- The operational burden of running LangGraph persistence separately from Lightwell workflow state.
- Packaging, deployment, identity, and scaling compatibility with the supported RHOAI agent-runtime environment.

### Pydantic AI

Pydantic AI is a strong candidate for individual model tasks because it provides typed dependencies, structured outputs, tool validation, model-provider abstraction, testing support, and OpenTelemetry-compatible instrumentation. It is a better fit for `AnalyzeRepository`, `ProposeTestPlan`, and `GenerateNativeTests` than for owning the entire product lifecycle.

Use a service-owned `ModelTaskRunner` interface even if Pydantic AI is selected. Model identifiers, provider settings, prompt versions, budgets, and output schemas belong in service configuration and provenance, not in domain objects.

The spike should compare Pydantic AI with direct provider SDK calls on:

- Structured-output conformance and repair behavior.
- Model/provider portability.
- Tool-call authorization and argument validation.
- Token, latency, and cost accounting.
- Offline testing with deterministic fake models.
- Prompt and trace redaction.
- Dependency footprint and release stability.
- Authentication, model discovery, structured-output behavior, and cost/usage reporting through RHOAI model-serving endpoints.

### Temporal

Temporal is a durable workflow platform, not an agent harness. It is appropriate if the service must survive long waits, worker restarts, asynchronous CI callbacks, human approvals, and multi-day runs and Lightwell does not already provide those capabilities.

It should not be selected merely because the flow is long-running. First determine whether Lightwell owns the outer workflow. A reasonable future composition is:

```text
Temporal or Lightwell workflow
  -> invoke Analyze stage
  -> wait for approval
  -> invoke Plan stage
  -> dispatch execution job
  -> wait for callback
  -> publish evidence
```

LangGraph or a typed agent runner may still operate inside a stage, but only one system should own retries, timers, cancellation, and externally visible lifecycle state.

### Existing test-generation work

`ai-test-harness` is the leading domain-harness candidate because its adapters, differential testing, failure register, provenance, mutation checks, and self-verification align with this service. Evaluate reuse at component boundaries:

- Framework-adapter interface.
- Differential environment construction.
- Assertion and exact-content gates.
- Failure-register self-checks.
- Result normalization and provenance.

`fullsend-ai/test-creator` is most relevant to Analyze and Plan: parallel analyzers, deterministic evidence gates, citation checks, gaps as first-class output, and review workflows. Its strategy/Jira coupling and E2E focus make wholesale adoption less likely, but its deterministic gate pattern is directly reusable or reproducible.

CyberGym, SWT-Bench, Defects4J, BugsInPy, and similar tools remain implementation-validation harnesses. They should not become production dependencies unless a later, explicit product requirement selects one as a runtime adapter.

### Harnesses not recommended as the initial foundation

- Multi-agent-first frameworks such as CrewAI or AutoGen add coordination semantics the initial scope does not require. Parallel analysis can be ordinary tasks with typed outputs.
- Provider-specific agent SDKs should not define public contracts. They may be used behind `ModelTaskRunner` if they materially outperform a provider-neutral implementation.
- A general autonomous coding harness should not receive unrestricted shell, network, or repository-write access. Test generation needs a smaller tool surface and policy-controlled file changes.
- MCP may standardize selected tool access later, but it is not required for internal function calls and does not replace authorization, tenancy, or evidence contracts.

## Contract Design Principles

Contracts should be versioned, immutable where possible, and independent of framework classes. Use JSON Schema or an equivalent IDL as the interchange definition and generate language bindings where useful.

Every externally exchanged object should include:

- `schema_version` and stable object `id`.
- `run_id`, `correlation_id`, and an idempotency key where commands can be retried.
- Tenant or authorization-context reference, without embedding credentials.
- Creation time and producer identity.
- Source revision or artifact digest when the object is repository-specific.
- URI plus digest for large artifacts; do not place source archives, full logs, or patches in workflow state.
- Explicit limitations and completeness status.
- Extension fields only in a namespaced location; unknown required fields must fail validation.

Prefer additive schema evolution. Breaking semantic changes require a new major schema version and conversion at the boundary, not conditional interpretation throughout the service.

## Phase Contract Sketches

The following sketches describe the likely data crossing each phase boundary. They are intended to expose missing concepts and support Epic creation; they do not prescribe REST, events, in-process calls, storage technology, or final field names.

### Common phase result

Every phase should return a consistent result envelope even when its primary output differs:

```text
PhaseResult<T>
  phase
  phase_version
  run_id
  status: completed | partial | awaiting_decision | blocked | failed | cancelled
  input_refs[]
  output: T | null
  artifact_refs[]
  evidence_refs[]
  findings[]
  limitations[]
  next_allowed_actions[]
  started_at
  completed_at
```

`failed` means the phase itself malfunctioned or violated its contract. `blocked` and `partial` are valid domain outcomes. For example, unavailable component infrastructure is a feasibility constraint, not a service failure.

### Pipeline summary

| Phase | Primary inputs | Primary outputs | Next consumer |
|---|---|---|---|
| Vulnerability prioritization input adapter | `TestGenerationRequest`, `VulnerabilityDriverPayload`, authorization and policy references | `IntakeResult`, `EvidenceBundle`, normalized driver context | Repository/context analysis |
| Repository/context analysis | Accepted intake, repository target, evidence bundle, repository access | `ContextAnalysisResult`, `RepositoryProfile`, objective candidates, analysis checkpoint | Feasibility assessment |
| Feasibility assessment | Repository profile, objective candidates, policies, execution-provider capabilities | `FeasibilityAssessment`, candidate validation strategies, required decisions | Test planning |
| Test planning and user checkpoint | Evidence, context analysis, feasibility, objective candidates, prior feedback | `TestPlan`, `StageCheckpoint`, stated gaps, approval request | Native framework generation after approval |
| Native framework generation | Approved plan/checkpoint digests, source snapshot, repository profile, adapter capability | `GenerationResult`, exact candidate patch, generation findings | Isolated execution |
| Isolated execution | `ExecutionRequest`, exact source/patch/image identities, approved command and sandbox profiles | `ExecutionResult`, raw and normalized artifact references | Quality and differential gates |
| Quality/differential gates | Objectives, plan, generated patch, execution results, deterministic policies | `QualityAssessment`, `GateEvaluation`, repair/review route | Generation, planning, or evidence assembly |
| Provenance and review evidence | All phase manifests, decisions, artifacts, gate outcomes, model/tool records | `EvidenceManifest`, `ReviewPacket`, optional publication/PR request | Lightwell and human reviewers |

### Likely service versus internal boundaries

A pipeline phase should not automatically become a separately deployed service. The initial design can keep semantic stages inside one AI Test Generation service while preserving contracts at the boundaries most likely to be remote:

| Boundary | Likely interaction | Contract family |
|---|---|---|
| Lightwell -> AI Test Generation | Synchronous acceptance plus asynchronous lifecycle updates | `TestGenerationRequest`, request disposition, checkpoint/status events |
| AI Test Generation -> Vulnerability Prioritization/Lightwell evidence | Authorized reference resolution | `EvidenceReference`, `EvidenceBundle`, resolution findings |
| AI Test Generation -> source control/source provider | Read immutable revision; later propose test-only change | source snapshot reference, `ChangeProposal`, publication result |
| Controller -> execution environment | Asynchronous job and callback/event | `ExecutionRequest`, execution status event, `ExecutionResult` |
| All phases -> artifact store | Content-addressed write/read/verify | `ArtifactReference`, digest, metadata, retention classification |
| AI Test Generation -> Lightwell evidence/review workflow | Publish evidence manifest and review state | `EvidenceManifest`, `ReviewPacket`, publication result |

Repository analysis, feasibility, planning, generation, and quality evaluation can initially be internal modules with versioned typed objects. They should be split into network services only when scaling, security isolation, ownership, or deployment cadence requires it.

### 1. Vulnerability prioritization input adapter

**Responsibility:** Validate the invocation, resolve authorized upstream references, and translate the Vulnerability Prioritization vocabulary into a stable driver contract. It does not reinterpret upstream vulnerability conclusions or plan tests.

**Inputs:**

- `TestGenerationRequest` with repository/artifact target and requested lifecycle capabilities.
- `VulnerabilityDriverPayload` containing advisory, component, applicability, symbol, reachability, path-witness, build-context, and limitation references.
- Caller identity, tenant, authorization context, and data-classification context.
- Input-adapter and service policy versions.
- Optional callback or workflow correlation reference.

**Outputs:**

- `IntakeResult` with `accepted`, `accepted_with_gaps`, `rejected`, or `unsupported` disposition.
- `EvidenceBundle` containing verified references, issuers, digests, subject identities, completeness, and upstream limitations.
- Normalized `DriverContext` containing driver type/version, relevant package/component identity, requested validation roles, and comparison hints.
- Input findings such as missing build context, stale evidence, mismatched repository revision, unresolved package identity, or insufficient authorization.
- An intake checkpoint and stable run ID when accepted.

**Handoff rules:**

- The adapter must preserve upstream evidence IDs and wording; normalization cannot silently strengthen certainty.
- Referenced evidence must resolve to the same subject revision or produce a conflict finding.
- Unsupported driver versions and unauthorized references fail before repository access.
- Missing optional evidence can produce `accepted_with_gaps`; missing identity or authorization produces `rejected` or `blocked`.

### 2. Repository/context analysis

**Responsibility:** Establish what repository and build/test context is actually available at the requested revision. Combine caller-supplied context with read-only discovery while keeping their provenance distinct.

**Inputs:**

- Accepted `IntakeResult`, `DriverContext`, and `EvidenceBundle`.
- Repository URI, immutable source revision, optional comparison revision, and artifact references.
- Read-only repository credential reference and repository-access policy.
- Optional caller-supplied repository profile, build context, deployment profile, existing coverage, or test inventory.
- Registered `FrameworkAdapter` discovery capabilities.

**Outputs:**

- `RepositoryProfile` with languages, frameworks, test locations, representative tests, fixtures, approved commands, result formats, services, and safety constraints.
- `ContextAnalysisResult` containing discovered facts, supplied facts, conflicts, and source/evidence references for each important fact.
- Existing-test and coverage inventory relevant to the affected component, symbols, paths, or interfaces.
- `ObjectiveCandidate[]` mapping the driver evidence to possible application behaviors and test surfaces without yet selecting scenarios.
- `AnalysisFinding[]`, including unmapped symbols, absent source, unavailable generated code, ambiguous call sites, unsupported framework, or conflicting commands.
- Analyze `StageCheckpoint` with assumptions and questions for review.

**Handoff rules:**

- Repository content can describe commands but cannot authorize them. Discovered commands remain candidates until onboarding or policy approves them.
- The analysis output must distinguish observed facts, supplied assertions, model inferences, and unresolved conflicts.
- The profile is bound to the source revision and declares invalidation inputs.
- No objective candidate may cite a symbol, call site, or existing test that lacks an evidence or source reference.

### 3. Feasibility assessment

**Responsibility:** Determine which validation strategies are technically possible and permitted for each objective candidate in the current context.

**Inputs:**

- `RepositoryProfile`, `ContextAnalysisResult`, `ObjectiveCandidate[]`, and `EvidenceBundle`.
- Registered framework-adapter capabilities.
- `ExecutionProvider` capability declaration: supported architectures, sandbox profiles, services, networking, credentials, resource ceilings, and artifact formats.
- Safety and authorization policy.
- Optional cost, time, or runner budget.

**Outputs:**

- `FeasibilityAssessment` with a per-objective, per-validation-strategy status rather than one global level.
- `ValidationStrategyCandidate[]`, such as static-only, library test, application call-site test, component test, differential test, or deployment-shaped simulation.
- Required environment and service dependencies for each viable strategy.
- Blocking constraints, estimated cost class, and evidence needed to remove each block.
- `RequiredDecision[]` for choices requiring human authorization or missing expected behavior.
- Explicit `StatedGap[]` where no strategy is currently viable.

**Handoff rules:**

- `available` means both technically supported and permitted; technical capability without authorization is `blocked`.
- Feasibility must not convert missing runtime context into a static pass.
- Planning may select only an available strategy or explicitly create a handoff/gap.
- Estimates are planning inputs, not execution evidence.

### 4. Test planning and user checkpoint

**Responsibility:** Convert evidence-backed objectives and feasible strategies into reviewable scenarios with explicit setup, action, oracle, cleanup, exclusions, and limitations.

**Inputs:**

- `EvidenceBundle`, `ContextAnalysisResult`, `RepositoryProfile`, `ObjectiveCandidate[]`, and `FeasibilityAssessment`.
- Existing-test inventory and duplicate candidates.
- Oracle-source precedence and test-quality policy.
- Prior `FeedbackDecision[]` when revising a plan.
- Model-task configuration and budget for semantic planning.

**Outputs:**

- Versioned `TestObjective[]` selected from or refined from the objective candidates.
- `TestPlan` with scenarios, test levels, framework, setup, action, oracle, boundaries, mock policy, cleanup, comparison semantics, and evidence citations.
- `CoverageMap` from upstream objective/evidence to planned scenario, team handoff, or stated gap.
- `StatedGap[]` and `RequiredDecision[]` for unsupported, unsafe, ambiguous, or environment-specific cases.
- Plan `StageCheckpoint` with the plan artifact digest and allowed decisions.
- Human-facing plan summary and `FeedbackDecision` request.

**Handoff rules:**

- Generation accepts only an approved checkpoint whose decision binds the exact plan digest.
- Every in-scope objective appears in the coverage map; omission is not an allowed state.
- Every oracle cites an attributable source or is explicitly labeled characterization.
- A model can propose scenarios but cannot approve its own plan, authorize destructive actions, or resolve conflicting authoritative sources.

### 5. Native framework generation

**Responsibility:** Produce the smallest test-only patch implementing the approved scenarios in the repository's native framework and perform non-executing or low-risk prechecks.

**Inputs:**

- Approved plan checkpoint ID, decision ID, `TestPlan` URI, and exact plan digest.
- Immutable source snapshot and repository/profile revision.
- Selected `FrameworkAdapter` and capability version.
- Bounded repository context: representative tests, fixtures, helpers, manifests, and relevant source paths.
- Generation policy, allowed file paths, model-task configuration, and attempt budget.

**Outputs:**

- `GenerationResult` and content-addressed test-only patch.
- Added/modified file manifest and scenario-to-test mapping.
- Static precheck results: output-schema validation, forbidden-path checks, parse/format/lint status, assertion findings, and optionally compilation or collection where safely available.
- `ModelInvocationRecord[]` and tool activity references.
- Generation findings, unresolved questions, and repair history.
- Code `StageCheckpoint` when review or correction is required.

**Handoff rules:**

- The patch may change only approved test/fixture paths unless an explicit decision expands scope.
- The generation result must bind source, profile, plan, adapter, prompt, model, and patch digests.
- Repairs cannot weaken assertions, add skips/retries, alter the oracle, or edit production code without routing to review or Plan.
- Execution receives the exact generated patch digest; regenerated text requires a new generation revision.

### 6. Isolated execution

**Responsibility:** Execute the exact candidate patch in an approved ephemeral environment and return normalized observations without asking a model to determine whether commands succeeded.

**Inputs:**

- `ExecutionRequest` containing exact repository, source revision, patch, and optional comparison identities.
- Approved command IDs and required command sequence from `RepositoryProfile` and the selected adapter.
- Sandbox profile, runner image digest, architecture, resource limits, timeout, egress rules, and short-lived credential references.
- Required service/environment specification.
- Idempotency key, cancellation token, callback reference, and requested artifact list.

**Outputs:**

- `ExecutionResult` for focused test, related suite, and any approved differential/stability/mutation run.
- Per-environment observations for base/vulnerable and candidate/fixed contexts; do not collapse them prematurely.
- Normalized collection, pass, fail, skip, timeout, and infrastructure status.
- JUnit, logs, coverage, mutation, stability, command transcript, and environment-manifest references.
- Worker, image, command, source, patch, policy, and credential-lease identities.
- Execution limitations, cleanup outcome, and attestation/signature reference when required.

**Handoff rules:**

- The worker rejects any digest or command mismatch before execution.
- Setup failure, policy block, timeout, cancellation, and test assertion failure are distinct outcomes.
- Duplicate submissions or callbacks with the same idempotency key must not create conflicting results.
- Raw logs and artifacts are stored externally; workflow state retains bounded manifests and references.

### 7. Quality, differential, and golden-reference gates

**Responsibility:** Evaluate whether the generated test is valid, relevant, discriminating, reproducible, and supported by the recorded evidence. This phase contains two related but distinct modes.

**Per-run inputs:**

- Approved `TestObjective[]`, `TestPlan`, coverage map, and plan/checkpoint digests.
- `GenerationResult`, exact patch, assertion/static findings, and repair history.
- `ExecutionResult[]` and normalized artifacts.
- Deterministic gate policy and optional model-review policy.

**Per-run outputs:**

- `QualityAssessment` with separate deterministic, behavioral, differential, stability, mutation, and model-assisted findings.
- `GateEvaluation[]` with `pass`, `fail`, `blocked`, `inconclusive`, or `not_applicable` outcome and supporting references.
- Objective-to-observed-behavior mapping and residual limitations.
- Route decision: accept for review, return to Code, return to Plan, return to Analyze, request human classification, or record a stated gap.

**Release-evaluation inputs:**

- Versioned golden-reference/failure-register dataset and held-out evaluation subset.
- Candidate model, prompt, adapter, gate, or harness version.
- Baseline evaluation result and equivalence/improvement policy.

**Release-evaluation outputs:**

- `EvaluationRunResult` with per-case structural, behavioral, evidence, cost, and latency results.
- Regression comparison and release recommendation.
- Newly discovered failure-register candidates.

**Handoff rules:**

- Golden-reference evaluation is normally a service-development/release gate, not something run against every customer request.
- A model review may add findings or lower confidence; it cannot override deterministic failures or upgrade runtime evidence.
- Differential classification must retain both observed sides and the expected role of each environment.
- Gate failure is not automatically a product defect; the route identifies whether the plan, generated test, environment, or implementation is implicated.

### 8. Provenance and review evidence

**Responsibility:** Assemble a bounded, reviewable account of what was requested, generated, executed, observed, decided, and left unresolved, then publish it through approved Lightwell and repository workflows.

**Inputs:**

- Request/intake manifest and upstream `EvidenceReference[]`.
- Repository/context analysis, feasibility, objectives, plan, and coverage map.
- All checkpoint and authorized feedback decisions.
- Generation, execution, quality, and optional release-evaluation manifests.
- `ModelInvocationRecord[]`, tool/version records, artifact digests, and policy versions.
- Publication, retention, redaction, signing, and reviewer-routing policy.

**Outputs:**

- Content-addressed `EvidenceManifest` linking all phase inputs, outputs, decisions, artifacts, supersession relationships, and limitations.
- Concise `ReviewPacket` describing what was requested, what each test proves, exact execution results, mocks/real dependencies, unresolved gaps, and proposed next action.
- Optional signed attestation reference.
- `EvidencePublicationResult` containing Lightwell or approved evidence-store references.
- Optional test-only `ChangeProposal` or PR request bound to the exact reviewed patch digest.
- Final review checkpoint and lifecycle event: proposed, approved, rejected, superseded, or retained as a gap.

**Handoff rules:**

- Evidence publication cannot change an upstream vulnerability conclusion; it links scoped downstream test evidence.
- Review evidence must identify what was not tested and why.
- The proposed patch must match the generated, executed, gated, and reviewed digest.
- Publication failures do not rewrite successful execution results; they produce a separate publication status and retry path.

### Feedback and reverse transitions

The pipeline is not strictly one-way. Typed outcomes route back to the phase that owns the invalid assumption:

| Finding | Owning phase | Typical transition |
|---|---|---|
| Evidence reference stale or subject mismatch | Input adapter | Reject or request refreshed evidence |
| Repository convention or required service missing | Repository/context analysis | Revise profile or record gap |
| Validation strategy unavailable or unauthorized | Feasibility | Select lower-fidelity strategy, request authorization, or record gap |
| Wrong scope, duplicate scenario, or unsupported oracle | Planning | Revise plan and obtain new approval |
| Syntax, fixture, import, or framework-use error | Generation | Bounded Code repair |
| Environment setup, runner, or service failure | Execution/Analyze | Retry infrastructure or revise environment context |
| Test reaches target but asserts wrong behavior | Planning | Revise oracle; do not weaken assertion in Code |
| Test does not exercise the stated objective | Planning or generation | Revise scenario or implementation |
| Exact patch/provenance mismatch | Generation/execution boundary | Reject and regenerate execution request |

Every reverse transition creates a new checkpoint or artifact revision and preserves the superseded record.

## Proposed Core Contracts

### 1. `TestGenerationRequest`

The request envelope remains stable while drivers provide typed payloads:

```json
{
  "schema_version": "1.0",
  "request_id": "tgr-123",
  "correlation_id": "lightwell-run-456",
  "driver": {
    "type": "vulnerability",
    "version": "1.0",
    "payload_ref": "evidence://requests/tgr-123/vulnerability.json"
  },
  "target": {
    "repository": "https://example.invalid/org/repo",
    "revision": "sha256-or-git-sha",
    "comparison_revision": null,
    "artifact_refs": []
  },
  "requested_capabilities": ["analyze", "plan", "generate", "execute"],
  "policy_ref": "policy://atg/default-v1",
  "callback_ref": "callback://lightwell/runs/456",
  "idempotency_key": "lightwell-run-456:v1"
}
```

The first release may accept only `driver.type = vulnerability`, but the enum and dispatch path must permit later registered drivers. Unsupported drivers return a typed `unsupported_driver` result rather than falling through to a generic prompt.

### 2. `VulnerabilityDriverPayload`

The initial payload should reference upstream evidence rather than copy its conclusions:

```json
{
  "schema_version": "1.0",
  "advisory_refs": ["lightwell://advisories/CVE-YYYY-NNNN"],
  "component_refs": ["lightwell://sbom/components/abc"],
  "applicability_evidence_refs": [],
  "affected_symbol_evidence_refs": [],
  "reachability_evidence_refs": [],
  "path_witness_refs": [],
  "build_context_ref": "lightwell://build-contexts/def",
  "upstream_limitations": []
}
```

References must carry or resolve to issuer, revision, digest, and authorization metadata. The service can derive test objectives from this evidence but cannot change the upstream records.

Future drivers should implement the same logical interface:

```text
validate(payload) -> validation findings
resolve_evidence(payload) -> EvidenceBundle
derive_objectives(evidence) -> TestObjective[]
```

### 3. `EvidenceReference` and `EvidenceBundle`

Use a common reference rather than embedding source-specific objects:

```json
{
  "id": "ev-123",
  "kind": "reachability_path",
  "uri": "lightwell://evidence/ev-123",
  "digest": "sha256:...",
  "issuer": "vulnerability-prioritization",
  "observed_at": "2026-10-23T00:00:00Z",
  "subject_refs": ["repo://org/repo@sha"],
  "classification": "internal",
  "completeness": "partial",
  "limitations": ["generated sources unavailable"]
}
```

`EvidenceBundle` is a manifest of references plus consistency findings. It must not duplicate full evidence payloads in checkpoint state.

### 4. `RepositoryProfile`

The profile records discovered facts and their sources:

- Repository and revision.
- Languages and native test adapters.
- Approved build, format, collection, focused-test, and suite commands.
- Representative test and fixture references.
- Required services and execution capabilities.
- Network, credential, destructive-operation, and artifact policies.
- Discovery confidence and unresolved conflicts.
- Profile invalidation inputs, such as changed manifests or test configuration.

Commands are data approved by policy or repository onboarding. The model may nominate a command, but it cannot authorize arbitrary shell execution.

### 5. `FeasibilityAssessment`

Analyze should emit capability decisions independently of the model narrative:

```json
{
  "levels": {
    "static_only": "available",
    "library_testable": "available",
    "call_site_testable": "blocked",
    "component_testable": "unavailable",
    "deployment_simulatable": "not_requested"
  },
  "constraints": [],
  "required_decisions": [],
  "evidence_refs": [],
  "status": "partial"
}
```

Use a status per capability instead of one ascending level because a repository may support component execution but not a particular library fixture.

### 6. `TestObjective` and `TestPlan`

`TestObjective` bridges driver-specific evidence and driver-neutral planning:

- Objective ID and driver source.
- Behavior statement.
- Evidence references.
- Target symbols, paths, component, or interface where known.
- Requested validation role: exposure, fix-pinning, regression, characterization, compatibility, or gap analysis.
- Required comparison semantics.
- Known limitations and safety flags.

Each `TestPlan` scenario should contain requirement/objective IDs, test level, setup, action, oracle, boundaries, mock policy, cleanup, existing-test exclusions, required environment, destructive flag, and evidence supporting the oracle. Plan approval must bind to the plan digest so later generation cannot silently use a revised plan.

### 7. `StageCheckpoint`

The checkpoint is a service-domain object, not a serialized framework graph:

```json
{
  "checkpoint_id": "cp-123",
  "run_id": "run-123",
  "stage": "plan",
  "stage_revision": 3,
  "status": "awaiting_review",
  "input_refs": [],
  "artifact_refs": [],
  "decision_refs": [],
  "assumptions": [],
  "open_questions": [],
  "attempts_used": 1,
  "next_allowed_actions": ["approve", "revise", "reject"],
  "supersedes": "cp-122"
}
```

Checkpoints are append-only. A revision supersedes an earlier checkpoint; it does not mutate the evidence that supported the earlier decision.

### 8. `FeedbackDecision`

Feedback must be typed and attributable:

- `approve`, `revise`, `reject`, `classify`, `authorize`, or `cancel`.
- Target checkpoint and artifact digest.
- Scope such as `expected_behavior`, `excluded_case`, `fixture_preference`, `safety_constraint`, or `failure_classification`.
- Author identity and authorization context.
- Free-form rationale as supporting text, not executable instruction.

Only an authorized decision can change policy or permit a destructive action. Repository text and peer/model suggestions cannot create authorization.

### 9. `GenerationResult`

Generation returns references and validation state, not just model text:

- Exact generated patch URI and digest.
- Files added or changed.
- Plan and checkpoint digest used.
- Framework adapter and version.
- Model/prompt invocation references.
- Parse, format, compile, or collection-precheck results.
- Unresolved generation findings.

The exact patch digest must be carried into execution and publication to prevent evaluating one version and proposing another.

### 10. `ExecutionRequest` and `ExecutionResult`

`ExecutionRequest` is an authorization-scoped command for an external worker:

- Repository and exact patch digests.
- Base/candidate artifact or revision identities.
- Approved command IDs from `RepositoryProfile`; no model-authored shell string.
- Sandbox profile, image digest, resources, timeout, egress policy, and credential references.
- Required artifacts and result callback.
- Idempotency key and cancellation token.

`ExecutionResult` separates infrastructure and behavioral outcomes:

- Worker, sandbox, image, command, source, and patch identities.
- Collection and test counts.
- Base and candidate results when differential.
- Normalized result state: `validated`, `reproduced`, `fixed_behavior`, `reached_not_reproduced`, `not_reached`, `blocked`, `environment_mismatch`, or `inconclusive`.
- Raw artifact references, coverage/mutation/stability references, and limitations.
- Attestation or signature reference when required.

### 11. `QualityAssessment`

Keep deterministic findings distinct from model-assisted review:

```text
QualityAssessment
  deterministic_findings[]
  model_review_findings[]
  behavioral_evidence_refs[]
  gate_decisions[]
  overall_status
```

A model can lower confidence, identify review candidates, or explain evidence. It cannot override a failed deterministic gate or upgrade an inconclusive execution result.

### 12. `ModelInvocationRecord`

Record model use independently of the chosen SDK:

- Task type, model/provider alias, and resolved model ID.
- Prompt-template ID and digest, policy version, and output-schema version.
- Input evidence and artifact digests, subject to redaction policy.
- Token counts, latency, cost, retry count, and termination reason.
- Tool names and argument/result digests.
- Structured-output validation findings.

Do not retain chain-of-thought. Retain attributable inputs, outputs, decisions, tool activity, and validation evidence needed for audit and reproduction.

## Adapter Interfaces

Define capability interfaces before choosing implementations:

```text
InputDriver
  validate(request)
  resolve_evidence(request)
  derive_objectives(evidence)

FrameworkAdapter
  discover(repository)
  plan_capabilities(profile)
  validate_generated(patch)
  focused_commands(plan, patch)
  normalize(raw_results)

ExecutionProvider
  assess(request)
  submit(execution_request)
  cancel(execution_id)
  get_result(execution_id)

ModelTaskRunner
  run(task_type, typed_input, output_schema, policy)

ArtifactStore
  put(stream, metadata)
  resolve(reference)
  verify(reference, digest)

EvidencePublisher
  publish(manifest)
  supersede(prior_ref, new_ref)

WorkflowController
  start(request)
  apply_decision(feedback)
  resume(checkpoint_id)
  cancel(run_id)
```

Adapters should declare capabilities rather than force callers to infer them. For example, a pytest adapter may support collection and coverage but not mutation; an execution provider may support restricted egress but not privileged containers.

## Decisions That Can Be Established Before the Spike

The following are framework-independent and can be accepted as Epic constraints now:

1. Public contracts use service-owned schemas and IDs, never framework state objects.
2. Workflow state stores artifact references and digests, not source archives, raw logs, secrets, or unbounded prompts.
3. Model tasks use typed inputs and outputs with explicit budgets and prompt/model provenance.
4. Deterministic gates cannot be overridden by model self-assessment.
5. The initial driver is vulnerability evidence; driver registration remains extensible.
6. The initial generation scope should be limited to explicitly selected native adapters. Unsupported repositories produce a stated gap.
7. Execution is asynchronous, isolated, idempotent, and external to the controller.
8. Commands and environments are selected from approved profiles; the model does not receive unrestricted shell authority.
9. Analyze and Plan checkpoints are human-reviewable by default. Unattended operation requires later policy approval.
10. Evidence and generated artifacts are content-addressed, and execution is bound to the exact generated patch.
11. Result states distinguish behavioral failure, setup failure, policy block, environment mismatch, non-reachability, and insufficient evidence.
12. OpenTelemetry is the portability boundary for traces and metrics, with service-defined redaction.
13. Golden-reference and held-out behavioral evaluation are release gates for model, prompt, adapter, and harness changes.
14. Test promotion occurs through a human-reviewed test-only PR or explicitly approved equivalent.
15. The agent/model control plane and untrusted repository execution plane are separate deployable concerns.
16. Execution-provider contracts remain independent of Jenkins, Tekton, GitHub Actions, or another customer pipeline implementation.
17. Human wait states persist checkpoints and release compute rather than holding an execution worker.
18. Model serving and agent runtime are accessed through service-owned interfaces; the available RHOAI path is evaluated first and does not define domain schemas.

## Decisions the Spike Must Inform

Do not settle these through Epic wording alone:

- LangGraph versus an explicit state machine for internal stage orchestration.
- Lightwell-native durability versus Temporal or another workflow platform.
- Pydantic AI versus direct provider SDKs for typed model tasks.
- Which `ai-test-harness` components can be reused, forked, or only reproduced.
- The first supported framework adapter and repository set.
- Artifact/evidence storage implementation and Lightwell publication mechanism.
- Sandbox provider and CI callback protocol.
- Model/provider selection and fallback policy.
- Prompt and source retention policy for sensitive vulnerability inputs.
- Quantitative cost, latency, acceptance, and false-positive gates.
- The RHOAI agent-runtime capability and deployment pattern to use, if any, beyond model serving.
- Python/Go versus Java/Python pilot repositories and first framework adapters.
- Vulnerability/remediation PR versus general PR as the first invocation scenario.
- Updating an existing remediation PR versus opening a linked test-only PR.

## Recommended Epic Decomposition

### Pre-Epic alignment gate

Before estimating the language-adapter and publication work, record the sponsor/stakeholder answers for pilot repositories, initial trigger scenario, RHOAI runtime availability, and PR publication mode. Contract and golden-reference work can begin while these decisions are pending, but adapter-specific commitments should not.

### Epic 1: Domain Contracts and Run State

Deliver:

- Versioned schemas for request, evidence references, objectives, plans, checkpoints, feedback, generation, execution, quality, and provenance.
- State-transition rules and idempotency behavior.
- Schema fixtures, compatibility tests, and invalid-input tests.
- Framework-neutral `WorkflowController` and `ModelTaskRunner` interfaces.
- Standard `/generate-tests` trigger/event payload and asynchronous status/callback contract.
- Explicit control-plane, execution-plane, and evidence-plane boundaries.

Exit condition: a complete synthetic run can be represented and replayed without selecting an LLM or orchestration framework.

### Epic 2: Vulnerability Driver and Evidence Intake

Deliver:

- `VulnerabilityDriverPayload` validation.
- Lightwell/Vulnerability Prioritization evidence-reference resolver stub or adapter.
- Evidence consistency and missing-context findings.
- Driver-neutral `TestObjective` derivation.

Exit condition: supplied evidence produces attributable objectives or explicit gaps without copying or rewriting upstream conclusions.

### Epic 3: Analyze and Repository Profile

Deliver:

- Read-only repository discovery.
- Approved command/profile model.
- Adapter capability detection.
- Feasibility assessment and invalidation rules.
- Analyze checkpoint and review packet.

Exit condition: one pilot repository produces a reproducible profile and a policy-grounded feasibility result without a model inventing commands or environment facts.

### Epic 4: Plan and Human Review

Deliver:

- Typed model-task prototype for scenario proposals.
- Deterministic evidence citation, oracle-source, duplicate-candidate, and safety gates.
- Versioned plan artifacts and feedback decisions.
- Approve/revise/reject/resume behavior.

Exit condition: an approved plan digest is traceable to objectives and evidence, and unsupported behaviors become stated gaps.

### Epic 5: Native Test Generation

Deliver:

- One stakeholder-selected framework adapter first; add the second pilot adapter only after the interface is proven.
- Bounded repository read and patch-write tool surface.
- Structured generation output and exact-patch provenance.
- Parse, format, compile, collection, and assertion-quality prechecks.
- Bounded repair with stop conditions.

Exit condition: generated tests conform to repository patterns and the exact accepted patch is content-addressed for execution.

### Epic 6: Isolated Execution and Result Normalization

Deliver:

- Execution provider contract and one adapter for the selected pilot pipeline, backed by an ephemeral worker.
- Focused and related-suite execution.
- Base/candidate differential mode where available.
- JUnit, coverage, log, mutation, and stability artifact normalization.
- Typed infrastructure versus behavioral result classification.

Exit condition: duplicate callbacks are safe, execution cannot access unapproved credentials or network destinations, and results bind to exact source/patch/image digests.

### Epic 7: Quality, Evaluation, and Release Gates

Deliver:

- Golden-reference dataset and failure-register cases.
- Deterministic gate engine and model-assisted review separation.
- Held-out evaluation runner, cost/latency accounting, and regression comparison.
- OpenTelemetry traces and redaction tests.
- Evaluation report suitable for selecting the implementation harness.

Exit condition: model, prompt, adapter, or harness changes cannot be promoted without comparable behavioral and evidence results.

### Epic 8: Evidence Publication and PR Integration

Deliver:

- Evidence manifest and provenance publication.
- Review summary constrained for human readability.
- Policy-selected existing-PR update or linked test-only PR publisher with exact-patch verification.
- Signed evidence/attestation output and release-gate verification result.
- Approval, rejection, supersession, and retention events.

Exit condition: a reviewer can trace every proposed test to its objective, evidence, executed content, results, limitations, and model/tool identities.

### Epic dependencies

```text
Epic 1: contracts and run state
  -> Epic 2: vulnerability evidence intake
  -> Epic 3: repository analysis
       -> Epic 4: plan and review
            -> Epic 5: native generation
                 -> Epic 6: isolated execution
                      -> Epic 8: evidence and PR publication

Epic 7: evaluation and release gates
  starts with Epic 1 fixtures and expands alongside Epics 2-6
```

Epic 7 is not a final hardening phase. Its golden references, failure cases, and measurements should be added as each capability is introduced.

## Cross-Epic Acceptance Guidance

Every Epic should include:

- Contract tests using versioned fixtures.
- Negative tests for missing, stale, conflicting, and unauthorized inputs.
- Idempotency and retry behavior.
- Explicit blocked and inconclusive acceptance cases.
- Authorization and tenant-boundary tests where applicable.
- Artifact digest verification.
- Audit events without secret or sensitive-source leakage.
- Failure-register entries for discovered harness defects.
- Measured latency, model usage, runner time, and artifact volume.

Avoid acceptance criteria such as "the agent successfully analyzes the repository" or "the test is high quality." Require observable artifacts and gates: schema-valid profile, cited objective, approved plan digest, collecting test, base/candidate result, exact patch digest, or explicit stated gap.

## Research Spike Shape

The spike should implement one thin vertical slice rather than isolated framework demos:

```text
Vulnerability request fixture
  -> remediation PR fixture with base/candidate revisions
  -> /generate-tests event
  -> repository profile
  -> one evidence-backed test objective
  -> one approved test plan
  -> one native generated test
  -> isolated focused execution through a pipeline adapter
  -> base/candidate differential result
  -> signed normalized evidence packet attached to the PR
```

Run the same slice through the shortlisted harness combinations. Compare correctness and operational behavior, not developer preference:

- Recovery and resume after forced failure.
- Human revision at Analyze and Plan.
- Duplicate request and callback handling.
- Structured-output failure and bounded repair.
- Provider/model substitution.
- Tool authorization enforcement.
- Trace and artifact completeness with redaction.
- Golden-reference behavioral result.
- Implementation complexity, dependency footprint, latency, and cost.
- Deployment and model access through the available RHOAI runtime or a documented local equivalent.
- No compute allocation retained while the run waits at a human checkpoint.

The spike result should recommend a stack per layer, identify components to reuse from existing work, and record which findings would trigger reconsideration. It should not redefine the service contracts to match the winning framework.

## Sources

- [AI Test Generation ADR](adr-ai-test-generation.md)
- [AI Test Generation Decision Record](decision-record.md)
- [Decision Ledger](decision-ledger.md)
- [Existing Work Landscape](existing-work-landscape.md)
- [Tool Reliability Findings](tool-reliability-findings.md)
- [Updated AI Test Harness Blueprint Business Case](../AI%20Test%20Harness%20Blueprint%20Business%20Case.md)
- [Secure Software Supply Chain SDLC ADR](../ADR%20-%20Secure%20Software%20Supply%20Chain%20SDLC.md)
- [LangGraph overview](https://docs.langchain.com/oss/python/langgraph/overview)
- [Temporal Python SDK guide](https://docs.temporal.io/develop/python)
- [Pydantic AI documentation](https://ai.pydantic.dev/)
