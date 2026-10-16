## Summary

Both resources provide strong foundations, but they solve different parts of the problem:

- The ADR defines how to establish trustworthy vulnerability impact evidence without overstating certainty.
- `ai-test-harness` defines how to generate, execute, evaluate, triage, review, and attest tests.
- `ai-test-generation` should sit between them as a **driver-agnostic test-generation system** that can consume vulnerability evidence, PR changes, bug reports, API contracts, dependency upgrades, and user-provided objectives.

The key design principle should be:

> Vulnerability impact is one prioritized input to test generation, not the architecture’s organizing assumption.

## 1. ADR Insights To Adopt

### Layered evidence ownership

The ADR cleanly separates:

- Vulnerability intelligence
- SBOM/package correlation
- Source and build-context assembly
- Symbol mapping
- Static call-graph evidence
- Optional prioritization
- Reporting

This is highly applicable to test generation. The test generator should not independently reinterpret vulnerability intelligence or static reachability. It should consume normalized evidence artifacts.

Proposed equivalent boundary:

```text
Input Driver
  -> Evidence Normalization
  -> Test Planning
  -> Test Generation
  -> Isolated Execution
  -> Evaluation
  -> Reviewable Evidence
```

Each layer should have one owner and a typed artifact contract.

### Evidence versus assessment

The ADR distinguishes immutable graph evidence from downstream assessment and recommendations.

That distinction should carry over directly:

- **Evidence:** source paths, affected symbols, call sites, dependency versions, advisory metadata, test results, coverage, mutation results.
- **Assessment:** likely impact, test priority, confidence, recommended next action.
- **Recommendation:** generate an exposure test, generate a fix-pinning test, request human review, or report insufficient evidence.

The model must not create graph facts, invent reachability, or convert “not reproduced” into “not vulnerable.”

### Explicit uncertainty

The ADR uses statuses such as:

- `complete`
- `partial`
- `not_evaluable`
- `definite_static_path`
- `possible_static_path`
- `unknown`

The test-generation design should use similarly explicit result states:

- `planned`
- `generated`
- `collected`
- `passed`
- `failed`
- `blocked`
- `inconclusive`
- `not_applicable`
- `needs_human_review`

Most importantly:

- A test that cannot run is not a passing test.
- A vulnerability that cannot be reproduced is not proven absent.
- Missing symbol mapping should produce an evidence gap, not a fabricated test target.
- A failed environment setup should not be classified as an application defect.

### Immutable analysis context

The ADR’s `AnalysisContext` is especially valuable. Every generated test should be tied to:

- Repository and source revision
- Base and candidate revisions when differential
- Dependency graph or SBOM identity
- Production/build profile
- Toolchain
- Generated-source state
- Analyzer versions
- Model and prompt versions
- Sandbox image and execution target

Without this, a later reviewer cannot determine what the test actually proved.

### Profile-based onboarding

The ADR correctly rejects inferring production context from repository contents alone.

The test-generation system should similarly require a repository profile containing:

- Native test framework
- Supported commands
- Test directories
- Required services
- Fixture and cleanup conventions
- Allowed test environments
- Sensitive or destructive operations
- Test ownership and review routing
- Artifact formats
- Network and credential policy

The system should not infer production or test authority from arbitrary repository text.

### Conservative first-release boundary

The ADR explicitly defers:

- Runtime activation
- Attacker control
- Exploitability
- Endpoint discovery
- Business-flow reconstruction

That is a useful constraint for test generation. The first release should generate tests from available evidence and known application call sites without pretending to prove full exploitability or business impact.

## 2. Harness Repository Insights

The harness repository is more advanced than the current plan in several practical areas.

### Strong lifecycle model

Its seven-stage pipeline is:

```text
Self-verify
  -> Intake
  -> Analyze
  -> Generate
  -> Execute
  -> Triage
  -> Review
  -> Feedback
```

This should inform our four-stage user-facing model:

```text
Analyze -> Plan -> Code -> Execute
```

The four-stage model is useful for human interaction, but the underlying implementation needs more internal stages:

| User-facing stage | Internal stages |
|---|---|
| Analyze | Self-verification, intake, repository discovery, dependency/SBOM analysis, static analysis |
| Plan | Risk scoring, scenario planning, driver-specific test objectives |
| Code | Framework adapter, model generation, compile/collection repair |
| Execute | Sandbox execution, differential testing, mutation, triage, evidence, review, feedback |

The user should see four understandable phases while the controller preserves the more granular lifecycle internally.

### Driver-specific risk budgeting

The harness already supports a useful risk model based on:

- Dependency depth
- Reachability
- Vulnerabilities
- API changes
- Changed lines
- Sensitive domains
- Preflight findings
- New packages

This is a good starting point, but the risk model should be generalized into a **test-generation objective and budget policy**.

Possible drivers:

- First-party PR change
- Dependency version change
- Vulnerability/advisory
- Bug report
- API/schema change
- Contract compatibility change
- Regression history
- User-requested test gap
- Scheduled rescan
- Pipeline or test-quality finding

Each driver contributes evidence and requested test categories. The risk model controls depth and budget, but should not determine the test objective by itself.

### Adapter interface

The adapter interface is one of the strongest implementation concepts:

```text
resolve_graph
extract_api
api_diff
build
run_tests
coverage
mutate
fuzz
```

This should be reused conceptually. The target project should add a second layer above ecosystem adapters: **input-driver adapters**.

Example:

```text
VulnerabilityDriver
PullRequestDriver
BugReportDriver
DependencyUpgradeDriver
ContractChangeDriver
UserObjectiveDriver
TestGapDriver
```

Each driver should normalize its source into a common `TestObjective` rather than directly invoking the model.

### Failure register

The failure register is a major strength and should be adopted as a first-class design requirement.

Its structure is particularly valuable:

```text
Issue
Reason
Cause
Detector
Correction
Self-check
```

This is better than generic “retry on failure” behavior. It converts observed model and harness failures into regression tests for the harness itself.

The target design should require:

- Stage-zero self-verification.
- A failure register before broad automation.
- Self-checks for every known failure mode.
- Fail-closed behavior when self-verification fails.
- No evidence publication from an unverified harness.

### Evidence and provenance manifests

The harness’s manifest schema provides a strong model for generated-test identity:

- Stable test ID
- File hash
- Category
- Target package and symbols
- Call sites
- Source reference
- SBOM component
- Model ID
- Prompt hash
- Tool versions
- Sandbox image digest
- Baseline result
- Coverage delta
- Mutation results
- Flake runs
- Reviewer state
- Lifecycle state
- Vulnerability role and VEX reference

This should be adapted rather than recreated.

### Differential test semantics

The harness correctly handles the important distinction between:

- Vulnerable versus fixed versions
- Upgrade versus downgrade
- Base versus candidate
- Exposure test versus fix-pinning test
- Same result versus changed result
- Inconclusive execution

This is directly aligned with the ADR’s caution against interpreting version changes or failed reproduction too aggressively.

### Feedback as a lifecycle

The harness treats reviewer edits, approvals, rejections, promotions, and retirements as structured feedback.

That is important for the requested interactive model. User feedback should not disappear into chat history; it should become:

- Stage-scoped feedback
- Reusable repository guidance
- Prompt/model evaluation data
- Test-plan corrections
- Failure-register entries when the harness misunderstood something
- Accepted/rejected examples for future evaluation

## 3. Proposed Structural Changes

These are changes to the design plan, not implementation yet.

### A. Reframe the system around input drivers

The current plan lists several use cases, but they should become a formal abstraction.

Define:

```text
TestGenerationRequest
  - request_id
  - driver_type
  - source_references
  - repository_context
  - target_revision
  - optional_comparison_revision
  - objectives
  - constraints
  - requested_evidence
  - safety_policy
  - user_feedback
```

The driver produces a normalized objective. The rest of the pipeline remains shared.

For example:

```text
VulnerabilityDriver
  -> exposure test
  -> fix-pinning test
  -> reachability evidence
  -> optional VEX evidence

PullRequestDriver
  -> changed-behavior tests
  -> negative/boundary tests
  -> regression coverage

BugReportDriver
  -> reproduction test
  -> regression test
  -> failure classification

ContractDriver
  -> compatibility tests
  -> schema/property tests
  -> consumer impact tests
```

### B. Separate evidence intake from test planning

The planner should consume normalized evidence rather than raw advisory text, PR text, or model summaries.

Proposed flow:

```text
Raw Input
  -> Source Adapter
  -> Normalized Evidence
  -> Test Objective
  -> Test Plan
```

Raw external text should remain untrusted data. The model may interpret it, but it must not be treated as control instructions.

### C. Add an evidence graph or evidence bundle abstraction

The test plan should reference evidence objects such as:

- `RepositoryProfile`
- `SourceContext`
- `DependencyContext`
- `AdvisoryEvidence`
- `ReachabilityEvidence`
- `ApiDiffEvidence`
- `BugEvidence`
- `ExistingCoverageEvidence`
- `RuntimeEnvironment`
- `UserDecision`

The planner then records which evidence supports every scenario and oracle.

This prevents a generated test from having an unexplained origin.

### D. Make stage checkpoints first-class controller objects

The current interactive stage model should become a durable state machine.

Recommended state:

```text
Run
  -> AnalyzeCheckpoint
  -> PlanCheckpoint
  -> CodeCheckpoint
  -> ExecuteCheckpoint
```

Each checkpoint should contain:

- Immutable source inputs
- Current artifact references
- Assumptions
- Open questions
- User feedback
- Decisions
- Stage status
- Retry/repair history
- Next permitted transition

The system should support transitions such as:

```text
Analyze -> Plan
Plan -> Analyze
Plan -> Code
Code -> Plan
Code -> Execute
Execute -> Code
Execute -> Plan
Execute -> Review
```

Examples:

- Missing repository convention: return to Analyze.
- Wrong test scope: return to Plan.
- Compile failure: return to Code.
- Incorrect expected result: return to Plan.
- Infrastructure failure: return to Analyze or Execute setup.
- Successful validation: proceed to Review.

### E. Preserve the distinction between user feedback and evidence

User input should have explicit types:

- `scope_change`
- `requirement_clarification`
- `expected_behavior`
- `excluded_case`
- `fixture_preference`
- `safety_constraint`
- `environment_authorization`
- `assertion_correction`
- `classification_override`

A free-form comment should be stored, but the system should also classify what artifact or decision it changes.

### F. Add triage and review as internal Execute outputs

The four-stage model can keep the user experience simple, but Execute must include:

- Focused execution
- Related-suite execution
- Differential execution
- Mutation testing
- Stability checks
- Result normalization
- Triage
- Evidence packet generation
- Review recommendation

Otherwise “Execute” will be too broad to diagnose failures accurately.

## 4. Proposed Functional Changes

### 1. Vulnerability-driven testing

Adopt the ADR’s future runtime branch as an explicit optional test-generation path:

```text
Advisory
  -> Package/version applicability
  -> Symbol or fix-range mapping
  -> Static reachability
  -> Test objective
  -> Exposure test
  -> Fix-pinning test
  -> Isolated runtime evidence
```

Rules:

- Static reachability can prioritize testing but does not prove runtime exploitability.
- Successful reproduction confirms behavior only for the tested artifact and conditions.
- Failure to reproduce is `not_reproduced` or `inconclusive`, never `not_affected` by itself.
- VEX generation remains downstream and requires the appropriate human/security approval.

### 2. Non-vulnerability drivers

Add a common driver framework for:

- PR changes
- Dependency upgrades
- Bug reports
- API/schema changes
- Regression history
- Test-gap requests
- User-provided objectives
- Scheduled analysis
- CI/test-quality findings

This avoids building a system whose prompt, evidence, and report models assume every run starts from a CVE.

### 3. User-controlled gates

The harness should default to:

1. Analyze
2. Pause for user review
3. Plan
4. Pause for user review
5. Code
6. Pause when assumptions change or generation is non-trivial
7. Execute
8. Pause for result classification when evidence is ambiguous

An unattended mode can exist for trusted repositories and low-risk requests, but it should be explicit policy.

### 4. Plan-level feedback

Allow the user to say:

- “Do not generate another malformed-token test; existing TC-44 covers it.”
- “This behavior is expected and should not be treated as a defect.”
- “Use the existing RHBK fixture, not a mock.”
- “This is an integration concern, not an E2E concern.”
- “The expected status is 401, not any 4xx.”
- “Do not test rate limiting in this run.”

The plan should update without regenerating unrelated scenarios.

### 5. Code-level feedback

Allow feedback on:

- File placement
- Test naming
- Fixture use
- Mock boundaries
- Assertion strictness
- Parametrization
- Cleanup
- Framework idioms

The system should regenerate only affected files or test cases, preserving accepted code.

### 6. Execute-level feedback

Allow the user to classify or correct:

- Expected failure
- Infrastructure failure
- Test bug
- Flake
- Wrong environment
- Wrong oracle
- Missing fixture
- Known product behavior

The system should record the classification and route back to the appropriate stage.

### 7. Harness self-testing

Make the failure register part of the architecture, not an implementation detail.

Required categories:

- Prompt injection
- Weak assertions
- Fabricated imports or symbols
- Collection failures
- Baseline failures
- Differential-environment errors
- Wrong vulnerable/fixed role
- Flaky execution
- Model truncation or reasoning leakage
- Invalid provenance
- Unsafe network or secret access
- Incorrect triage classification

### 8. Explicit evidence limitations

Every report should include:

- What the test proved.
- What it did not prove.
- Which assumptions were user-provided.
- Which evidence was static versus runtime.
- Which dependencies were mocked.
- Whether the test ran on both versions.
- Whether the environment matched the intended production context.
- Whether the result was complete, partial, blocked, or inconclusive.

## 5. Recommended Relationship Between the ADR and Test Generation

The ADR should remain the authority for vulnerability-impact evidence.

The future AI Test Generation ADR should not duplicate or redefine:

- Vulnerability feed ownership
- SBOM ownership
- Static graph semantics
- Symbol-mapping certainty
- Production profile semantics

Instead, it should define how test generation consumes those artifacts.

Proposed relationship:

```text
Vulnerability Impact ADR
  produces:
    - applicability evidence
    - affected-symbol evidence
    - reachability/path evidence
    - analysis limitations

AI Test Generation design
  consumes:
    - those evidence references
  produces:
    - test objectives
    - exposure/fix-pinning tests
    - runtime evidence
    - test provenance
    - review recommendations
```

This keeps the systems independently useful:

- Static impact assessment can publish without runtime testing.
- Test generation can run for non-vulnerability inputs.
- Runtime test evidence can enrich, but not rewrite, static evidence.

## 6. Recommended Next Design Artifacts

Before writing the AI Test Generation ADR, I recommend designing these artifacts:

1. `TestGenerationRequest` schema
2. `InputDriver` interface
3. `EvidenceBundle` schema
4. `TestObjective` schema
5. `TestPlan` schema
6. `StageCheckpoint` and feedback model
7. `ExecutionResult` and limitation statuses
8. `TestProvenanceRecord`
9. Driver-to-test-category matrix
10. Controller state-transition diagram
11. Failure-register contract
12. Human-gate policy
13. Vulnerability-to-test evidence mapping
14. Example end-to-end runs:
    - CVE with definite static reachability
    - CVE with unknown reachability
    - dependency upgrade without vulnerability
    - first-party PR
    - bug report with regression test
    - user-requested test-gap analysis

## 7. Red Hat Lightwell Integration Context

The AI Test Generation capability is intended to integrate into the Red Hat Lightwell platform. Lightwell is the surrounding product and evidence ecosystem; AI Test Generation is a specialized capability within that ecosystem, not a replacement for Lightwell services.

The design should assume the following separation until Lightwell interfaces are formally defined:

| Concern | Likely Lightwell/platform responsibility | AI Test Generation responsibility |
|---|---|---|
| Vulnerability intelligence | Advisory ingestion, normalization, correlation, and lifecycle | Consume normalized advisory references and preserve source provenance |
| SBOM and dependency inventory | Package identity, version, artifact, and dependency relationships | Use those references to select test objectives and differential contexts |
| Artifact/build context | Build identity, image digest, source revision, deployment/profile facts | Verify that the execution context matches the supplied profile |
| Static impact evidence | Symbol mapping, call graph, reachability, path witnesses, limitations | Consume evidence to prioritize and shape exposure or fix-pinning tests |
| Workflow orchestration | Platform triggers, tenants, authorization, scheduling, and durable run ownership | Expose stage operations and checkpoint state through an integration contract |
| Evidence storage and attestation | Shared evidence records, signing, retention, and policy verification | Produce test results, provenance, runtime evidence, and explicit limitations |
| Security/remediation workflows | Product Security review, VEX lifecycle, notification, and remediation routing | Supply scoped test evidence and recommendations; never make final security claims |

The exact ownership must be confirmed with Lightwell stakeholders. These boundaries are design assumptions intended to prevent duplicated platform services and conflicting sources of truth.

### Integration principles

- Lightwell remains authoritative for vulnerability, SBOM, artifact, and platform identity records.
- AI Test Generation references Lightwell artifacts by stable IDs or URIs; it should not silently copy or reinterpret authoritative records.
- Generated tests and runtime evidence carry the Lightwell context that selected them: application, artifact, package, advisory, build profile, and analysis run.
- Lightwell may invoke the Analyze -> Plan -> Code -> Execute lifecycle, pause it for user decisions, and resume it from checkpoint state.
- User authorization, tenant isolation, retention, and audit policy come from Lightwell or its approved platform services.
- The test-generation service must fail closed when required Lightwell context is missing, stale, ambiguous, or unauthorized.
- Runtime simulation remains isolated and never targets production by default.

### Candidate integration contract

The future design should define a narrow adapter boundary rather than hard-code assumptions about Lightwell internals:

```text
Lightwell trigger
  -> TestGenerationRequest
  -> Lightwell evidence references
  -> AI Test Generation checkpoints
  -> Test and runtime evidence
  -> Lightwell evidence/reporting workflow
```

At minimum, the boundary will need contracts for:

- Request creation and correlation IDs.
- Repository, source revision, artifact, and build-profile references.
- Advisory, SBOM, and reachability evidence references.
- Stage checkpoint status and user feedback.
- Test results and runtime-validation result states.
- Provenance and artifact identity.
- Human review, approval, rejection, and escalation decisions.

These are integration design topics for the eventual AI Test Generation ADR; they are not yet commitments to specific Lightwell APIs.

## 8. Holistic Test-Generation Model

The project should be designed as one test-generation system with multiple input drivers, not as a CVE testing tool with additional features attached. Vulnerability assessment is an important prioritization source, but the same planning, generation, execution, and evidence machinery should support:

- First-party pull requests.
- Dependency upgrades and downgrades.
- Bug reports and regression reproductions.
- API, schema, and compatibility changes.
- Existing test-gap requests.
- CI and assertion-quality findings.
- Scheduled characterization or rescan jobs.
- Explicit user objectives.

The common architecture is:

```text
Input driver
  -> normalized evidence
  -> test objective
  -> Analyze
  -> Plan
  -> Code
  -> Execute
  -> triage and evidence
```

The four stages are the user-facing interaction model. Internally, they contain the finer-grained harness lifecycle:

| User-facing stage | Internal responsibilities |
|---|---|
| Analyze | Self-checks, intake, repository profile, SBOM/dependency context, source/API analysis, vulnerability and reachability evidence |
| Plan | Risk/budget policy, scenario selection, test-level choice, oracle definition, mock policy, exclusions, and user approval |
| Code | Framework adapter, generation, compilation/collection, assertion checks, bounded repair |
| Execute | Sandbox execution, differential comparison, mutation/stability checks, triage, review packet, provenance, and feedback capture |

Every stage must support checkpointing and user feedback. Feedback should identify the stage and decision it changes, for example `expected_behavior`, `excluded_case`, `fixture_preference`, `safety_constraint`, or `classification_override`. Accepted artifacts should not be regenerated when feedback changes only a later stage.

## 9. Vulnerability Confirmation and Production Simulation

The ADR correctly distinguishes several claims that must not be collapsed:

1. The application or artifact uses an affected package and version.
2. The advisory maps to an affected symbol or source change.
3. Application-owned code has a static path to that symbol.
4. A representative runtime can activate the relevant path.
5. The vulnerable behavior can be reproduced with an applicable input.
6. The behavior is exploitable or materially impacts production.

The first three are primarily static evidence. The fourth and fifth require runtime validation. The sixth requires additional security and product judgment and is not established by an automated test alone.

The project should treat production-environment simulation as a future, optional evidence tier rather than a first-release requirement:

```text
Production profile
  -> representative build/artifact
  -> isolated environment
  -> required services/configuration
  -> realistic entrypoint or call-site setup
  -> controlled trigger/input
  -> runtime observation
  -> scoped reproduction evidence
```

### What simulation should establish

A useful simulation may establish that:

- The tested artifact matches the relevant production build context.
- The affected dependency and version are actually present in the artifact.
- Required feature flags, platform settings, generated sources, and service dependencies are present.
- The application can reach the affected behavior through a representative route, job, message, CLI, or internal call site.
- A controlled input causes the vulnerable behavior or reaches the relevant guard/fix.

It must not claim that a failed reproduction proves the application is safe. A failed run may reflect missing traffic, configuration, platform differences, unavailable services, insufficient trigger construction, or a test harness defect.

### Simulation profiles

Research should compare several levels of fidelity:

| Profile | Purpose | Evidence strength | Typical cost |
|---|---|---|---|
| Library-level fixture | Exercise the affected function with controlled inputs | Behavior of the dependency/API only | Low |
| Application call-site test | Exercise the way application code invokes the dependency | Application-to-dependency behavior | Low to medium |
| Component environment | Run the application with required local services and production-like configuration | Cross-component activation and behavior | Medium |
| Deployment-shaped environment | Use the production artifact, deployment manifests, platform, flags, and service topology in isolation | Strongest practical reproduction evidence | High |
| Production observation | Validate behavior against production telemetry or controlled production testing | Operational confirmation, subject to strict policy | Very high and generally out of scope |

The system should record the selected profile and its differences from production. “Production-like” is not a sufficient claim without a declared profile, artifact identity, configuration comparison, and service dependency inventory.

### Research questions for a future design

- How can a production artifact be reconstructed or selected from the repository/build provenance?
- Which deployment facts are required: image digest, platform, architecture, feature flags, generated configuration, service versions, identity, and network policy?
- How should required external services be simulated: real disposable services, protocol-compatible fakes, recorded interactions, or a mix?
- How can an entrypoint or call-site be selected without allowing an LLM to invent production flows?
- How are attacker-controlled or sensitive inputs safely generated, minimized, and retained?
- What evidence proves that the runtime actually traversed the affected symbol or guard?
- How are non-deterministic, asynchronous, and distributed behaviors observed?
- Which simulation results may inform VEX, and which require Product Security approval?
- How are undisclosed vulnerabilities isolated and access-controlled?
- What is the minimum useful fidelity for each ecosystem and deployment model?

### Proposed result states

Runtime validation should return more precise outcomes than pass/fail:

- `reproduced`: the scoped trigger reached and demonstrated the vulnerable behavior.
- `fixed_behavior`: the same trigger reached the fixed behavior in the candidate environment.
- `reached_not_reproduced`: the relevant path was observed, but the vulnerable outcome did not occur.
- `not_reached`: the test did not activate the relevant path.
- `environment_mismatch`: the simulation did not match required production facts.
- `blocked`: policy, service, credential, or sandbox constraints prevented execution.
- `inconclusive`: evidence is insufficient to classify the result.

These outcomes enrich the static impact report but do not rewrite its graph certainty. A runtime result should carry the tested artifact digest, environment profile, configuration fingerprint, trigger/input reference, path-observation evidence, logs, and limitations.

### Scope recommendation

Do not make production simulation a POC gate. Add a research track after the initial static-evidence and test-generation workflow is stable. Begin with one reference application and one vulnerability where:

- The affected behavior has a known trigger.
- The application call site is understood.
- The build artifact is reproducible.
- Dependencies can run in a disposable environment.
- The simulation can avoid production credentials and sensitive data.

The research outcome should be a separate design proposal or future ADR, not an implicit expansion of the initial AI Test Generation scope.

## 10. Vulnerability Validation Landscape

The open-source ecosystem provides two complementary classes of tools for separating theoretical vulnerability presence from demonstrated application exposure. These tools should be treated as potential Lightwell integrations and evidence producers, not as replacements for the AI Test Generation lifecycle.

### Active exploitation and adversarial validation

These tools execute controlled attack techniques or proof-of-concept payloads against an explicitly authorized target environment:

| Tool or project | Potential role | Evidence produced |
|---|---|---|
| Filigran OpenAEV | Adversarial exposure validation and attack-path orchestration | Control/exposure execution records and attack-chain outcomes |
| RedAmon | Agent-assisted penetration-test exploration and exploit chaining | Tool actions, attempted chains, target responses, and remediation signals |
| OWASP Nettacker | Modular active service testing and vulnerability validation | Scan/test results and target behavior |
| Metasploit Framework | Precise exploit-module execution for supported CVEs and services | Module result, target response, callback or payload evidence where applicable |

These tools answer a question different from static reachability:

> Can an authorized exploit attempt succeed against this running target under these exact conditions?

They require substantially stronger controls than ordinary test execution:

- Explicit authorization and target identity.
- Disposable or tightly isolated environments.
- Non-production enforcement.
- Payload and capability allowlists.
- Egress, callback, credential, and data-exfiltration controls.
- Time, resource, and blast-radius limits.
- Full action logging and human review for high-impact modules.
- Safe handling of undisclosed or restricted vulnerability details.

Active-exploitation results must never be treated as proof that production is exploitable unless the tested artifact, configuration, network exposure, and relevant deployment conditions are demonstrated to match production. A failed exploit attempt is not proof of safety.

### Reachability and contextual validation

These tools establish whether vulnerable code can be reached or whether a vulnerability is applicable in the application context:

| Tool or project | Potential role | Evidence produced |
|---|---|---|
| OpenVEX/VEX tooling | Record and review contextual applicability and exploitability claims | Auditable VEX statements and justifications |
| Semgrep | Taint and data-flow analysis from sources to vulnerable sinks | Source-backed paths, blockers, sanitizers, and analysis limitations |
| Opengrep | Open-source static rule and taint-analysis alternative | Matching paths, rule results, and limitations |
| Language-server/compiler analysis | Map affected symbols and incoming calls | Symbol identity, static call facts, and path witnesses |

These tools answer a different question:

> Does application-controlled or attacker-controlled data have a statically supported path to the affected behavior?

Static reachability is valuable prioritization and test-planning evidence, but it does not by itself establish runtime activation, valid configuration, attacker control, exploitability, or production deployment.

### Proposed evidence layering

The AI Test Generation and Lightwell integration should preserve the distinction between these outcomes:

```text
Advisory applicability
  -> static symbol/reachability evidence
  -> generated call-site or exposure test
  -> representative runtime reproduction
  -> authorized active-exploitation validation
```

Each layer should enrich the report without rewriting earlier evidence:

| Layer | Possible result | Interpretation |
|---|---|---|
| Applicability | matched, unmatched, unknown | Package/version/platform relationship |
| Static mapping | definite path, possible path, no observed path, incomplete | Source/build-context evidence |
| Generated test | passed, failed, blocked, inconclusive | Behavior under the selected test setup |
| Runtime simulation | reproduced, reached-not-reproduced, not-reached, environment-mismatch | Behavior in a declared representative environment |
| Active validation | exploit confirmed, exploit unsuccessful, blocked, not applicable | Result of an authorized adversarial action |
| VEX decision | affected, not affected, fixed, under investigation | Human/security-reviewed product statement |

The model may help select and explain tests, but it must not upgrade a weaker result into a stronger one. For example:

- A static path does not become exploitability merely because an LLM describes an attack.
- A generated test passing does not establish that production is safe.
- An unsuccessful Metasploit or OpenAEV attempt does not establish `not_affected` without environment and trigger-coverage evidence.
- A VEX statement should cite the evidence and remain subject to Product Security ownership.

### Integration recommendation

Treat these tools as pluggable validation adapters behind a common contract:

```text
validate(target, scenario, policy)
  -> tool identity and version
  -> target/artifact/environment identity
  -> actions attempted
  -> observations and callbacks
  -> result state
  -> evidence references
  -> limitations and required review
```

The initial AI Test Generation POC should prioritize static/contextual evidence and safe application-level tests. Active exploitation should be a separately approved research track, initially exercised only against a purpose-built disposable target with a single known vulnerability and a documented rollback/cleanup policy.

The tool list above is a candidate landscape, not an adoption decision. Before integration, each tool requires validation of project activity, license, deployment model, supported vulnerability classes, evidence quality, isolation behavior, and compatibility with Lightwell’s authorization, tenant, audit, and evidence services.

## 11. Structural Lessons From Agentic Dependency Remediation

`Agentic Dependency Remediation.md` is a landscape and comparison document rather than a formal ADR, and that is acceptable: its formatting — concise, plain English, consistent tables, evidence notes, constrained conclusions — is the preferred model for the eventual AI Test Generation ADR. What it lacks is only minimal decision-record metadata (status, an explicit decision, consequences), which we add. The Evidence-Driven Vulnerability Impact Prioritization ADR remains the reference for evidence-boundary discipline, not for format.

### Patterns worth reusing

- **Start with a capability definition.** It defines dependency remediation and application-code remediation separately before comparing tools.
- **Use a comparison matrix.** Each option is assessed against the same dimensions: native capability, autonomous behavior, hosting, licensing, extension cost, and evidence.
- **Distinguish documented facts from assessment.** The ADR explicitly labels evidence limitations and avoids treating “not documented” as “unsupported.”
- **Separate adjacent workflows.** Dependency version updates and application-code migration are related but not the same product capability.
- **End with a constrained recommendation.** The conclusion names realistic directions and their limiting factors instead of declaring a universal winner.
- **Include ownership and placement.** It identifies Lightwell as a likely product home but correctly leaves organizational ownership as an open question.

### Structural concerns for our eventual ADR

The current research material is intentionally broader than an ADR. It combines landscape research, architecture, future ideas, candidate tools, runtime-validation questions, and proposed contracts. Before creating the eventual decision record, split these concerns into separate artifacts:

1. **AI Test Generation ADR:** the decision, boundaries, ownership, lifecycle, evidence rules, and major alternatives.
2. **Input-driver design:** vulnerability, PR, dependency, bug, contract, test-gap, and user-objective drivers.
3. **Runtime-validation detailed design:** representative production simulation and active exploitation, explicitly optional.
4. **Lightwell integration design:** API/event contracts, tenancy, authorization, evidence references, and lifecycle callbacks.
5. **Ecosystem/tool landscape:** current tools, license/activity checks, documented capability, and adoption status.
6. **Schemas and contracts:** requests, evidence bundles, objectives, checkpoints, execution results, and provenance.

The eventual ADR should not copy the full research document. It should link to these detailed designs and preserve only the decisions and constraints necessary to understand the architecture.

### Concerns exposed by the comparison

#### Capability boundary

Dependency remediation and test generation must remain separate capabilities. Test generation may provide failing tests, regression evidence, and upgrade validation to a remediation workflow, but it should not silently edit application code or dependency manifests.

```text
Dependency/remediation driver
  -> AI Test Generation
  -> tests and evidence
  -> separate remediation workflow
  -> new candidate revision
  -> AI Test Generation validation
```

This preserves independent evidence and prevents the remediation workflow from self-certifying its own change without independent test evidence.

#### Evidence quality

The eventual ADR should use a consistent evidence table for every major claim:

| Claim | Source | Evidence type | Confidence/limitation | Decision impact |
|---|---|---|---|---|
| Capability exists | Vendor/project documentation or executed test | Documented or observed | Scope and version limits | Adopt, integrate, or defer |
| Tool is deployable | License, image, runtime, support data | Operational | Hosting and maintenance constraints | Platform fit |
| Test proves behavior | Base/candidate or runtime execution | Experimental | Environment and trigger limits | Accept evidence or escalate |
| Vulnerability is affected | Lightwell/advisory/static/runtime evidence | Layered | Applicability versus exploitability | Security review route |

Tool-specific adoption checks (license, activity, deployment model, isolation behavior, evidence quality) are already defined in the Vulnerability Validation Landscape section and should not be duplicated per claim; this table governs claim-level evidence, that list governs tool-level adoption.

The AI Test Generation ADR should avoid phrases such as “supports,” “production-like,” or “validated” without defining the evidence behind them.

#### Ownership and controller scope

The Lightwell responsibility table in the Red Hat Lightwell Integration Context section already assigns platform, evidence, and security ownership; the ADR should reference it rather than restate it. What the ADR must add is controller scope: whether the test-generation controller owns only test-generation runs or is also expected to own broader vulnerability-assessment lifecycle state. The current recommendation is that it owns test-generation checkpoints and artifacts while Lightwell owns platform workflow and evidence identity.

#### Decision status and alternatives

The eventual ADR should include explicit status and decision metadata:

```text
Status: Proposed
Scope: AI-assisted test generation integrated with Lightwell
Decision type: Layered, evidence-driven test-generation pipeline
Owner: To be confirmed with Lightwell stakeholders
Related: vulnerability-impact ADR, runtime-validation DD, Lightwell integration DD
```

It should compare at least:

- Build versus integrate with the existing `ai-test-harness` implementation.
- Lightwell-native orchestration versus an independent service invoked by Lightwell.
- Static/contextual test generation only versus optional runtime validation.
- Human-gated operation versus trusted-repository unattended operation.
- Shared Lightwell evidence records versus test-generation-owned records linked to Lightwell.

### Recommendation for ADR preparation

Before writing the ADR, maintain the current research as a working document and track open decisions in a separate `decision-ledger.md` file rather than expanding this document further. Each ledger entry records:

- Decision or question.
- Options considered.
- Evidence collected.
- Current recommendation.
- Owner required for confirmation.
- Follow-up design artifact.
- Conditions that would change the decision.

This keeps the eventual ADR concise, auditable, and appropriately cautious while preserving the broader research behind it.

## 12. Recommended Direction

The next design iteration should not start with model selection or implementation language. It should start with:

```text
Input drivers
  -> Evidence contracts
  -> Test objectives
  -> Interactive checkpoints
  -> Native framework adapters
  -> Isolated execution
  -> Evidence and feedback
```

The existing `ai-test-harness` repository provides a strong implementation reference for adapters, risk budgets, differential testing, failure registration, provenance, and lifecycle management. The ADR provides the necessary discipline around evidence ownership, applicability, uncertainty, source/build context, and non-overclaiming.

The main design opportunity is to combine them without making vulnerabilities the only entry point or allowing the AI to become the source of authoritative evidence.
