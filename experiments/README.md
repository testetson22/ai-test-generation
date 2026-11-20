# Experiments

This directory contains temporary research and comparison work. Current candidates include the legacy demo's Aider/Claude Code task, `ai-test-harness`, `fullsend-ai/test-creator`, internal test-plan tooling, direct SDKs, LangGraph, Pydantic AI, and bounded VVAH/evaluation experiments.

Experiments may recommend reuse, fork, reproduction, or replacement. They must not become hidden runtime dependencies or change domain contracts to fit a candidate framework.

Each experiment records source revision, license/ownership status, capabilities tested, local reproduction command, results, limitations, and a removal or promotion decision.

## Working Around Pending Spike Decisions

FLPATH-5050 evaluates implementation harnesses; FLPATH-5051 evaluates the physical
service/provider split. Neither recommendation needs to be complete before
independent controller and task-contract implementation begins. Stub the
replaceable implementation behind an explicit boundary, not the correctness of
its recommendation. Controller lifecycle/retry authority remains agreed behavior.

| Pending decision | Stable boundary to implement | Required test substitute | Still requires real evidence |
|---|---|---|---|
| FLPATH-5050: agent harness/model integration | Authorized task/attempt specification, bounded inputs/tool capabilities/budgets, and attributable outputs, failures, usage, and artifact references | Deterministic task runner or replay adapter producing valid, malformed, interrupted, unsupported, and budget-exhausted outcomes | Candidate licensing/security/maintenance/fit, real model/tool behavior, and live output quality |
| FLPATH-5051: provider and physical responsibility split | Submission token/specification binding, submit, observe/reconcile, cancel, and cleanup with explicit capability and durability semantics | Stateful job-provider simulator covering acceptance followed by lost acknowledgement, duplicate submission, unknown outcome, partial completion, cancellation, and cleanup failure | Actual platform guarantees, isolation, permissions, limits, cancellation, artifact survival, and operational recovery |

The existing Python `GenerationPort` and deterministic generation adapter provide
a starting reference for the first boundary, not the complete asynchronous task
protocol. Do not expose Pydantic AI, LangGraph, or another harness's graph,
checkpoint, retry, or SDK objects as canonical ATG state. Select a production
component's language/build when implementing it; a substitute does not remove
that decision or justify a mandatory hidden Python reference service.

Keep simulated provider state independent of the controller's journal so a
controller restart does not erase accepted fake jobs. Share contract/conformance
cases with real adapters; assert launch counts, identities, failure behavior, and
durability/cleanup ordering, not a fake's unconditional success response.

For a Tekton candidate, add pinned resource/API fixtures and adapter-level tests
between the domain-level simulator and real cluster integration. See the
[Tekton boundary](../deploy/development-harness/README.md#provisional-tekton-boundary).

Record provisional assumptions, supported capabilities, limitations, and the
conditions for replacing each substitute. Later spike findings should normally
change adapter/configuration code. If they expose a missing semantic guarantee,
revise the boundary explicitly with compatibility tests rather than hiding it.
Test substitutes are injected only in test/development composition, clearly
identified as simulated evidence, and never a production fallback or grounds for
closing a spike. Missing production adapters fail readiness/dispatch explicitly.

## FLPATH-5050 Campaign Findings (2026-11-20)

The [two-task campaign and Aider diagnostic findings](https://raw.githubusercontent.com/vkolodny/ai-test-generation/refs/heads/flpath-5050-fake-model-replay/experiments/harness/FLPATH-5050-campaign-findings.md)
are experiment evidence, not a harness selection. The report says all formal
candidate decisions remain `insufficient_evidence`; its retained archives are
host-local and are not copied into this repository.

FLPATH-5050 is ready to close as an investigation spike with these findings and
limitations recorded. Harness assessment will continue after the campaign
changes are merged. The follow-ups below are subsequent assessment work, not
blockers to spike closure; closing the spike does not select or approve a
production harness or change the report's candidate dispositions.

- Each tool had one attempt per task against the same model endpoint/model. All
  four candidates discriminated the Quarkus defect. Pydantic AI, Claude Code,
  and LangGraph discriminated the Spring defect; Aider timed out during Analyze
  in that run.
- Aider later discriminated the Spring defect in a diagnostic, but the changed
  run specification and evaluator make it a separate provenance cohort, not a
  fixed-configuration repeat or reliability estimate. The reported repeated
  Analyze log blocks still need investigation.
- The Spring coverage target must be `parseAndCreateTransactions`, where the
  changed condition resides, rather than `importStatement`. The corrected
  archived-XML analysis reports 45/45 lines and 25/28 branches before and after
  each successful test (zero delta). This does not negate defect detection:
  the prior suite executed the branch without asserting the lower-case behavior.
  The original campaign remains `coverage_unknown`; derived XML analysis does
  not rewrite its raw result. The Quarkus fixture has no method-coverage
  contract, so coverage is `not_applicable` there.
- QE's semantic test review is separate from the campaign machine report's
  pending semantic-review status. Named formal decisions with authorization
  references remain outstanding. The common model also means the campaign does
  not compare model quality.
- Follow-ups are to investigate the Aider Analyze loop and repeat under a fixed
  configuration, use the corrected Spring coverage target, and add another
  distinct task or repository before recording formal candidate decisions.
- The security review found no blocker for these four candidates. Before a
  future third-party adapter-image run, review and allowlist the exact image and
  restrict egress or use an experiment-scoped credential; the referenced
  `run_comparison.py` passes `ENMAAS_API_KEY` to `/adapter` with host networking.

Continue the post-merge assessment behind the existing provider-neutral harness
boundary, retaining deterministic replay substitutes for test/development use.
Production harness selection remains a separate decision; do not promote any
campaign candidate into runtime composition based on this report alone.
