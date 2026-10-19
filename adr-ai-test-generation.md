# AI Test Generation

## Status

Proposed

## Context

Software changes and dependency updates can alter behavior that existing tests do not exercise. A package version or advisory alone does not establish that an application reaches affected behavior or that it is exploitable in production.

Red Hat Lightwell is the intended integration context. Vulnerability Prioritization supplies authoritative evidence [R6]; AI Test Generation consumes it and produces scoped test evidence without rewriting the upstream assessment.

The AI Test Generation service is driver-agnostic: vulnerability findings, pull requests, dependency changes, bugs, contracts, test gaps, and user objectives may all initiate a run. Each request may include additional system context, such as how the target is built and tested, its existing test topology, available environments, and permitted validation actions. The proposed user-facing lifecycle is:

```text
Analyze -> Plan -> Code -> Execute
```

Each stage may pause for user feedback and produce a durable checkpoint. Every reached behavior must end as validated by an executed test, handed to the team for execution in its environment, or recorded as a stated gap with its reason.

## Options

### Option 1: Assemble a service from open-source and internal components

Build the Lightwell integration and lifecycle around selected open-source and internal components rather than adopting one existing project as the system. Candidate inputs include the `ai-test-harness` implementation [R1], `fullsend-ai/test-creator`/`odh-test-gen` [R2], the internal test-plan-generator pipeline [R3][R4][R5], native ecosystem runners, Playwright and pytest [R7][R8], static-analysis and mutation tools [R11][R12], and provenance/attestation tooling [R14]. Known reliability findings and scope limitations must be addressed by the surrounding service rather than assumed away [R15]. Each component would remain behind an adapter or integration boundary after license, ownership, maintenance, and technical-fit review.

**Pros**

- Control over interfaces, lifecycle, and Lightwell integration while reusing proven components.
- Can be designed around the required evidence and checkpoint model from the beginning.
- Allows different components to be selected per function and testing/evaluation ecosystem.

**Cons**

- Requires integration, upgrade, and compatibility ownership across multiple components.
- Requires building adapters, quality gates, provenance, execution, and feedback workflows.
- May produce a fragmented system if boundaries and ownership are not explicit.

### Option 2: Extend or integrate existing work

Candidate sources include the `ai-test-harness` implementation [R1], `fullsend-ai/test-creator` (formerly `odh-test-gen`) [R2], and the internal test-plan-generator pipeline and data repository [R3][R4][R5].

**Pros**

- Existing implementations provide working patterns for deterministic gates, sub-agent analysis, test planning, case generation, PR workflows, and failure handling.
- Reduces duplicated engineering effort.
- Provides real operational evidence and failure modes to inform the design.

**Cons**

- License, ownership, maintenance, and migration status require verification.
- Existing tools have narrower scope and documented reliability gaps: for example, black-box/UI planning, Python/Go support, no sealed differential execution, weak/generated assertions, uncollectable candidates, or evidence that is not bound to the exact executed content [R15].
- Integration may be more complex than a clean implementation if lifecycle and evidence models differ.

### Option 3: Use existing work as reference only

**Pros**

- Avoids dependency on external ownership or incompatible architecture.
- Allows a clean Lightwell-oriented contract and lifecycle.

**Cons**

- Repeats proven work.
- Loses access to existing quality gates, failure registers, analyzers, and operational lessons.

### Option 4: Federated service model

Lightwell owns platform workflow, tenancy, authorization, authoritative evidence identity, and downstream security workflows. Specialized test-generation and ecosystem components are invoked through contracts and return evidence references.

**Pros**

- Aligns with Lightwell platform ownership.
- Allows multiple implementations or adapters to be evaluated independently.
- Keeps vulnerability evidence and test evidence under explicit ownership boundaries.

**Cons**

- Requires stable cross-service contracts and correlation identifiers.
- Distributed lifecycle state and failure handling are more complex.
- Requires clear authority when a component or evidence source is unavailable.

### Cross-cutting evaluation criteria

Every implementation option must be evaluated against the same non-optional requirements:

- Golden-reference validation for known behaviors, failure modes, and representative generation cases.
- Deterministic quality gates that can reject weak, uncollectable, unexecuted, or unsupported test output.
- Isolated execution and evidence tied to the exact generated content and environment.
- Differential, mutation, or equivalent behavioral validation where applicable.
- Human review and explicit handling of blocked, inconclusive, and stated-gap outcomes.

The golden-reference requirement applies whether the service is built from scratch, assembled from components, directly integrated with existing work, or implemented as a federated Lightwell service.

## Decision

We propose a **driver-agnostic, evidence-driven AI Test Generation service integrated with Lightwell through explicit contracts**. The current implementation direction is to build the service around the required evidence and Lightwell contracts while selectively consuming existing tools where they provide sufficient fidelity. Stronger reuse or integration proposals remain admissible.

The first-cut architecture is:

```text
Input driver
  -> normalized evidence and test objective
  -> Analyze
  -> Plan
  -> Code
  -> Execute
  -> triage, provenance, and review evidence
```

Vulnerability Prioritization is one input driver. It supplies applicability, affected-symbol, reachability, path-witness, build-context, and limitation references. Other drivers may supply code changes, dependency upgrades, bugs, contracts, test gaps, or user objectives.

The Analyze stage also evaluates the target before generation. It discovers or consumes the service's test conventions, build and test commands, fixtures, deployment profile, existing coverage, dependency behavior, and available sandbox or dynamic-validation capabilities. It produces a feasibility assessment that determines which validation layers are safe and technically possible for this run: static analysis, library-level tests, application call-site tests, component environments, deployment-shaped simulation, or no automated execution. This assessment constrains the Plan stage; it does not silently turn an unavailable validation layer into a passing result.

The tool will:

- Preserve evidence, assessment, and recommendation as separate concepts.
- Generate native-framework tests from approved plans and repository patterns.
- Use deterministic tooling for discovery, parsing, validation, citation, mapping, and quality gates wherever possible.
- Permit user feedback and approval at every lifecycle stage.
- Execute generated tests in an approved isolated environment.
- Distinguish test failures, setup failures, blocked runs, non-reproduction, and inconclusive evidence.
- Produce test and runtime evidence tied to source, artifact, dependency, environment, model, prompt, and tool identities.
- Deliver tests only through a human-reviewed pull request or an explicitly approved equivalent workflow.
- Avoid modifying production code, dependency manifests, release state, or authoritative Lightwell vulnerability records.
- Validate model, prompt, adapter, and service changes against a maintained golden-reference set before treating them as equivalent or improved.
- Require quality gates to establish that generated tests exercise the prioritized vulnerability, code change, or other stated objective rather than merely compiling or passing.

The following remain open and are not selected by this ADR:

- The extent to which existing tools should be consumed, adapted, forked, or replaced.
- Consume, fork, integrate by contract, or learn-only from `fullsend-ai/test-creator` and internal test-plan tooling.
- Lightwell-native, independent-service, or hybrid orchestration.
- Static/contextual testing versus optional representative runtime validation.
- Whether active exploitation is included in a future separately approved research track.
- Shared, service-owned, or federated evidence-record storage and publication.
- Human-gated versus policy-selected unattended operation.
- The first production-simulation fidelity profile.
- Final security and operational ownership beyond the POC.

These choices remain in `decision-ledger.md` until confirmed by the responsible owners. The existing implementation and tool inventory is listed in the References section below; references identify candidates, not selected dependencies.

## References

The following references identify the existing options considered by this ADR. Listing a reference does not imply adoption, compatibility, approval, or license clearance.

### Existing implementations and internal work

| Ref | Resource | Current evidence | Potential use |
|---|---|---|---|
| [R1] | [`croadfeldt/ai-test-harness`](https://github.com/croadfeldt/ai-test-harness) | Public blueprint and opinionated Python/Go implementation | Pipeline stages, adapters, differential testing, failure register, provenance, mutation, triage |
| [R2] | [`fullsend-ai/test-creator`](https://github.com/fullsend-ai/test-creator), formerly `opendatahub-io/odh-test-gen` | Public skill-based RHOAI test-plan workflow | Strategy input driver, parallel analyzers, deterministic rubric gates, test cases, pytest/Playwright workflows |
| [R3] | [`rhel-ai/agentic-ci/test-plan-generator`](https://gitlab.com/redhat/rhel-ai/agentic-ci/test-plan-generator) | Internal GitLab repository; access required for review | CI trigger, Jira sign-off workflow, batching, scoring gate, artifact publication |
| [R4] | [`rhel-ai/agentic-ci/test-plans-data`](https://gitlab.com/redhat/rhel-ai/agentic-ci/test-plans-data) | Internal GitLab repository; access required for review | Existing test-plan artifact and data organization |
| [R5] | `ADLC Enablement Series_ Test Plan Generator - 2026_10_14 15_00 IDT - Notes by Gemini.md` | Local meeting notes; reported and requires transcript verification | Internal workflow, rubric thresholds, adoption lessons, migration context |
| [R6] | `Evidence-Driven Vulnerability Impact Prioritization.md` | Local proposed ADR | Upstream vulnerability applicability, reachability, path-witness, and limitation evidence |

### Candidate third-party and open-source components

| Ref | Resource | Potential use | Verification required |
|---|---|---|---|
| [R7] | Python, `uv`, pytest, pytest-cov, PyYAML | Runtime, dependency management, test execution, parsing | Version, license, ecosystem fit |
| [R8] | Playwright | Browser/UI execution and verification | Sandbox, cluster access, data isolation |
| [R9] | Ruff, flake8, RedHatQE flake8 plugins, pre-commit, markdownlint | Deterministic linting and repository quality checks | License, support, integration boundaries |
| [R10] | Jira REST API and GitHub CLI | Strategy intake, issue context, PR publication and feedback | Credentials, tenant policy, platform abstraction |
| [R11] | Semgrep/Opengrep and language-server/compiler tooling | Static reachability, symbol mapping, taint and evidence generation | Precision, recall, language coverage, licensing |
| [R12] | Mutation and fuzzing tools | Test-strength and negative-behavior validation | Ecosystem support, execution limits, reproducibility |
| [R13] | OpenAEV, Metasploit, and related validation tools | Future authorized active-exploitation validation | Authorization, isolation, payload controls, audit, license |
| [R14] | in-toto, SLSA, signing and attestation tooling | Test provenance and evidence verification | Lightwell evidence integration and policy support |
| [R15] | [`tool-reliability-findings.md`](https://github.com/testetson22/ai-test-generation/blob/main/tool-reliability-findings.md) | Observed and reported limitations in existing test-generation work | Reliability envelope, self-check, and evidence-gate requirements |

The detailed landscape, evidence levels, and reuse questions are maintained in `existing-work-landscape.md` and `ai-test-generation-research.md`.

## Consequences

### Pros

- One architecture can support vulnerability-driven testing and broader test-generation requests.
- Static vulnerability evidence is preserved without being overstated as runtime exploitability.
- Existing internal and open-source work can be evaluated and reused without committing prematurely.
- Human feedback, evidence gaps, and ambiguous results become explicit workflow states.
- Native tests and signed evidence can integrate with existing repository and Lightwell workflows.
- Deterministic quality gates reduce reliance on model self-assessment.
- Golden references provide repeatable regression checks across implementation options, model changes, prompt changes, and adapter changes.
- Production simulation and active exploitation remain possible without making them first-release requirements.

### Cons

- The initial design may need to coordinate multiple evidence owners and integration boundaries.
- A federated Lightwell integration requires contracts for requests, checkpoints, evidence references, results, and review decisions.
- Supporting multiple input drivers and ecosystems increases adapter and validation effort.
- Human gates and evidence review add latency compared with unconstrained test generation.
- Runtime and active-validation claims require more expensive, carefully controlled environments.
- Existing tools may not be consumable after license, ownership, or maintenance review.
- Golden references require expert curation, versioning, drift management, and protection of sensitive examples.
- Exact output comparison would be brittle; validation must compare normalized structure, behavior, evidence, and execution outcomes where appropriate.
- The first ADR does not resolve all implementation choices; follow-on detailed designs and owner decisions are required.
