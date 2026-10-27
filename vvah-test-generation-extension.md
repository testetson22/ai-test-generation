# VVAH Test-Generation Extension Map

## Purpose

Map how Visa Vulnerability Agentic Harness (VVAH) could inform or support AI Test Generation without modifying the VVAH repository or assuming that its current security-validation stages already provide generic test generation.

Reference repository: <https://github.com/visa/visa-vulnerability-agentic-harness>

Local initial assessment: `vvah-context-graph-notes`.

## Current VVAH Capability

VVAH is an Apache-2.0 application-security pipeline with three broad phases:

```text
Discover (S0-S9) -> Remediate (S10) -> Validate (S11)
```

Relevant current capabilities:

- Deterministic source/sink discovery.
- A per-scan Context Graph containing symbols, calls, imports, and communities.
- Agentic threat modeling and deep-dive investigation.
- Deterministic finding validation, deduplication, ranking, and typed report output.
- External finding injection through `inject_findings`.
- Optional live HTTP exploit verification for a local target.
- Remediation proposal/application through S10.
- Read-only adversarial validation of proposed fixes through S11.
- Markdown, SARIF, and typed `findings.json` outputs.
- Skills and hard gates for evidence requirements.

The current boundary is important:

- VVAH's Context Graph is a security reachability engine, not a test mapper.
- S11 reviews fixes statically and does not run unit or integration tests.
- Live exploit verification is HTTP/API-oriented and does not cover generic library or component tests.
- S10 may edit target source; AI Test Generation must keep test generation independent from remediation.

## Reusable Evidence

| VVAH output | Test-generation use |
|---|---|
| `findings.json` | Vulnerability-driven input driver and test objective source |
| Affected file, line, CWE, and code evidence | Candidate test target and oracle context |
| Context Graph symbols and call paths | Map affected functions to application call sites and likely test owners |
| Source-to-sink reachability | Identify trigger paths and prioritize exposure tests |
| Finding exploit scenario and preconditions | Seed test inputs, setup, and expected security behavior |
| S6 adversarial rationale | Plan negative, hostile, or boundary tests |
| S6 confidence and CVSS | Test-generation budget and human-review priority |
| S8 ranking and attack chains | Order objectives and identify multi-step behavior |
| `inject_findings` | Accept Lightwell or other scanner findings without requiring VVAH discovery |
| Local HTTP exploit evidence | Candidate runtime-validation evidence for API-facing vulnerabilities |
| S9 Markdown/SARIF/typed reports | Review packet inputs and evidence references |

VVAH evidence should remain authoritative for what VVAH established. AI Test Generation should reference it and add its own test evidence; it should not rewrite the VVAH finding or promote static reachability to exploitability.

## Context Graph as a Reusable Test-Planning Utility

The Context Graph should be evaluated as a general test-planning utility rather than treated as inherently tied to vulnerability scanning or VVAH. For the first cut, its primary purpose is to show which existing tests exercise affected code and which affected paths have no corresponding test coverage.

The first-cut analysis can use an assessed vulnerability or code change as its target, without defining a broader input-driver taxonomy. Potential outputs include:

- Changed or affected symbols.
- Application-owned callers and call paths.
- Existing tests that reference affected symbols.
- Candidate test levels and likely fixtures.
- Untested affected paths and behaviors.
- Ownership or repository boundaries.
- Unresolved edges and analysis limitations.

```text
Repository/build context
  -> Context Graph
  -> affected symbols and call paths
  -> existing test references
  -> candidate test targets and gaps
  -> test plan
```

The graph does not prove that a test executes a function. Runtime coverage, trace evidence, or an executed test result is still required. Its first-cut role is to improve existing-test mapping and identify untested paths before generation.

VVAH is one potential graph provider. Other options include language-server/compiler adapters, SCIP-derived facts, or an independent graph service. These providers should be evaluated against symbol identity, language coverage, dynamic dispatch, generated code, freshness, provenance, precision, recall, and abstention behavior.

## Proposed Extension Boundary

The lowest-risk design is a downstream adapter:

```text
VVAH S9 findings.json / Lightwell assessment
  -> Vulnerability Test Driver
  -> AI Test Generation Analyze
  -> Plan exposure/fix-pinning/characterization tests
  -> Generate native tests
  -> Execute in approved sandbox or representative environment
  -> Evaluate and report test evidence
```

This keeps VVAH's security pipeline unchanged and lets AI Test Generation consume findings from VVAH, Lightwell, or other sources.

A deeper integration could add optional test-generation stages after S9, but that should be evaluated only after the downstream adapter proves useful:

```text
S9 report
  -> T1 test-target mapping
  -> T2 test planning
  -> T3 test generation
  -> T4 isolated execution
  -> T5 test-evidence evaluation
  -> T6 test report/provenance
```

S10 remediation and S11 fix validation should remain separate. A generated test may be supplied to remediation, and a remediation candidate may return for validation, but the fixer must not be the sole validator of its own change.

## Context Graph Extension

The Context Graph already has much of the structural data needed to improve test planning:

- Symbol definitions and references.
- Caller/callee relationships.
- Imported packages.
- Communities and clusters.
- Source-to-sink paths.
- Application paths reaching affected functions.

A test-target mapping stage could use this to:

1. Resolve the affected symbol or sink from a finding.
2. Traverse incoming callers to find application-owned call sites.
3. Identify existing test references to those symbols.
4. Determine whether tests are unit, component, integration, or E2E level.
5. Compare changed or affected functions with existing test coverage.
6. Produce candidate test targets and missing-coverage evidence.

The graph does not by itself prove that a test executes a function. Runtime coverage, trace evidence, or an executed test result is still required.

### Potential graph query contract

```text
map_finding_to_tests(
  finding,
  repository_profile,
  source_revision,
  test_inventory
) -> TestTargetMap
```

Potential output:

```text
TestTargetMap
  finding_id
  affected_symbols
  application_callers
  existing_test_files
  candidate_test_levels
  path_witnesses
  unresolved_edges
  limitations
  confidence
```

## VVAH Stage-to-Test Mapping

| VVAH stage | Current role | Possible test-generation extension |
|---|---|---|
| S0/S1 | Deterministic source/sink and repository context | Reuse repository scope and language/build context |
| Context Graph | Symbols, calls, imports, communities | Map affected functions to call sites and existing tests |
| S2 | Threat model | Convert threat hypotheses into test objectives and hostile-input categories |
| S3 | Investigation units | Convert units into bounded test-planning work items |
| S4 | Deep-dive security analysis | Extract exploit preconditions, assumptions, and expected security behavior |
| S5 | Deterministic finding validation | Validate test target paths, source lines, and evidence references |
| S6 | Adversarial finding verification | Seed exposure-test and fix-pinning objectives; retain confidence and limitations |
| S7/S8 | Deduplication, ranking, attack chains | Deduplicate test objectives and prioritize multi-step validation |
| S9 | Typed reports and artifacts | Provide input to the Vulnerability Test Driver and Lightwell evidence workflow |
| S10 | Remediation changes | Separate consumer of generated tests; do not combine with test generation |
| S11 | Static fix review, no tests | Potentially augment with generated-test results, but preserve independent test execution |
| EV | Local HTTP exploit confirmation | Supply runtime evidence for API-facing tests; not a generic test runner |

## Test-Generation Stages for a VVAH Finding

### T1: Test-target mapping

Inputs:

- VVAH finding or injected scanner finding.
- Context Graph reference.
- Repository profile.
- Source and artifact revision.
- Existing test inventory.

Outputs:

- Affected symbols and application callers.
- Existing tests that exercise the path.
- Candidate test level.
- Unresolved graph edges and limitations.

### T2: Test planning

For each finding, produce one or more explicit objectives:

- Exposure test at an application call site.
- Fix-pinning test across vulnerable and fixed versions.
- Regression test for a confirmed defect.
- Negative or hostile-input test.
- Characterization test when exploitability cannot be established.
- Runtime simulation request when a representative environment is available.

The plan must record when no safe or meaningful test can be generated.

### T3: Native test generation

Generate tests using the target repository's framework and existing patterns. VVAH findings provide security context, but repository discovery determines:

- File placement.
- Fixtures and setup.
- Test commands.
- Authentication and service dependencies.
- Assertions and expected errors.
- Cleanup and environment policy.

### T4: Isolated execution

Execute generated tests in an approved environment:

- Library-level sandbox.
- Application call-site environment.
- Component environment.
- Deployment-shaped simulation.
- Explicitly authorized HTTP exploit validation where applicable.

VVAH's live HTTP exploit path can be one adapter, not the universal execution method.

### T5: Evaluation

Quality gates should establish that the test:

- Collects and executes.
- Exercises the intended affected function or behavior.
- Uses the intended trigger or input.
- Distinguishes vulnerable and fixed behavior where applicable.
- Does not merely assert existence or broad failure.
- Is not redundant with existing tests.
- Produces evidence bound to exact source, artifact, environment, and test content.

Golden references, differential results, mutation sensitivity, and human review should complement VVAH confidence and exploit evidence.

### T6: Evidence output

Return a test-evidence record that references, rather than replaces:

- VVAH finding ID.
- VVAH report and path-witness IDs.
- Source and artifact revisions.
- Generated test identity and hash.
- Environment and sandbox identity.
- Execution result and logs.
- Coverage/path-observation evidence.
- Limitations and result state.

## Integration Options

### Option A: Downstream adapter

AI Test Generation consumes VVAH `findings.json` or Lightwell assessment artifacts.

**Best fit:** initial feasibility study and POC.

**Advantages:** no remote changes, preserves VVAH boundaries, works with other scanners, and isolates test-generation failure from security scanning.

**Limitations:** requires stable finding/evidence contracts and separate graph-to-test mapping.

### Option B: VVAH plugin or custom stage

Add a test-generation integration through VVAH's plugin or stage extension points.

**Advantages:** direct access to Context Graph and finding lifecycle.

**Limitations:** tighter coupling to VVAH pipeline, lifecycle, dependencies, and release cadence; requires upstream coordination and should not be assumed available without source/API review.

### Option C: Shared Context Graph adapter

Consume the graph or graph-derived artifacts independently through a supported SDK or exported evidence format.

**Advantages:** reusable mapping capability without coupling to the full VVAH pipeline.

**Limitations:** graph schema, SDK stability, language coverage, provenance, and license/maintenance must be verified.

### Option D: Reference-only

Use VVAH concepts and evidence patterns while implementing independent graph and test-target mapping.

**Advantages:** maximum control.

**Limitations:** repeats significant graph and security-analysis work.

## Current Recommendation for Evaluation

No adoption decision is made here. The most contained experiment is to use VVAH as an upstream finding/evidence source through a local fixture or exported `findings.json`, then implement T1 test-target mapping and T2 test planning outside VVAH.

The experiment should answer:

- Can VVAH affected symbols and path witnesses identify useful application call sites?
- Can those call sites be mapped to existing unit/component/integration tests?
- Can S6 exploit preconditions become useful test objectives without overclaiming exploitability?
- Does the Context Graph provide enough precision and coverage for test planning?
- What evidence must be added for generated tests to prove they exercised the target?

This is an evaluation plan, not a decision to modify, fork, or consume the remote VVAH repository.
