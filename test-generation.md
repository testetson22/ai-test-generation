# AI Test Generation

## 1. Purpose

This document translates the **AI Test Harness Business Case** into a focused technical and delivery proposal for AI-assisted test generation as an integration capability for the Red Hat Lightwell platform.

The system generates tests for application changes and dependency upgrades, runs them in an isolated environment, evaluates whether they prove useful behavior, and presents the evidence to a human reviewer. Accepted tests enter the repository only through a normal pull request.

The system is not an autonomous release system and is not a replacement for product-team ownership of requirements, test intent, or merge decisions.

Lightwell is the surrounding platform context for vulnerability, SBOM, artifact, evidence, workflow, and security integrations. This capability should consume Lightwell-owned evidence through stable contracts rather than recreate those platform services.

## 2. Problem

Teams ship substantial amounts of code they did not directly author:

- Transitive dependencies are numerous and rarely tested against application-specific behavior.
- Dependency upgrades are evaluated through changelogs, existing suites, and manual judgment rather than differential evidence.
- Security advisories identify affected versions but do not prove whether the application reaches the vulnerable behavior.
- Existing tests often cover the application’s happy path but not dependency-specific regressions, malformed inputs, boundary conditions, or failure behavior.
- Test generation is slow and inconsistent across Python, Go, TypeScript, shell, infrastructure, and UI repositories.
- Test results and review decisions are rarely captured as portable, signed evidence.

The business problem is therefore not simply “write more tests.” It is:

> Generate the smallest useful set of tests that proves whether a change or dependency version affects the behavior this application relies on, and provide evidence that a reviewer can trust.

## 3. Proposed Solution

For a pull request or dependency change, the harness:

1. Resolves the repository, changed files, dependency graph, and relevant advisories.
2. Reads repository instructions, existing tests, fixtures, CI commands, and behavioral contracts.
3. Builds a test plan from requirements, bug reports, API schemas, and dependency behavior.
4. Generates tests in the repository’s native framework.
5. Runs focused tests, related suites, and differential tests against base and candidate revisions.
6. Evaluates assertion quality, mutation sensitivity, determinism, cleanup, and redundancy.
7. Produces a review packet and a test-only pull request.
8. Records a signed evidence bundle tied to source revisions, container images, test commands, and decisions.

The harness may propose a test, but it does not merge it, publish a release, modify production infrastructure, or use production secrets.

Every behaviour a change reaches must end in one of three states: validated by a test that ran on both sides of the change, handed to the team for execution in their environment, or a stated gap with its reason. Nothing is silent, and nothing is claimed that was not proven.

## 4. What “AI-Generated Test” Means

A generated test is acceptable only when it has all of the following:

- A named requirement, bug, advisory, contract, or explicitly approved characterization goal.
- A defined test level: unit, component, integration, contract, or E2E.
- A setup, action, oracle, and cleanup description.
- Assertions that distinguish correct behavior from incorrect behavior.
- A repository-native implementation using existing fixtures and conventions.
- A reproducible execution command.
- Evidence that the test is not redundant with existing coverage.

Generated code that merely increases line coverage, repeats an existing test, or asserts that an object exists is not considered successful output.

## 5. Supported Use Cases

### 5.1 Pull request test generation

Input:

- PR diff and description.
- Acceptance criteria or linked issue.
- Existing tests near changed code.

Output:

- Tests for changed behavior.
- Negative and boundary cases identified from the contract.
- A test plan mapped to the PR requirements.

### 5.2 Dependency upgrade testing

Input:

- Base revision with dependency version A.
- Candidate revision with dependency version B.
- Dependency manifest and lockfile.
- Advisory, changelog, or reported behavior change.

Output:

- Characterization tests for behavior the application relies on.
- A base-versus-candidate comparison.
- A classification: fixed, regressed, unchanged, inconclusive, or not reachable.

### 5.3 Vulnerability-targeted testing

Input:

- Advisory identifier and affected package/version range.
- Application dependency graph.
- Reachability evidence where available.

Output:

- A test that exercises the relevant behavior, when feasible.
- Evidence of behavior on the vulnerable and remediated versions.
- A gap report when the behavior cannot be reproduced or safely exercised.
- Optional VEX draft backed by the test result, never by the advisory alone.

### 5.4 Repository test-gap analysis

Input:

- Existing test inventory.
- Public APIs, schemas, and changed code.
- CI and test configuration.

Output:

- Missing scenario categories.
- Weak or non-discriminating assertions.
- Uncovered changed behavior.
- Recommended tests ranked by risk and expected value.

## 6. End-to-End Workflow

```text
Trigger
  -> Discover repository and dependency context
  -> Build test plan
  -> Human plan review when required
  -> Generate native tests
  -> Run focused validation
  -> Run related suite
  -> Run differential/mutation/stability checks
  -> Produce evidence and review packet
  -> Open test-only PR
```

## 6.1 Interactive Stage Model

The harness should expose four user-visible stages rather than one opaque generation request:

```text
Analyze -> Plan -> Code -> Execute
    ^        ^       ^        ^
    |        |       |        |
  feedback feedback feedback feedback
```

Each stage may produce a checkpoint and pause for user guidance. The user can approve, reject, edit, constrain, or add context before the next stage. Feedback is stored as part of the run state and is applied to subsequent turns; it is not merely appended to an ephemeral prompt.

| Stage | Agent responsibility | User can provide | Checkpoint output |
|---|---|---|---|
| **Analyze** | Discover repository conventions, existing coverage, requirements, dependencies, and safety boundaries | Scope, repositories, source documents, known exclusions, risk concerns | Repository profile, findings, assumptions, candidate test gaps |
| **Plan** | Convert requirements into non-duplicative scenarios with setup, action, oracle, level, mock policy, and cleanup | Priority, boundaries, test level, cases to omit, expected behavior corrections | Versioned test plan and coverage matrix |
| **Code** | Implement tests using representative repository patterns and selected fixtures | Naming, file placement, fixture preferences, assertion strictness, implementation constraints | Test diff, generated rationale, unresolved questions |
| **Execute** | Run focused and related tests, collect artifacts, evaluate quality, and classify failures | Commands to permit, environment selection, retry policy, failure interpretation | Results, quality report, mutation/differential evidence, proposed next action |

The user may return to an earlier stage. For example, an execution failure caused by an incorrect oracle should return to **Plan**, while a missing import should return to **Code**. A test failure must not automatically trigger code changes without identifying which stage's assumption failed.

### Stage state

Each checkpoint stores immutable inputs and a mutable working decision set:

```json
{
  "run_id": "atg-2026-00123",
  "stage": "plan",
  "revision": "<git-sha>",
  "inputs": ["issue:FLPATH-0000", "tests/e2e/api_auth_test.go"],
  "decisions": {
    "include": ["expired-token", "wrong-audience"],
    "exclude": ["rate-limiting:covered-by-existing-suite"],
    "test_level": "integration"
  },
  "assumptions": ["401 is the stable public contract"],
  "feedback": [
    {
      "author": "human",
      "text": "Do not add a second test for malformed tokens; TC-44 already covers it.",
      "applies_to": "plan"
    }
  ],
  "artifacts": ["repository-profile.json", "test-plan.md"]
}
```

The system must distinguish user instructions from repository content and model-generated suggestions. Repository files are evidence, not authorization to bypass a user decision or safety policy.

### Interaction controls

- **Approve:** proceed using the current checkpoint.
- **Revise:** apply user feedback and regenerate only the affected artifact.
- **Ask:** request clarification on an unresolved requirement or conflict.
- **Reject:** discard the proposal and record the reason.
- **Skip:** omit an optional stage only when the user explicitly accepts the reduced evidence.
- **Resume:** continue from any saved checkpoint without regenerating accepted artifacts.

The default behavior is pause after Analyze and Plan, then continue Code and Execute only after the plan is approved. Fully unattended operation is an explicit repository policy, not the default.

### Phase A: Discover

The discovery worker reads, but does not modify:

- `AGENTS.md`, `CLAUDE.md`, `.cursor/rules/`, test READMEs, and contribution guidance.
- Language and build manifests.
- Two or three representative tests.
- Fixtures, page objects, builders, mock servers, and cleanup helpers.
- CI commands, test markers, labels, timeouts, and report formats.
- API schemas, protobufs, acceptance criteria, bug reports, and advisory data.

It produces a repository profile containing the supported framework, focused test command, required services, artifact format, and safety constraints.

The discovery process follows existing workspace guidance:

- `agentic-ai-guidance/rules/test-generation-essentials.mdc`: human defines boundaries, agent reads two or three representative tests, agent generates from patterns, human validates.
- `agentic-ai-guidance/prompts/build-test-plan.md`: read existing repository test plans and linked change context before proposing E2E cases; avoid duplicating unit and integration coverage.
- `agentic-ai-guidance/guides/agent-collaboration-patterns.md`: use exploration before implementation and incremental, validated steps rather than a large one-shot change.
- `.continue/rules/dcm-test-plans.md`: for DCM repositories, use `.ai/test-plans/`, linked Jira/PR context, and the documented E2E boundary.

### Phase B: Plan

The planner produces scenarios before test code. Each scenario includes:

| Field | Meaning |
|---|---|
| Requirement | Source behavior to prove |
| Level | Unit, component, integration, contract, or E2E |
| Setup | Fixtures, services, and data required |
| Action | Operation under test |
| Oracle | Exact expected result |
| Boundaries | Empty, minimum, maximum, timeout, malformed, or transition cases |
| Mock policy | Which boundaries are mocked and why |
| Cleanup | State restoration or resource deletion |
| Exclusions | Existing tests or cases intentionally not duplicated |

The plan is sent to a human gate when requirements conflict, the test is destructive, the repository is unfamiliar, or the expected behavior cannot be grounded in an authoritative source.

The plan also applies the workspace test-quality rules: explicit scope, happy and negative paths, edge boundaries, existing-fixture reuse, and a clear distinction between unit, integration, and E2E value. The plan is a durable artifact that can be reviewed independently of the generated code.

### Phase C: Generate

The generator uses the repository’s established framework:

| Repository pattern | Generated output |
|---|---|
| Python/pytest | pytest tests, fixtures, markers, parametrization |
| Go testing | Go tests and test helpers |
| Go/Ginkgo | Ginkgo contexts, tables, Gomega assertions, cleanup hooks |
| TypeScript/Jest/Vitest | Native unit/component tests |
| Playwright | Browser workflows using existing page objects and locators |
| Ansible/Molecule | Role scenarios and state verification |
| Terraform | Validation, plan assertions, and policy checks |

Pytest can orchestrate black-box container tests when that is already the repository convention. It is not imposed as the universal framework.

Code generation uses the examples and rules discovered in Analyze rather than a generic language prompt. For pytest, this includes composed fixtures, parametrization with readable IDs, module-boundary patching, context-manager mocks, and explicit output assertions. For Go/Ginkgo, this includes nested behavioral contexts, `BeforeEach`/`DeferCleanup`, table-driven cases, and Gomega assertions. For Playwright, this includes explicit waits and stable locators.

### Phase D: Execute

Validation is incremental:

1. Format, parse, lint, and compile.
2. Run the generated test alone.
3. Run its package or directory.
4. Run the repository’s required suite.
5. Compare base and candidate revisions where the task is differential.
6. Run mutation testing on targeted behavior where supported.
7. Repeat a stable subset to detect order dependence and flakiness.

The Execute checkpoint separates three outcomes that are often conflated:

- **Test failure:** the test ran and observed a behavior that did not match its oracle.
- **Test/system setup failure:** the environment, fixture, dependency, or command prevented execution.
- **Harness uncertainty:** available evidence is insufficient to classify the result.

Only the first category is evidence that the implementation failed the planned behavior. Setup failures return to Analyze or Code; oracle failures return to Plan; harness uncertainty is surfaced to the user rather than hidden by retries.

## 7. Test Quality Rules

### Assertions

The evaluator rejects:

- Tautologies such as `len(items) >= 0`.
- Broad status classes when a specific code is required.
- Existence-only checks when the expected value is known.
- Conditional assertions with a silent else path.
- Create-without-readback tests when persistence is part of the behavior.
- Inexact cardinality when the expected count is known.
- Assertions that permanently accept known-bad behavior.

### Test reuse and redundancy

The planner inventories existing tests by requirement, endpoint or public symbol, scenario category, boundary, and oracle. A candidate is redundant only when an existing test proves the same behavior at an equal or stronger test level.

Semantic similarity is a discovery aid, not the final decision. Tests with different boundaries, integration surfaces, or failure oracles may both be justified.

### Mocks and real dependencies

Mocks isolate boundaries that are destructive, unavailable, expensive, or inappropriate for a unit test. They do not prove the real provider contract.

- Patch dependencies at the boundary where they are consumed.
- Use interface/spec-constrained mocks so unexpected calls fail.
- Assert that rejected operations do not call destructive methods.
- Use disposable databases, brokers, identity providers, namespaces, and containers for integration tests.
- Use real identity-provider flows for JWT signature, issuer, audience, key-discovery, and rotation behavior.
- Never target production accounts, clusters, Vault paths, or credentials.

## 8. Differential Testing Model

For dependency changes, the harness runs the same planned test against two controlled environments:

| Base result | Candidate result | Meaning |
|---|---|---|
| Fail | Pass | Candidate fixes the tested behavior |
| Pass | Fail | Candidate introduces a regression |
| Pass | Pass | Characterization test; no fix claim |
| Fail | Fail | Test or implementation conflict; investigate |
| Inconclusive | Any | Environment or test limitation |

The harness must not claim a vulnerability is fixed because a package version changed. It must report the behavior the test actually proved.

## 9. Isolation and Security

- Run generation and execution on ephemeral workers or short-lived jobs.
- Treat repository content, generated scripts, and model output as untrusted.
- Restrict network egress to explicitly approved destinations.
- Do not mount host container sockets or grant privileged execution by default.
- Use short-lived, least-privilege test credentials only for disposable environments.
- Keep production Vault tokens and production credentials outside generation and validation workers.
- Apply CPU, memory, process, and wall-clock limits.
- Capture commands, revisions, images, logs, and decisions without recording secret values.

## 10. Review Packet and Evidence

Every run produces:

```text
repository-profile.json
test-plan.json
generated-diff.patch
commands.json
junit.xml
coverage.xml
mutation.json          # when supported
stability.json
review-summary.md
provenance.json
```

The review packet answers:

- What requirement or advisory motivated each test?
- Which existing tests were considered?
- What behavior does each test prove?
- Which real dependencies and mocks were used?
- Did the test fail on the known-bad revision where expected?
- Did it pass without retries, or was it flaky?
- What changed during auto-fix attempts?
- What remains unverified?

Evidence is signed and tied to repository revisions, container image digests, test commands, and the reviewer decision. Evidence publication is allowed; product and release publication remains outside the harness.

## 11. Concrete Examples

### Python validation and persistence boundary

Requirement: `POST /providers` rejects an empty name with HTTP 400 and does not write to the repository.

```python
def test_rejects_empty_provider_name_without_writing(provider_service, provider_repo):
    with pytest.raises(ValidationError, match="name is required"):
        provider_service.create(name="", region="us-east-1")

    provider_repo.create.assert_not_called()
```

The unit test proves early validation. A separate integration test can prove the HTTP status and error contract. The planner should not generate both if existing tests already prove both behaviors.

### Go/Ginkgo authentication regression

Requirement: malformed, expired, wrong-issuer, wrong-audience, and invalid-signature JWTs return HTTP 401.

```go
DescribeTable("rejects invalid bearer tokens",
    func(token string) {
        response := requestCatalog(token)
        DeferCleanup(response.Body.Close)
        Expect(response.StatusCode).To(Equal(http.StatusUnauthorized))
    },
    Entry("malformed", malformedToken),
    Entry("expired", expiredToken),
    Entry("wrong issuer", foreignIssuerToken),
    Entry("wrong audience", wrongAudienceToken),
    Entry("invalid signature", tamperedToken),
)
```

The identity provider is disposable because mocked JWT parsing cannot prove gateway signature or key-discovery behavior.

### Playwright authorization workflow

Requirement: an unauthorized user must not see or invoke an administrative action.

```typescript
test("hides provider deletion from unauthorized users", async ({ unauthorizedPage }) => {
  await unauthorizedPage.goto("/providers/test-provider");

  await expect(unauthorizedPage.getByRole("button", { name: "Delete provider" }))
    .not.toBeVisible();

  const response = await unauthorizedPage.request.delete("/api/providers/test-provider");
  expect(response.status()).toBe(403);
});
```

This proves both UI suppression and server-side authorization.

## 12. Delivery Phases

These phases retain the Business Case timeline while making each phase measurable.

### Phase 1: Pilot, weeks 1–6

Scope:

- Two example repositories: one Python, one Go.
- First-party code and direct dependencies.
- Platform-agnostic pull-request triggers: the pipeline the team already runs.
- Review packet as a PR comment or check.

Deliverables:

- Repository discovery profile.
- Test-plan approval flow.
- Native pytest and Go/Ginkgo adapters.
- Focused generation and isolated execution.
- JUnit, coverage, assertion-quality, and provenance artifacts.

Exit criteria:

- At least 80% of proposed tests compile or collect before human repair.
- At least one real regression or dependency behavior finding.
- Reviewer measures collected first on every run: evidence arrival rate, review time per request, and catches that review alone would have missed.
- Average model inference under 15 seconds per generated suite, false-positive rate under 5% after automated triage, and inference cost under $0.05 per evaluated pull request.
- Feedback from both pilot teams.
- No production credentials or uncontrolled network access.
- No default model or prompt changes without evaluation evidence.

### Phase 2: First-party scale, weeks 7–12

Scope:

- Every pull request in pilot repositories.
- Test overlay repository and test-only pull requests.
- Automatic repository-profile refresh when test conventions change.

Deliverables:

- Duplicate-test detection.
- Test-owner routing.
- Stable CI result normalization.
- Review metrics: acceptance rate, review time, flaky rate, and unique findings.

### Phase 3: Direct dependencies, weeks 13–20

Scope:

- Every direct dependency bump.
- Base-versus-candidate differential execution.
- Advisory-targeted tests and VEX drafts.

Exit criteria:

- Every tested advisory produces a behavioral result or an explicit evidence-backed gap.
- Dependency changes cannot be marked “fixed” from version metadata alone.

### Phase 4: Transitive dependencies, weeks 21–30

Scope:

- Reachability and risk scoring across the dependency graph.
- Cheap characterization snapshots for unreachable or low-risk packages.
- Full test generation only where application behavior reaches the package or advisory.

Guardrail:

- Package count is not a test-generation quota. Effort follows reachability, risk, and evidence value.

### Phase 5: Continuous operation, week 31 onward

Scope:

- Scheduled rescans.
- Additional ecosystems and test frameworks.
- Promotion of high-value generated tests into maintained repository suites.
- Periodic model and prompt evaluation.

## 13. Roles and Human Gates

| Gate | Human decision | Evidence required |
|---|---|---|
| Test-plan gate | Is the behavior and scope correct? | Requirements, scenarios, exclusions, risk flags |
| Test-review gate | Does the generated test prove useful behavior? | Diff, assertions, fixtures, focused results |
| Differential-result gate | Does the base/candidate comparison support the claim? | Reproducible commands and both environment results |
| Promotion gate | Should the test enter the maintained suite? | Stability, ownership, maintenance cost, provenance |

The AI may explain, classify, and propose. A human owns ambiguous requirements, destructive operations, security exceptions, and promotion decisions.

## 14. Metrics

### Quality

- Compile or collection rate before repair.
- Accepted-test rate after review.
- Mutation score on targeted behavior where supported.
- Percentage of weak or tautological assertions rejected.
- Duplicate-test rejection rate.
- Flaky-test rate after repeated execution.
- Unique regressions caught before merge or release.

### Efficiency

- Median time from trigger to reviewable test PR.
- Human review minutes per accepted test.
- Model tokens and runner minutes per accepted test.
- Auto-fix attempts per accepted test.

### Safety

- Blocked secret or network access attempts.
- Prevented destructive operations.
- Runs requiring manual cleanup.
- Provenance verification success rate.

### Business-case targets

- Average model inference under 15 seconds per generated test suite.
- False-positive rate under 5% after automated local execution and triage, before anything reaches a human.
- Operational inference cost under $0.05 per evaluated pull request.
- Reviewer measures reported first: evidence arrival, review time, and unique catches.

Line coverage is reported as context, not as the primary success metric.

## 15. POC Definition of Done

The POC is complete when a pilot team can point the harness at a repository and PR or dependency change and receive, without harness-team intervention:

- A repository profile.
- A human-reviewable test plan.
- A native-framework test patch.
- Focused and related-suite results.
- Differential results for a selected dependency or bug-fix case.
- Assertion-quality and redundancy findings.
- A signed evidence bundle.
- A test-only pull request.

The POC must demonstrate at least one meaningful regression or vulnerability-targeted result that the existing suite did not provide, while maintaining the isolation and human-gate controls defined here.

## 16. Implementation Order

1. Repository discovery and framework adapters.
2. Test-plan schema and human gate.
3. Native test generation using existing repository examples.
4. Focused execution and result normalization.
5. Assertion-quality and redundancy evaluation.
6. Differential testing for bug fixes and dependency upgrades.
7. Stability and mutation checks for selected repositories.
8. Signed evidence and test-only PR publication.
9. Bounded repair routing and persistent orchestration.
10. Additional ecosystems only after pilot quality metrics justify expansion.
