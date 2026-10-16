# AI Test Generation

This directory is the starting documentation set for a future AI Test Generation repository. It focuses on generating high-fidelity tests for pull requests, bug fixes, and dependency changes as an integration capability for the Red Hat Lightwell platform.

Canonical repository: <https://github.com/testetson22/ai-test-generation>

## Documents

- [`business-case.md`](business-case.md) - problem statement, proposed value, stakeholders, delivery phases, and business-case definition of done.
- [`test-generation.md`](test-generation.md) - focused technical and delivery proposal for the AI Test Generation system.
- [`factory-specification.md`](factory-specification.md) - broader technical specification covering orchestration, framework adapters, evidence, security, and quality gates.
- [`software-factory-notes.md`](software-factory-notes.md) - supporting architecture and ecosystem notes; these are source material, not normative requirements.
- [`ai-test-generation-research.md`](ai-test-generation-research.md) - working analysis of input drivers, evidence boundaries, harness integration, and future production-environment simulation.
- [`existing-work-landscape.md`](existing-work-landscape.md) - analysis of odh-test-gen (`fullsend-ai/test-creator`) and the internal test plan generator pipeline: what to consume, what to learn from, and required verification.
- [`tool-reliability-findings.md`](tool-reliability-findings.md) - concrete observed failures, scope limitations, and reported operational shortcomings in existing tools.
- [`vulnerability-driven-structure.md`](vulnerability-driven-structure.md) - decision-neutral high-level structure for feeding Vulnerability Prioritization evidence into AI Test Generation.
- [`adr-ai-test-generation.md`](adr-ai-test-generation.md) - first-cut local ADR using the architecture-team template; proposed and not yet approved.
- [`decision-ledger.md`](decision-ledger.md) - open decisions, options, evidence, owners, and change conditions on the path to the ADR.
- [`adr-plan.md`](adr-plan.md) - what the eventual AI Test Generation ADR will decide, its inputs and exclusions, outline, and drafting sequence.

## Recommended Reading Order

1. `business-case.md` to understand the problem and intended value.
2. `test-generation.md` to understand the focused product scope.
3. `factory-specification.md` for detailed implementation constraints.
4. `software-factory-notes.md` for background ideas and alternatives.
5. `ai-test-generation-research.md` for open design questions and future research tracks.
6. `existing-work-landscape.md` for prior internal and open-source work to consume or learn from.
7. `vulnerability-driven-structure.md` for the upstream evidence handoff and validation layers.
8. `adr-ai-test-generation.md` for the first-cut ADR.
9. `decision-ledger.md` and `adr-plan.md` when preparing the next revision.

## Guidance Sources

The future repository should reuse the workspace guidance rather than copy it into competing versions:

- `agentic-ai-guidance/rules/test-generation-essentials.mdc`
- `agentic-ai-guidance/rules/test-quality-patterns.mdc`
- `agentic-ai-guidance/rules/pytest-quality-patterns.mdc`
- `agentic-ai-guidance/prompts/build-test-plan.md`
- `agentic-ai-guidance/guides/agent-collaboration-patterns.md`
- `agentic-ai-guidance/guides/knowledge-capture.md`

The intended operating model is:

```text
Analyze -> Plan -> Code -> Execute
```

Each stage produces a checkpoint that a user can approve, revise, reject, or resume. Repository-specific instructions and existing test plans remain authoritative for each target repository.

The intended deployment context is Red Hat Lightwell. The project should integrate with Lightwell's vulnerability, SBOM, artifact, evidence, and workflow services rather than recreate those systems. The test-generation capability should remain independently testable and should consume well-defined Lightwell evidence references.

## Suggested Future Repository Layout

```text
ai-test-generation/
  README.md
  docs/
    business-case.md
    test-generation.md
    factory-specification.md
    software-factory-notes.md
  schemas/
  adapters/
  prompts/
  policies/
  examples/
  tests/
```

The current directory intentionally contains documentation only. Implementation should begin with repository discovery, the test-plan schema, and one native framework adapter.
