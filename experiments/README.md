# Experiments

This directory contains temporary research and comparison work. Current candidates include the SSC Aider/Claude Code task, `ai-test-harness`, `fullsend-ai/test-creator`, internal test-plan tooling, direct SDKs, LangGraph, Pydantic AI, and bounded VVAH/evaluation experiments.

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
