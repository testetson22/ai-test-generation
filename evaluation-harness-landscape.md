# Evaluation-Harness Landscape

Date: Oct 23, 2026

## Purpose

This document assesses external benchmarks and harnesses as evaluation inputs for the AI Test Generation effort. They are evaluation infrastructure, not production dependencies and not substitutes for the Analyze -> Plan -> Code -> Execute lifecycle.

The central question is not whether a tool can run code. It is whether it supplies an independent oracle for a generated test, a reproducible environment, and evidence that can be mapped to the requested objective.

## Assessment Summary

| Resource | Primary oracle | Best use here | Fit | Current evidence |
|---|---|---|---|---|
| [CyberGym](https://github.com/sunblaze-ucb/cybergym) | Task-specific vulnerable/fixed PoC differential result with recorded exit codes | Vulnerability-driver differential evaluation and sandbox design reference | High for security PoCs; low for native framework generation | Verified from repository README |
| [CyberGym-E2E](https://github.com/sunblaze-ucb/cybergym-e2e) | Agent PoC crashes pre-patch, does not crash post-patch; patch and project-suite checks | End-to-end vulnerability discovery, PoC, and patch research track | Medium for security workflow research; not a direct test-generation corpus | Verified from repository README |
| [Defects4J](https://github.com/rjust/defects4j) | Triggering test fails on buggy revision and passes on fixed revision | Real-fault regression validation and mutation/coverage experiments | High for Java regression tests | Verified from repository README |
| [BugsInPy](https://github.com/soarsmu/BugsInPy) | Buggy/fixed checkouts with test, coverage, mutation, and fuzz commands | Python regression-test validation | High for Python, subject to environment reproducibility | Verified from repository README |
| [SWT-Bench](https://github.com/logic-star-ai/swt-bench) | Generated test exposes an issue in the original state and passes after the patch | Closest external benchmark for repository-level test generation | High for Python issue-to-test generation | Verified from repository README |
| [SWE-bench Verified](https://github.com/SWE-bench/SWE-bench) | Candidate patch passes the issue-specific and regression test suite | Comparator for change-driven repair, not test generation | Medium as a test-generation evaluation source | Verified from repository README |
| [Magma](https://github.com/HexHive/magma) | Ground-truth bug canary is detected by a fuzzing campaign | Fuzz-target and bug-detection validation | Medium for fuzzing-specific adapters; low for native tests | Verified from repository README/repository description |

"Verified" means the capability and stated oracle were checked against the project's current public documentation. It does not mean that the benchmark has been executed locally, that its current data is uncontaminated, or that it meets Lightwell licensing and operational requirements.

## CyberGym Assessment

### What it provides

CyberGym is a large-scale benchmark for AI agents performing real-world vulnerability analysis. The public setup provides Docker-based task environments, vulnerable source archives, task descriptions, masked fix information, a local PoC submission server, and optional network isolation through an allowlist proxy. The documented dataset is large; the project provides a ten-task subset for initial evaluation and distinguishes binary-only from full compilation data modes.

The important oracle is differential: a submitted PoC is evaluated against vulnerable and fixed builds. The result includes separate vulnerable and fixed exit codes. This is materially stronger than a green test run because it checks that the generated trigger distinguishes the known faulty and corrected behavior.

### Fit to AI Test Generation

CyberGym should be treated as an **external security-driver evaluation harness**. It can validate these parts of the proposed system:

- Whether a vulnerability-oriented planner produces a trigger that reaches the intended behavior.
- Whether the execution layer preserves vulnerable/fixed roles and reports both outcomes.
- Whether isolation, egress restrictions, artifact identity, and execution provenance are enforced.
- Whether model, prompt, and adapter changes regress on a fixed external holdout.

It does not validate the complete native-framework generation path. The documented corpus is primarily C/C++ memory-safety material derived from real vulnerability datasets and fuzzing contexts. A successful CyberGym PoC therefore demonstrates a security PoC capability under the benchmark's harness; it does not demonstrate correct pytest, Go, Ginkgo, JUnit, or application-level test generation.

CyberGym-E2E extends the research question from PoC generation to vulnerability discovery and patch generation. Its documented modes and four-stage validation make it useful for a separately approved end-to-end security-agent study, but its patch objective must not be confused with the service's boundary: AI Test Generation produces tests and evidence and does not modify product code or certify a patch.

### Adoption recommendation

Use CyberGym in the evaluation track, not the production execution path:

1. Start with the ten-task subset, subject to local resource and architecture checks.
2. Pin task, image, dataset, and benchmark revisions; keep prompt-development tasks separate from the holdout.
3. Record the exact PoC bytes, vulnerable/fixed artifact identities, exit codes, logs, tool versions, and network policy in the service provenance record.
4. Map benchmark outcomes to service states without collapsing them into security conclusions:

| CyberGym observation | Service interpretation |
|---|---|
| Vulnerable behavior demonstrated and fixed behavior not demonstrated | `reproduced` or `inconclusive`, depending on the expected fixed oracle |
| Same result on both builds | `inconclusive` or `not_reached`; never proof that the advisory is irrelevant |
| PoC or environment cannot run | `blocked` or `environment_mismatch` |
| Fixed build shows the expected non-vulnerable behavior | `fixed_behavior` for the tested artifact and trigger |

The exact mapping needs a task-specific oracle because process exit codes are not a universal statement of behavior. A command returning zero on both builds is not, by itself, evidence of a failed security claim; it may indicate a weak PoC, a non-crashing vulnerability, or an incorrect expected result.

## Comparison With Similar Tools

### Regression-fault datasets: Defects4J and BugsInPy

Defects4J is the most mature fit among the listed tools for controlled regression evaluation. Its public metadata identifies buggy and fixed revisions, triggering tests, changed classes, and commands for checkout, compilation, testing, coverage, and mutation. It is Java-specific and has documented version/timezone reproducibility constraints. It should be used to test the evaluator and differential semantics, not to imply Java coverage for other ecosystems.

BugsInPy provides the analogous Python-oriented workflow with buggy/fixed project checkouts and commands for compilation, tests, coverage, mutation, and fuzzing. It is a useful Python slice for the initial service because it can exercise native pytest-compatible generation and real-fault detection. Its project environments and historical dependencies require a reproducibility trial before it becomes a CI gate.

### Repository-level test generation: SWT-Bench

SWT-Bench is the closest match to the requested test-generation objective. Given a codebase and issue, the agent generates a reproducing test. The documented harness supports unit-test mode, where predictions are integrated with the existing suite, and reproduction-script mode. It reports applicability, success, pass/fail transitions, and coverage deltas, and publishes Lite, Verified, and full datasets.

SWT-Bench should be the primary external benchmark for the Plan -> Code -> Execute path for Python issue-driven tests. Its limitations must remain explicit: it is issue/repository centered, predominantly Python, and its success oracle is tied to the benchmark's patch and test setup. It does not cover vulnerability applicability evidence, Lightwell contracts, multi-language adapters, production-shaped environments, or the full range of user objectives.

### Change-driven repair: SWE-bench Verified

SWE-bench evaluates generated patches for real GitHub issues in Dockerized environments. It is valuable as a comparator for issue understanding, repository navigation, and regression-suite execution, and its Verified subset provides a curated evaluation slice. It is not a direct test-generation benchmark because the primary output is a patch. It may be used only where the AI Test Generation experiment explicitly measures whether a generated test detects or validates a known change; patch success must remain a separate metric.

### Ground-truth fuzzing: Magma

Magma supplies real programs with real bugs and ground-truth fuzzing signals. It is a useful validation adapter for fuzz-target generation, crash detection, and bug-canary sensitivity. It is not a substitute for native unit or integration tests. Its results should be reported as fuzzing evidence with campaign configuration, target, seed corpus, sanitizer, timeout, and canary identity.

## Evaluation Design Implications

The tools cover different axes and should not be merged into one score:

| Axis | Candidate harnesses | Suggested metric |
|---|---|---|
| Vulnerability PoC differential correctness | CyberGym | Vulnerable/fixed discrimination rate, with blocked and inconclusive rates |
| Vulnerability discovery and patch workflow | CyberGym-E2E | PoC, patch, and suite stages reported separately |
| Real-fault regression detection | Defects4J, BugsInPy | Bug detection rate, collection rate, mutation/coverage context |
| Issue-to-test generation | SWT-Bench | Applicability, reproduction success, pass-to-pass and coverage deltas |
| Change-driven repository repair | SWE-bench Verified | Patch resolution and regression outcomes, not test-generation score |
| Fuzz-target sensitivity | Magma | Ground-truth canary detection under a declared campaign |

Every evaluation run should also record generated-test validity, exact-content execution, collection failures, assertion quality, flakiness, environment setup failures, cost, and latency. Benchmark success must not hide these failure classes.

The recommended initial suite is a small, version-pinned CyberGym subset, a BugsInPy slice, a Defects4J slice only if Java support is in scope, and an SWT-Bench Verified/Lite slice. Keep a separate holdout for final comparison. Add Magma only when a fuzzing adapter is being evaluated. Do not use benchmark tasks as prompt-development examples if they are later used for headline evaluation.

## Risks and Open Verification

- Benchmark data can enter prompts or model training; contamination and memorization controls are required.
- Large datasets, Docker images, x86 assumptions, and ARM support limitations affect cost and reproducibility.
- Public benchmark metadata does not establish current maintenance, license compatibility with a product deployment, or suitability for confidential vulnerability data.
- A benchmark's gold test or PoC can validate a known task without proving that the generated test is maintainable or appropriate for a customer repository.
- External benchmark outcomes cannot establish production exploitability, VEX status, or release approval.

Before promoting any harness to a CI quality gate, run a local reproducibility trial, pin dependencies and images, verify licensing and retention policy, measure runtime/cost, inspect task overlap with training and prompt corpora, and document the result-state mapping.

## Sources

- CyberGym README and FAQ: https://github.com/sunblaze-ucb/cybergym
- CyberGym-E2E README: https://github.com/sunblaze-ucb/cybergym-e2e
- Defects4J README: https://github.com/rjust/defects4j
- BugsInPy README: https://github.com/soarsmu/BugsInPy
- SWT-Bench README and evaluation harness: https://github.com/logic-star-ai/swt-bench
- SWE-bench README and harness: https://github.com/SWE-bench/SWE-bench
- Magma repository: https://github.com/HexHive/magma
