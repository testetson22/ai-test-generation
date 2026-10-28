# AI Test Generation

Date: Oct 19, 2026

## Status

Proposed

## Context

Software changes and dependency updates can alter behavior that existing tests do not exercise. A package version or advisory alone does not establish that an application reaches affected behavior or that it is exploitable in production.

Red Hat Lightwell is the intended integration context. Vulnerability Prioritization supplies authoritative evidence \[R6\]; AI Test Generation consumes it and produces scoped test evidence without rewriting the upstream assessment.

The AI Test Generation service is driver-agnostic: vulnerability findings will be the driver for the proof of concept and initial release. Pull requests, dependency changes, bugs, contracts, test gaps, and user objectives may all initiate a run in the future and extensible design here is important. Each request may include additional system context, such as how the target is built and tested, its existing test topology, available environments, and permitted validation actions. The proposed user-facing lifecycle is:

Analyze \-\> Plan \-\> Code \-\> Execute

Each stage's results must be subject to automated review with an option for human-in-the-loop review. After assessing a vulnerability, change, or other input, the service identifies the relevant behaviors to test. For each identified behavior, it records whether the behavior was validated by an executed test, handed to the owning team for environment-specific execution, or recorded as a stated gap with its reason. The resulting sign-off covers test evidence and limitations, not implementation or release approval.

## Options

### Option 1: Assemble a service from open-source and internal components

Build and own the core AI Test Generation service, including request normalization, Analyze → Plan → Code → Execute orchestration, checkpointing, quality gates, evidence evaluation, and Lightwell integration contracts. Selectively consume existing internal and open-source tools through the Vulnerability Prioritization input boundary, analysis, execution, and evidence integration boundaries, while leaving room for future input drivers. A research spike should be executed to identify the best candidates for each component. Any tools identified in this document are preliminary candidates for evaluation.

Proposed architecture:

```
Lightwell request and evidence references
  -> AI Test Generation service
       -> vulnerability prioritization input adapter
       -> repository/context analysis
       -> feasibility assessment
       -> test planning and user checkpoint
       -> native framework generation
       -> isolated execution
       -> quality/differential/golden-reference gates
       -> provenance and review evidence
  -> Lightwell workflow and evidence consumers
```

**Pros**

• Control over interfaces, lifecycle, and Lightwell integration while reusing proven components.

• Can be designed around the required evidence and checkpoint model from the beginning.

• Allows different components to be selected for distinct service responsibilities and supported software stacks.

**Cons**

• Requires integration, upgrade, and compatibility ownership across multiple components.

• Requires building adapters, quality gates, provenance, execution, and feedback workflows.

• May produce a fragmented system if boundaries and ownership are not explicit.

### Option 2: Extend or integrate existing work

Candidate sources include the ai-test-harness implementation \[R1\], fullsend-ai/test-creator (formerly odh-test-gen) \[R2\], and the internal test-plan-generator pipeline and data repository \[R3\]\[R4\]\[R5\]. The important thing is that we don't replicate research efforts. We should fork or version pin any dependencies and change what we need to in the cases of direct consumption.

**Pros**

• Existing implementations provide working patterns for deterministic gates, sub-agent analysis, test planning, case generation, PR workflows, and failure handling.

• Reduces duplicated engineering effort.

• Provides real operational evidence and failure modes to inform the design.

**Cons**

• License, ownership, maintenance, and migration status require verification.

• Existing tools have narrower scope and documented historical failure modes addressed by safety checks, e.g., black-box/UI planning, Python/Go support, no sealed differential execution, weak/generated assertions, uncollectable candidates, or evidence that is not bound to the exact executed content \[R17\].

• Integration may be more complex than a clean implementation if lifecycle and evidence models differ.

### Option 3: Use existing work as reference only

**Pros**

• Avoids dependency on external ownership or incompatible architecture.

• Allows a clean Lightwell-oriented contract and lifecycle.

**Cons**

• Repeats proven work.

• Loses access to existing quality gates, failure registers, analyzers, and operational lessons.

### Cross-cutting evaluation criteria

Every implementation option must be evaluated against the same non-optional requirements:

• Golden-reference validation for known behaviors, failure modes, and representative generation cases.

• Deterministic quality gates that can reject weak, uncollectable, unexecuted, or unsupported test output.

• Isolated execution and evidence tied to the exact generated content and environment.

• Differential, mutation, or equivalent behavioral validation where applicable.

• Human review and explicit handling of blocked, inconclusive, and stated-gap outcomes.

The golden-reference requirement applies whether the service is built from scratch, assembled from components, or directly integrated with existing work.

## Decision

**Option 1: A driver-agnostic, evidence-driven AI Test Generation service integrated with Lightwell through explicit contracts**. The current implementation direction is to build the service around the required evidence and Lightwell contracts while selectively consuming existing tools where they provide sufficient fidelity. Stronger reuse or integration proposals remain admissible.

### Integration boundary

Lightwell is the surrounding platform boundary for workflow triggers, tenancy, authorization, authoritative evidence identity, and downstream security workflows. AI Test Generation and its components exchange requests, checkpoints, and evidence through explicit contracts. This is an integration decision, not an implementation option; the deployment and orchestration topology remain open.

Vulnerability Prioritization is the input driver targeted for the initial release. It supplies applicability, affected-symbol, reachability, path-witness, build-context, and limitation references. Other drivers in the future may supply code changes, dependency upgrades, bugs, contracts, test gaps, or user objectives.

The Analyze stage also evaluates the target before generation. It discovers or consumes the service's test conventions, build and test commands, fixtures, deployment profile, existing coverage, dependency behavior, and available sandbox or dynamic-validation capabilities \[R7\]. It produces a feasibility assessment that determines which validation layers are safe and technically possible for this run: static analysis, library-level tests, application call-site tests, component environments, deployment-shaped simulation, or no automated execution. This assessment constrains the Plan stage; it does not silently turn an unavailable validation layer into a passing result.

The service will:

• Preserve evidence, assessment, and recommendation as separate concepts.

• Generate native-framework tests from approved plans and repository patterns\[R7\].

• Use deterministic tooling for discovery, parsing, validation, citation, mapping, and quality gates wherever possible.

• Permit user feedback and approval at every lifecycle stage\[R8\].

• Execute generated tests in an approved isolated environment managed by Red Hat initially with future options to integrate customer provisioned resources.

• Distinguish test failures, setup failures, blocked runs, non-reproduction, and inconclusive evidence.

• Produce test and runtime evidence tied to source, artifact, dependency, environment, model, prompt, and tool identities. This should be represented by appropriate industry-standard evidence, provenance, attestation formats, attachable to the source-control change, publishable to an approved audit/evidence store, and consumable by automated release and policy gates.

• Deliver tests only through a human-reviewed pull request or an explicitly approved equivalent workflow.

• Avoid modifying production code, dependency manifests, release state, or authoritative Lightwell vulnerability records.

• CI/CD: Internal quality gates that validate model, prompt, adapter, and service changes against a maintained golden-reference set before treating them as equivalent or improved. This is intended to ensure fidelity on a known set of inputs and outputs.

• Require quality gates to establish that generated tests exercise the prioritized vulnerability, code change, or other stated objective rather than merely compiling or passing. A merge decision will consider two things where either can prevent a merge: the pipeline result and the test evidence. The initial release may be a human-involved review, but the end-goal *is* synthesizing these gates with minimal input.

The following remain open and are not selected by this ADR:

• The extent to which existing tools should be consumed, adapted, forked, or replaced.

• Consume, fork, integrate by contract, or learn-only from fullsend-ai/test-creator and internal test-plan tooling.

• Lightwell-native, independent-service, or hybrid orchestration.

• Static/contextual testing versus optional representative runtime validation.

• Whether active exploitation is included in a future separately approved research track.

• Shared, service-owned, or federated evidence-record storage and publication.

• Human-gated versus policy-selected unattended operation.

• The first production-simulation fidelity profile.

• Final security and operational ownership beyond the POC.

These choices remain in [decision-ledger.md](https://github.com/testetson22/ai-test-generation/blob/main/decision-ledger.md) until confirmed by the responsible owners. The existing implementation and tool inventory is listed in the References section below; references identify candidates, not selected dependencies.

## References

The following references identify the existing options considered by this ADR. Listing a reference does not imply adoption, compatibility, approval, or license clearance. As we evaluate existing work and potential dependencies we need to be mindful of the maintenance impact incurred.

### Existing implementations and internal work

| Ref | Resource | Current evidence | Potential use |
| :---- | :---- | :---- | :---- |
| R1 | [https\://github.com/croadfeldt/ai-test-harness](https://github.com/croadfeldt/ai-test-harness)  | Public blueprint and opinionated \`Python/Go implementation | Pipeline stages, adapters, differential testing, failure register, provenance, mutation, triage |
| R2 | [https\://github.com/fullsend-ai/test-creator](https://github.com/fullsend-ai/test-creator)  | Public skill-based RHOAI test-plan workflow | Strategy input driver, parallel analyzers, deterministic rubric gates, test cases, pytest/Playwright workflows |
| R3 | [https\://gitlab.com/redhat/rhel-ai/agentic-ci/test-plan-generator](https://gitlab.com/redhat/rhel-ai/agentic-ci/test-plan-generator)  | GitLab repository; access required for review | CI trigger, Jira sign-off workflow, batching, scoring gate, artifact publication |
| R4 | [https\://gitlab.com/redhat/rhel-ai/agentic-ci/test-plans-data](https://gitlab.com/redhat/rhel-ai/agentic-ci/test-plans-data)  | GitLab repository; access required for review | Existing test-plan artifact and data organization |
| R5 | [ADLC Enablement Series\_ Test Plan Generator](https://docs.google.com/document/d/1bR6zQu8zZza3Zm5KD3yQ4YuQvfzTSO_GSU2m41vZdKY/edit?usp=sharing)  | Meeting notes | Internal workflow, rubric thresholds, adoption lessons, migration context |
| R6 | [Evidence-Driven Vulnerability Impact Prioritization](https://docs.google.com/document/d/1xD6MkLutIbsmEeugL5J-7VumQGOR3weXwe_CsLik0Wo/edit?usp=sharing)  | Related Blueprint ADR | Upstream vulnerability applicability, reachability, path-witness, and limitation evidence |
| R7 | [test-generation.md\#phase-a-discover](https://github.com/testetson22/ai-test-generation/blob/main/test-generation.md#phase-a-discover) | Proposed Analyze-stage repository discovery process. | Repository profiles, native framework selection, test-context discovery, fixtures, CI, and safety constraints. |
| R8 | [ai-test-generation-research.md\#d-make-stage-checkpoints-first-class-controller-objects](https://github.com/testetson22/ai-test-generation/blob/main/ai-test-generation-research.md#d-make-stage-checkpoints-first-class-controller-objects)  | Proposed durable checkpoint/state-transition model. | Future feedback, pause/resume, retry routing, and Lightwell orchestration design. |

### Candidate third-party and open-source components

| Ref | Resource | Potential use | Verification required |
| :---- | :---- | :---- | :---- |
| R9 | Python, uv, pytest, pytest-cov, PyYAML | Runtime, dependency management, test execution, parsing | Version, license, ecosystem fit |
| R10 | Playwright | Browser/UI execution and verification | Sandbox, cluster access, data isolation |
| R11 | Ruff, flake8, RedHatQE flake8 plugins, pre-commit, markdownlint | Deterministic linting and repository quality checks | License, support, integration boundaries |
| R12 | Jira REST API and GitHub CLI | Strategy intake, issue context, PR publication and feedback | Credentials, tenant policy, platform abstraction |
| R13 | Semgrep/Opengrep and language-server/compiler tooling | Static reachability, symbol mapping, taint and evidence generation | Precision, recall, language coverage, licensing |
| R14 | Mutation and fuzzing tools | Test-strength and negative-behavior validation | Ecosystem support, execution limits, reproducibility |
| R15 | OpenAEV, Metasploit, and related validation tools | Future authorized active-exploitation validation | Authorization, isolation, payload controls, audit, license |
| R16 | in-toto, SLSA, signing and attestation tooling | Test provenance and evidence verification | Lightwell evidence integration and policy support |
| R17 | [tool-reliability-findings.md](https://github.com/testetson22/ai-test-generation/blob/main/tool-reliability-findings.md) | Observed and reported limitations in existing test-generation work | Reliability envelope, self-check, and evidence-gate requirements |

The detailed landscape, evidence levels, and reuse questions are maintained in existing-work-landscape.md and ai-test-generation-research.md.

## Consequences

### Pros

• One architecture can support vulnerability-driven testing and broader test-generation requests.

• Static vulnerability evidence is preserved without being overstated as runtime exploitability.

• Existing internal and open-source work can be evaluated and reused without committing prematurely.

• The service can pause for human guidance, resume from a checkpoint, and route evidence gaps or ambiguous results for explicit follow-up instead of silently treating them as success.

• Native tests and signed evidence should be able to integrate with existing repository and Lightwell workflows.

• Deterministic quality gates reduce reliance on model self-assessment.

• Golden references provide repeatable regression checks across implementation options, model changes, prompt changes, and adapter changes.

• Production simulation and active exploitation remain possible without making them first-release requirements.

### Cons

• The initial design will not need to coordinate multiple evidence owners and integration boundaries; however, in the future we may need to handle multiple owners and extensibility needs to be part of the design.

• Contract-based Lightwell integration requires requests, checkpoints, evidence references, results, and review decisions.

• Supporting additional input drivers and software stacks later will increase adapter and validation effort.

• Golden references require curation, versioning, drift management, and protection of sensitive examples.
