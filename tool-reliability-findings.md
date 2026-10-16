# Existing Test-Generation Reliability Findings

This document records concrete limitations in existing work. It distinguishes observed failures, documented scope limitations, and reported operational problems. It is evidence for comparing build, assembly, integration, and reference-only options; it is not a rejection of any tool.

## Summary

| Resource | Evidence type | Concrete limitation | Design implication |
|---|---|---|---|
| `ai-test-harness` | Observed failures in its failure register | Generated tests can be empty, weak, incorrectly targeted, uncollectable, falsely submitted, or misclassified without explicit gates | A wrapper or successor needs self-checks, evidence-bound submission, differential role handling, and fail-closed triage |
| `fullsend-ai/test-creator` | Documented scope and workflow behavior | Focuses on Jira-strategy-driven E2E/UI plans, uses live-cluster UI verification, and does not provide sealed differential execution, mutation, or signed provenance | Useful Plan/Code-stage source, but an integration layer must supply broader evidence and execution controls |
| Internal test-plan-generator | Reported meeting notes, not independently verified | Excessive plan text, low adoption, no automatic obsolete-case deletion, ambiguity when impacted repositories are not declared | Concise artifacts, explicit repository inputs, and test retirement must be first-class requirements |

## `ai-test-harness`: observed failure examples

The repository's failure register states that each entry was observed in a real run and has a detector, correction, and self-check. Representative examples:

| Register entry | Failure | What it demonstrates |
|---|---|---|
| GF-003 | Catch-all assertions and assertion-free tests passed both versions | Compilation and green status do not establish useful assertions; assertion-quality gates are required |
| GF-005 | A CVE test targeted the wrong function and passed on both versions | Short advisory summaries are insufficient; generation needs source/fix diffs and affected-symbol evidence |
| GF-006 | A CVE test imported a name present only in the fixed version and could not collect on the vulnerable version | Differential generation must validate collection against both environments |
| GF-010 | The old dependency environment was constructed by swapping one package into the head graph and could not resolve | Differential environments must be real base/candidate resolutions, not synthetic swaps |
| GF-014 | A fix-pinning trigger reached the fix, but the generated assertion expected the wrong exception | A correct trigger can still produce an incorrect oracle; execution triage must distinguish trigger success from assertion correctness |
| GF-017 | Tests asserted only `callable`, `isinstance`, `issubclass`, `is not None`, or `hasattr` | Existence and shape assertions can pass while proving no behavior |
| GF-020 | A file that never collected shipped with tests marked kept | Candidate acceptance must require that the exact submitted text collected and ran |
| GF-023 | The model submitted text that had never been run; a shape-only gate accepted it | Provenance must bind submission to the exact executed content |
| GF-027 | A later failed repair erased an earlier attempt that had collected and run | Repair loops need fallback semantics and must preserve the last valid evidence |
| GF-028 | Mutation sampled unreachable Go sites and reported a zero score | Mutation results must be scoped to executed code, or they misrepresent test strength |

These are not hypothetical concerns. They show why the AI Test Generation service should wrap generation with deterministic collection, assertion, differential, mutation, and provenance gates rather than trust model output or a passing command alone.

## `fullsend-ai/test-creator`: scope and workflow limitations

The public README and skills describe a capable strategy-to-test-plan workflow, but its documented scope is narrower than the proposed AI Test Generation service:

- It is centered on RHAISTRAT/RHOAIENG Jira strategies and RHOAI test-plan artifacts.
- It focuses on E2E/UI-oriented planning; `/test-plan-case-implement` skips `TC-UI-*` cases and targets backend pytest automation by default.
- UI verification uses Playwright against a live ODH/RHOAI cluster, not a sealed differential sandbox.
- The documented workflow does not provide the `ai-test-harness`-style base/candidate dependency differential, mutation analysis, signed test provenance, or failure-register self-verification.
- Quality scoring is strengthened by deterministic evidence gates, but the underlying semantic analysis and code generation remain model-assisted.
- Publication is GitHub/fork-oriented and requires Jira/GitHub credentials and repository-specific context.

These are scope boundaries, not defects. They suggest possible reuse for a strategy input driver and Plan/Code stages, while leaving execution isolation, broader input drivers, differential validation, provenance, and Lightwell integration as separate concerns.

## Internal test-plan-generator: reported operational limitations

The ADLC meeting notes are Gemini-generated and require transcript verification. They report:

- Generated plans are too verbose for regular review; at least one reviewer reportedly pastes them into another LLM instead of reading them directly.
- User adoption remains low despite feedback efforts.
- Obsolete test cases can be updated when discrepancies are found, but automatic deletion or retirement is not implemented.
- Running test-plan generation in parallel with implementation preserves independence, but test generation becomes ambiguous unless the strategy declares all impacted teams and repositories.
- The CI orchestrator and test-plan data repositories are internal and the migration from agentic CI to Fullsendrunner was unresolved at the time of the notes.

These reports support design requirements for concise review artifacts, explicit repository-impact inputs, lifecycle retirement proposals, and observable adoption metrics. They should not be treated as verified product facts until the recording or repositories are reviewed.

## Implications for implementation options

### Build or assemble

Existing failures argue for building a reliability envelope around reusable components:

- Deterministic preflight and collection checks.
- Strong assertion and evidence gates.
- Exact-content execution provenance.
- Real base/candidate environment construction.
- Failure-register self-checks.
- Scope-aware mutation and differential results.

### Direct integration

Direct integration can reduce duplicated functionality, but compatibility must be tested against the reliability requirements above. Passing a repository's existing tests does not demonstrate that it supplies these controls.

### Reference-only

A clean implementation can avoid inherited constraints, but it should still reproduce the proven controls and failure cases rather than repeat them unknowingly.

## Evidence gaps still requiring investigation

- License and maintenance status for `fullsend-ai/test-creator`.
- Internal repository access and source review for the agentic-CI orchestrator and test-plans-data contracts.
- Transcript verification for the ADLC meeting notes.
- Whether the internal tools use additional third-party components not visible in the public repository README.
- Which reliability controls are already implemented in internal CI but absent from the notes.
