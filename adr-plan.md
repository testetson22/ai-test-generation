# AI Test Generation ADR Plan

This is the planning artifact for the eventual AI Test Generation ADR. It defines the design aspects, options, evidence, exclusions, and drafting sequence. The ADR itself is drafted only after the open entries in `decision-ledger.md` have named owners and confirmed options.

## ADR form

Model the ADR's formatting on `Agentic Dependency Remediation.md`: concise, plain English, no unexplained jargon, consistent comparison tables, per-option sections with pros/cons/assessment, an explicit evidence note separating documented fact from assessment, and a conclusion that names realistic directions with their limiting factors instead of a universal winner.

Add only the minimal decision-record elements that document lacks:

- A status, scope, owner, and related-decisions line.
- An explicit decision statement early in the document.
- A short consequences section (positive, negative, neutral).

Deliberately avoid the heavyweight apparatus of the Evidence-Driven Vulnerability Impact Prioritization ADR: no embedded wire contracts, no illustrative JSON, no numbered sub-decision taxonomy. Schemas and contracts live in linked detailed designs. The vulnerability ADR remains the reference for evidence-boundary discipline — what the ADR is allowed to claim — not for its format.

## Decisions the ADR will record

### Core architectural questions

1. **Input drivers:** how to support vulnerability impact alongside PR, dependency, bug, contract, test-gap, and user-objective inputs.
2. **Stage model:** whether four user-facing stages (Analyze, Plan, Code, Execute) should sit over a finer-grained internal lifecycle with durable checkpoints and typed feedback.
3. **Evidence separation:** how to keep evidence, assessment, and recommendation distinct while preventing the model from creating or upgrading evidence.
4. **Result states:** which explicit states are required, including `blocked`, `inconclusive`, and `needs_human_review`.
5. **Three-states principle:** how every reached behaviour becomes validated, handed to the team for its environment, or a stated gap with its reason.
6. **Boundary:** whether the harness only tests and proposes through pull requests, or also supports other controlled outputs.
7. **Failure register:** whether observed harness failures require detectors, corrections, self-checks, and fail-closed runs.
8. **Adapter layering:** how input-driver adapters and ecosystem adapters compose while preserving native framework output.
9. **Immutable analysis context:** which source, dependency, build, toolchain, model, prompt, and sandbox identities are required.
10. **Golden-reference validation:** which generated outputs, behaviors, evidence, and failure modes require repeatable reference checks across every implementation option.

### Decisions from the ledger

Entries 1 through 11 of `decision-ledger.md` provide options and evidence for implementation basis, orchestration, runtime validation, active exploitation, operating mode, evidence ownership, controller scope, simulation fidelity, pilot targets, platform framing, and existing-tool reuse. The ADR should cite confirmed options and preserve unresolved alternatives as open questions.

## Inputs

- `business-case.md` — the simplified 2026-10-16 revision: platform-agnostic framing, three-states definition of done, quantitative cost and quality targets, reviewer-first pilot measures.
- `ai-test-generation-research.md` — sections 1 through 12: ADR insights, harness insights, structural and functional changes, Lightwell integration context, holistic model, production simulation, validation landscape, structural lessons.
- `existing-work-landscape.md` — odh-test-gen (`fullsend-ai/test-creator`) and the internal test plan generator pipeline: what to consume, what to learn from, and the verification required first.
- `vulnerability-driven-structure.md` — decision-neutral high-level relationship between Vulnerability Prioritization and AI Test Generation.
- `tool-reliability-findings.md` — concrete observed and reported shortcomings that motivate deterministic and golden-reference quality gates.
- `decision-ledger.md` — open design questions, options, evidence, owners, and change conditions.
- Evidence-Driven Vulnerability Impact Prioritization ADR — the evidence contracts test generation consumes; the boundary between the two systems.
- `ai-test-harness` repository — blueprint (docs/03), decision record (docs/21), pilot plan (docs/22), failure register (docs/15), evidence rollup (docs/11), adapter interface, and manifest schema.
- Agentic Dependency Remediation — capability boundary with remediation workflows; structural patterns.

## What the ADR excludes

Linked detailed designs, not duplicated:

- Input-driver detailed design.
- Runtime-validation detailed design (production simulation, active exploitation).
- Lightwell integration and API design.
- Ecosystem and tool landscape.
- Schemas: `TestGenerationRequest`, `EvidenceBundle`, `TestObjective`, `TestPlan`, `StageCheckpoint`, `ExecutionResult`, `TestProvenanceRecord`.

## Proposed ADR outline

Modeled on the remediation document's flow:

1. **Framing** — one-paragraph decision statement; status, scope, owner, and related-decisions line.
2. **What counts as AI test generation** — capability definition: what the system does and deliberately does not do, including the three-states principle and the testing-only boundary.
3. **The decision** — layer ownership (Lightwell, controller, drivers, adapters, execution, review) and the stage model, in prose and small tables.
4. **Options considered** — the ledger's decision entries, each with options, evidence, and assessment in the remediation document's per-option style.
5. **Pros, cons, and maintenance** — for each serious option, without assuming a selected direction.
6. **Decision status** — selected options, if any, and unresolved alternatives.
7. **Conclusion** — realistic directions and their limiting factors.
8. **Consequences** — positive, negative, and neutral for selected or compared options.
9. **Open questions** — owned and dated.
10. **References** — linked detailed designs, research, ledger, and harness documents.

## Drafting sequence

1. Confirm owners for open ledger entries and gather evidence for each serious option.
2. Draft Title, Status, Context, and Alternatives without selecting an option.
3. Circulate Context and Alternatives with Lightwell stakeholders for the ownership-boundary assumptions.
4. Confirm linked detailed designs and schemas exist for everything the ADR would otherwise need to embed; link rather than inline.
5. Record the selected option and justification only after owner confirmation; complete Consequences and set status to `Proposed`.

## Ready checklist

- [x] Business case simplified and synced into this repository.
- [x] Research consolidated, renumbered, and reviewed.
- [x] Decision ledger with options, evidence, owners, and change conditions.
- [ ] Ledger owners confirm selected options, where a decision is ready.
- [ ] Lightwell interface assumptions validated with stakeholders.
- [ ] Harness decision record (docs/21) reviewed for overlaps and citations.
- [ ] ADR skeleton drafted from this outline.
