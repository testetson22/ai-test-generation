# AI Test Generation Factory: Technical Specification

## 1. Purpose

The factory generates, executes, evaluates, and proposes tests for an existing software repository. Its primary product is not generated application code. Its product is reviewable test evidence:

- A requirement-to-test coverage map.
- Framework-native tests that follow repository conventions.
- Reproducible execution results from an isolated environment.
- Evidence that the tests can detect relevant failures, not merely execute lines.
- A pull request containing only tests and supporting fixtures that a human may accept or reject.

The factory does not merge changes, publish product or release artifacts, modify production infrastructure, or read production secrets. It may publish test reports and signed evidence to an approved CI artifact store.

## 2. Initial Assessment and Corrections

The original proposal establishes useful high-level boundaries: stateful orchestration in LangGraph, isolated execution in CI, Git as the source of truth, bounded repair loops, and human approval before promotion. The supporting software-factory notes also correctly emphasize spec-derived tests, containerized interfaces, and a layered validation strategy.

Several claims require refinement before implementation:

- **Pytest is not a universal test framework.** It can orchestrate external commands, but generated tests should use the repository's native framework: pytest, Go testing/Ginkgo, Jest/Vitest, Playwright, or another established runner. A thin result-normalization layer can aggregate JUnit and coverage artifacts.
- **Coverage is not proof of test quality.** Line coverage is useful as a gap signal, but assertion strength, mutation score, failure sensitivity, and behavioral coverage determine whether a generated test is valuable.
- **Tests generated only from implementation can reproduce the implementation's bugs.** Test intent should originate from requirements, API schemas, bug reports, and behavioral contracts, then be grounded in existing repository patterns.
- **An LLM should not choose a language or architecture from generic heuristics.** Existing repository conventions and supported platforms take precedence. The factory should generate tests for the system it finds rather than redesigning the system.
- **An “uncertainty coefficient” cannot be a free-form model opinion.** Risk routing must be based on observable inputs such as destructive operations, missing requirements, unfamiliar frameworks, external services, authentication changes, and low confidence in repository discovery.
- **CI isolation is not automatically air-gapped.** Runner identity, egress policy, secret scope, filesystem lifetime, and artifact retention must be explicitly configured.
- **Automatic retries can conceal flaky tests.** Retries may be diagnostic, but a test that passes only after retry must not be reported as reliably passing.
- **Mocks are not a default substitute for integration.** Mocks should isolate destructive or unavailable boundaries; contract and integration tests must still prove that boundary assumptions are correct.

## 3. Architecture

```text
Issue / PR / API schema / bug report
                 |
                 v
        Repository Discovery
  instructions, tests, fixtures, CI, APIs
                 |
                 v
         Test Planning Agent
 requirements -> scenarios -> test levels
                 |
          Human plan gate
                 |
                 v
      Framework Adapter + Generator
 pytest | Go/Ginkgo | Jest | Playwright
                 |
                 v
       Ephemeral Validation Runner
 lint -> focused test -> suite -> coverage
       -> mutation/differential checks
                 |
                 v
         Quality Evaluation Gate
 assertion strength, cleanup, determinism,
 behavioral fidelity, changed-line impact
                 |
                 v
         Test PR + Evidence Bundle
```

### Components

| Component | Responsibility | Trust boundary |
|---|---|---|
| LangGraph controller | Persist workflow state, route failures, enforce budgets and human gates | Cannot execute repository code or access deployment secrets |
| Repository discovery worker | Read repository instructions, representative tests, build files, CI, schemas, and changed code | Read-only checkout |
| Test planner | Produce structured scenarios from requirements and discovered behavior | Cannot edit code |
| Test generator | Generate tests using a selected framework adapter | Writes only to an isolated branch/worktree |
| CI execution worker | Run untrusted generated code in an ephemeral environment | No production credentials; restricted network and resource limits |
| Quality evaluator | Evaluate test results, assertions, mutation score, coverage delta, and flakiness | Consumes artifacts; does not modify tests |
| PR publisher | Open or update a test-only PR after gates pass | Cannot approve or merge |

## 4. Repository Discovery Contract

Test generation begins by learning the repository rather than selecting tools from a global default.

The discovery worker must collect:

1. Repository instructions such as `AGENTS.md`, `CLAUDE.md`, `.cursor/rules/`, and test READMEs.
2. Language and build manifests such as `pyproject.toml`, `go.mod`, `package.json`, and Makefiles.
3. Two or three representative tests near the changed code.
4. Shared fixtures, page objects, builders, mock servers, cleanup helpers, and test data.
5. CI commands, required services, labels/markers, timeout conventions, and artifact formats.
6. Behavioral sources: acceptance criteria, OpenAPI/AsyncAPI schemas, protobuf definitions, bug reports, and existing tests.
7. Safety boundaries: operations that create clusters, delete resources, rotate keys, restart services, or call external systems.

Discovery produces a versioned `repository-profile.json`:

```json
{
  "schema_version": "1.0",
  "repository": "dcm-project/utilities",
  "revision": "<git-sha>",
  "languages": ["go", "bash"],
  "test_adapters": [
    {
      "framework": "ginkgo",
      "path": "tests/e2e",
      "command": "go run github.com/onsi/ginkgo/v2/ginkgo -r -v --tags=e2e .",
      "representative_tests": [
        "tests/e2e/api_auth_test.go",
        "tests/e2e/api_catalog_test.go"
      ],
      "cleanup_pattern": "DeferCleanup",
      "result_format": "junit"
    }
  ],
  "constraints": {
    "network": "restricted",
    "production_secrets": false,
    "destructive_tests_require_approval": true
  }
}
```

The profile is invalidated when relevant manifests, test configuration, or repository instructions change.

## 5. Test Plan Contract

The planner converts requirements into test intent before code generation. Each planned scenario records what it proves and what it intentionally excludes.

```json
{
  "schema_version": "1.0",
  "source_revision": "<git-sha>",
  "requirements": [
    {
      "id": "AUTH-04",
      "source": "issue:FLPATH-0000",
      "statement": "Requests with an expired token are rejected with HTTP 401"
    }
  ],
  "scenarios": [
    {
      "id": "AUTH-04-expired",
      "requirement_ids": ["AUTH-04"],
      "level": "integration",
      "framework": "ginkgo",
      "setup": "Issue a one-second token from the test identity provider",
      "action": "GET /catalog-items after expiry",
      "oracle": {
        "status": 401
      },
      "boundaries": ["one second before expiry", "at expiry", "one second after expiry"],
      "mock_policy": "Use the test identity provider; do not mock gateway JWT verification",
      "cleanup": "Delete the temporary client with DeferCleanup",
      "destructive": false
    }
  ],
  "out_of_scope": [
    "Identity-provider availability testing is covered by AUTH-RESTART"
  ]
}
```

### Required scenario categories

The planner considers, but does not blindly generate, the following categories:

- Happy path.
- Authentication and authorization (`401` versus `403`).
- Input validation and malformed payloads.
- Not-found and conflict behavior.
- Empty, minimum, maximum, and just-outside-boundary values.
- Failure and timeout behavior at external boundaries.
- State transitions and idempotency.
- Concurrency when the requirement identifies shared mutable state.
- Regression reproduction for a bug report.

The plan must identify cases already covered elsewhere to prevent redundant tests.

### Test oracle precedence

Expected behavior must have an attributable source. When sources disagree, the factory does not silently choose the implementation's current behavior.

1. Approved acceptance criteria or normative product specification.
2. Published API or protocol contract.
3. Approved test plan or linked bug report.
4. Existing tests and documented repository conventions.
5. Current implementation behavior, used only for characterization when no stronger contract exists.

A conflict between higher-precedence sources triggers a human gate. The evidence bundle records the source used for every expected result.

### Redundancy detection

Before generation, the planner inventories existing tests by requirement identifier, endpoint or public symbol, scenario category, input boundary, and expected result. Semantic similarity may nominate possible duplicates, but the final decision compares setup, action, oracle, and test level. A candidate is rejected as redundant only when an existing test proves the same behavior at an equal or stronger level.

## 6. Generation Workflow

### Step 1: Select the correct test level

| Behavior | Preferred level |
|---|---|
| Pure computation or validation | Unit |
| Database, queue, identity provider, or API boundary | Integration/contract |
| Critical user or service workflow | E2E |
| Deployment manifest or container contract | Component/infrastructure validation |

Use the lowest level that can prove the behavior. Do not generate an E2E test for logic that a deterministic unit test can prove.

### Step 2: Extract repository patterns

The generator reads two or three representative tests and records:

- Naming and directory conventions.
- Fixture and setup style.
- Assertion libraries and response helpers.
- Cleanup patterns.
- Markers, labels, and test selection rules.
- Existing mock and fake implementations.
- Expected error and status contracts.

### Step 3: Generate framework-native tests

The generator uses a framework adapter rather than routing every language through pytest.

| Repository | Native output | Factory execution |
|---|---|---|
| Python | pytest tests and fixtures | `pytest` |
| Go | `testing` or Ginkgo/Gomega | `go test` or `ginkgo` |
| TypeScript unit tests | Jest/Vitest | `npm test` or native script |
| UI flows | Playwright | `playwright test` |
| Ansible | Molecule plus verifier | `molecule test` |
| Terraform | `terraform validate`, plan assertions, policy checks | native CLI plus policy runner |

Pytest may orchestrate black-box container tests when the repository already uses Python for system validation. It is not required merely to normalize reports.

Each framework adapter implements the same operations:

```text
discover()  -> commands, fixtures, conventions, representative tests
plan()      -> supported scenario levels and required environment
generate()  -> test files and fixture changes
select()    -> focused command for generated tests
execute()   -> exit status and raw artifacts
normalize() -> JUnit, coverage, skips, and framework metadata
```

The controller does not construct framework commands itself; commands come from the repository profile and selected adapter.

### Step 4: Review generated assertions

Every assertion must be able to fail and must prove the stated behavior. The evaluator rejects:

- Tautologies such as `len(items) >= 0`.
- Broad status classes when a specific status is part of the contract.
- Existence-only assertions when the expected value is known.
- Conditional assertions with a silent success path.
- Create-without-readback tests when persistence is the behavior.
- Inexact cardinality where the expected count is known.
- Tests that permanently accept a known bug as an alternative result.

### Step 5: Validate incrementally

1. Format, parse, lint, and compile the new test.
2. Run the single generated test.
3. Run the related test package or directory.
4. Run the repository's required suite.
5. Compare coverage on changed or targeted code.
6. Run mutation or differential validation where supported.
7. Repeat a stable subset to detect order dependence and flakiness.

## 7. Mocking and Side-Effect Policy

Mocks are chosen by boundary and risk, not by convenience.

| Boundary | Default strategy | Reason |
|---|---|---|
| Pure internal collaborator | Mock or fake at the module interface | Keeps unit tests deterministic |
| HTTP client | Local mock server or recorded contract | Exercises request serialization and response handling |
| Database | Disposable database for integration; repository mock for unit tests | Separates query contract from business logic |
| Queue/event bus | In-memory fake for unit; disposable broker for integration | Verifies both logic and wire behavior |
| Cloud API | LocalStack/fake only for supported semantics; contract tests for provider-specific behavior | Cloud emulators are incomplete |
| Identity provider | Disposable Keycloak/RHBK for JWT and rotation behavior | Mocked tokens cannot prove signature integration |
| Kubernetes/OpenShift | Fake client for pure reconciliation; ephemeral namespace or cluster for API behavior | Avoids destructive operations on shared clusters |
| Destructive lifecycle | Mock/fake unless explicitly approved in an isolated environment | Prevents deletion or mutation of shared resources |

Rules:

- Patch at the boundary where the dependency is consumed, not an internal helper.
- Use interface/spec-constrained mocks so invalid calls fail.
- Assert important interactions, including that destructive methods were not called on rejection paths.
- Prefer unique namespaces and cleanup fixtures for real integration resources.
- A mock-based test cannot claim to validate the real provider contract without a corresponding contract or integration test.
- Generated tests must not target production accounts, clusters, Vault paths, or credentials.

## 8. Quality Gates

### Mandatory gates

1. **Repository conformance:** Generated code follows discovered test structure and uses existing fixtures where appropriate.
2. **Buildability:** The test compiles or is collected successfully.
3. **Behavioral result:** The test passes against the intended implementation.
4. **Regression sensitivity:** For a known bug, the test fails on the known-bad revision and passes on the fixed revision.
5. **Assertion quality:** Static and model-assisted review finds no tautological or non-discriminating assertions.
6. **Isolation:** No production secrets, unrestricted egress, or unapproved destructive targets are available.
7. **Suite compatibility:** Related existing tests still pass.
8. **Stability:** No order-dependent or retry-only success in the repeated validation subset.

### Context-dependent gates

- Changed-line coverage delta.
- Mutation score for targeted code.
- API schema conformance.
- Performance budget.
- Accessibility checks for UI tests.
- Infrastructure policy checks.

Coverage alone never promotes a generated test.

## 9. Auto-Fix Policy

The repair loop may fix:

- Syntax, imports, formatting, and compilation errors.
- Incorrect fixture names or framework usage when repository evidence identifies the correct pattern.
- Test setup failures caused by the generated test itself.

The repair loop must stop for human review when a proposed fix would:

- Weaken or delete an assertion.
- Change the expected behavior to match the implementation.
- Add a skip, xfail, retry, or broad exception handler.
- Modify production code unless the task explicitly includes implementation.
- Change infrastructure, permissions, authentication, or secret handling.
- Exceed the configured attempt or token budget.

Each attempt records the failure category, files changed, test result, and reason for the next action.

## 10. Evidence Bundle

Every run emits machine-readable and human-readable evidence:

```text
artifacts/
  repository-profile.json
  test-plan.json
  generated-diff.patch
  commands.json
  junit.xml
  coverage.xml
  mutation.json
  stability.json
  review-summary.md
  provenance.json
```

The review summary includes:

- Source requirements and their scenario mapping.
- Tests generated, modified, rejected, or found redundant.
- Exact commands and revisions used.
- Pass, fail, skip, and flaky counts.
- Coverage and mutation deltas with limitations.
- Mocks/fakes used and which real contracts remain unverified.
- Destructive operations requested and whether they were blocked or approved.
- Auto-fix attempts and whether any assertion changed.

## 11. Concrete Examples

### Example A: Python API validation

Requirement: `POST /providers` rejects an empty name with HTTP 400 and does not write to the repository.

Generated unit test:

```python
def test_rejects_empty_provider_name_without_writing(provider_service, provider_repo):
    with pytest.raises(ValidationError, match="name is required"):
        provider_service.create(name="", region="us-east-1")

    provider_repo.create.assert_not_called()
```

Generated integration test:

```python
def test_post_provider_rejects_empty_name(api_client):
    response = api_client.post(
        "/api/v1/providers",
        json={"name": "", "region": "us-east-1"},
    )

    assert response.status_code == 400
    assert response.json()["detail"] == "name is required"
```

Why both may be justified:

- The unit test proves rejection occurs before persistence.
- The integration test proves HTTP serialization and the public error contract.
- The factory checks existing tests before proposing either to avoid duplication.

### Example B: Go/Ginkgo authentication regression

Requirement: expired, malformed, wrong-issuer, wrong-audience, and invalid-signature JWTs return HTTP 401.

Generated structure:

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

Factory decisions:

- Use a disposable identity provider because mocking JWT verification cannot prove key discovery or signature validation.
- Create temporary clients with unique names and register `DeferCleanup` immediately.
- Keep restart and signing-key-rotation cases labeled `disruptive` and run them after the regular suite.
- Do not generate separate cases if the table already covers the requirements.

### Example C: Playwright workflow

Requirement: an unauthorized user must not see or invoke the administrative action.

```typescript
test("hides provider deletion from unauthorized users", async ({ unauthorizedPage }) => {
  await unauthorizedPage.goto("/providers/test-provider");

  await expect(unauthorizedPage.getByRole("button", { name: "Delete provider" }))
    .not.toBeVisible();

  const response = await unauthorizedPage.request.delete("/api/providers/test-provider");
  expect(response.status()).toBe(403);
});
```

This validates both UI suppression and server-side authorization. UI-only coverage would be insufficient.

### Example D: Dependency upgrade differential test

Input:

- Base revision uses dependency version A.
- Candidate revision uses dependency version B.
- A bug report or advisory describes the affected behavior.

Execution:

1. Generate a test from the behavioral claim.
2. Run it against A and B in otherwise identical containers.
3. Classify the result:
   - Fails on A, passes on B: candidate regression test for a fix.
   - Passes on both: characterization test, not proof of a fix.
   - Fails on both: implementation or test-plan conflict.
   - Passes on A, fails on B: candidate upgrade regression.
4. Run mutation testing on the exercised application boundary when feasible.

The report must not claim a vulnerability is fixed solely because an advisory no longer applies; it records exactly what behavior the test proved.

## 12. Orchestration State

LangGraph stores references to artifacts, not raw secrets or full unbounded logs.

```json
{
  "run_id": "atg-2026-00123",
  "repository": "owner/repo",
  "base_revision": "<sha>",
  "candidate_revision": "<sha>",
  "phase": "quality_evaluation",
  "risk_flags": ["authentication", "external-service"],
  "attempt": 1,
  "attempt_limit": 3,
  "artifact_uris": {
    "profile": "artifact://repository-profile.json",
    "plan": "artifact://test-plan.json",
    "results": "artifact://junit.xml"
  },
  "human_gate": {
    "required": true,
    "reason": "disruptive authentication test requested"
  }
}
```

Human gates are triggered by explicit policy flags, not a model-selected floating-point score alone.

## 13. Execution Security

- Use ephemeral runners or short-lived Kubernetes jobs with immutable base images.
- Run as non-root with a read-only base filesystem where possible.
- Disable privileged containers and host socket mounts by default.
- Deny network egress unless the test plan names an allowed destination.
- Issue short-lived, least-privilege test credentials at execution time.
- Never expose production Vault tokens to generation or validation workers.
- Apply CPU, memory, process, and wall-clock limits.
- Sign test evidence and bind it to repository and container-image digests.
- Treat repository content, generated scripts, and model output as untrusted input.

## 14. Success Metrics

### Quality

- Generated-test compile/collection rate.
- Accepted-test rate after human review.
- Mutation score on targeted code.
- Percentage of generated assertions rejected as weak or tautological.
- Duplicate-test rejection rate.
- Flaky-test rate after repeated execution.
- Bugs or regressions uniquely caught by generated tests.

### Efficiency

- Median time from requirement to reviewable test PR.
- Human review minutes per accepted test.
- Model tokens and runner minutes per accepted test.
- Auto-fix attempts per accepted test.

### Safety

- Attempts to access blocked secrets or networks.
- Destructive operations prevented by policy.
- Generated tests requiring manual cleanup.
- Provenance verification success rate.

Coverage is reported, but it is not a primary success metric.

## 15. Proof-of-Concept Scope

The first implementation should be smaller than the full software-factory vision.

### Supported inputs

- One existing Python repository using pytest.
- One existing Go repository using Go testing or Ginkgo.
- A Git pull request, issue/acceptance criteria, or bug report.

### Supported outputs

- Repository profile.
- Test plan requiring human approval.
- Unit and integration tests in existing frameworks.
- JUnit, coverage, assertion-quality, and optional mutation results.
- A test-only pull request and evidence bundle.

### Explicitly out of scope

- Generating application architecture or choosing a new implementation language.
- Production deployment.
- Production secrets or infrastructure mutation.
- Autonomous merge or release.
- Broad transitive-dependency test generation without reachability evidence.

### Pilot exit criteria

- At least 80% of proposed tests compile or collect before human repair.
- At least 60% of proposed tests are accepted or accepted with minor edits.
- Fewer than 5% of accepted tests are flaky across repeated runs.
- Every accepted test maps to a documented requirement or regression.
- At least one accepted test fails on a known-bad revision and passes on the fixed revision.
- No generated test reaches a production service or secret.

## 16. Recommended Implementation Order

1. Build repository discovery and framework adapters.
2. Define and validate the test-plan schema.
3. Implement generation using existing repository examples.
4. Add focused execution and JUnit normalization.
5. Add assertion-quality and duplicate-test evaluation.
6. Add differential execution for bug fixes and dependency upgrades.
7. Add mutation testing for selected repositories.
8. Add LangGraph persistence and bounded repair routing.
9. Add signed evidence and test-only PR publication.
10. Expand frameworks only after quality metrics support the next adapter.

This sequence proves test-generation value before investing in a general-purpose AI software factory.
