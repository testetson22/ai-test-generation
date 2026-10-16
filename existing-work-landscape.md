# Existing Work Landscape: Test Plan Generation

Internal and open-source test-generation work to evaluate for reuse or learning, analyzed against the AI Test Generation architecture. Evidence levels are marked: **documented** (read from repository or README), **reported** (meeting notes, unverified), **inaccessible** (requires internal access).

Canonical project repository: <https://github.com/testetson22/ai-test-generation>

## Resources

| Resource | Access | Evidence level | Role |
|---|---|---|---|
| `ADLC Enablement Series: Test Plan Generator` notes (2026-10-14) | Local file | Reported (Gemini notes; presenter Federico Mosca) | Describes the internal GitLab CI pipeline around the test plan generator |
| `github.com/opendatahub-io/odh-test-gen` | Public; redirects to `fullsend-ai/test-creator` | Documented (README, repository tree) | The skill implementation: strategy-to-test-plan workflow |
| `gitlab.com/redhat/rhel-ai/agentic-ci/test-plan-generator` | 403 without auth | Inaccessible | CI orchestrator for the internal pipeline |
| `gitlab.com/redhat/rhel-ai/agentic-ci/test-plans-data` | 403 without auth | Inaccessible | Central test-plan data collection |

## odh-test-gen (now `fullsend-ai/test-creator`)

### What it does

End-to-end test planning for RHOAI: generate E2E/UI-focused test plans from Jira strategies (RHAISTRAT/RHOAIENG), create traceable test cases, implement executable pytest automation, verify UI tests against live clusters via Playwright, publish to GitHub through a PR, resolve review feedback, and score plans with rubrics backed by deterministic evidence gates.

### Architecture

- **8 user-invocable skills** (`/test-plan-create`, `-create-cases`, `-update`, `-case-implement`, `-ui-verify`, `-publish`, `-resolve-feedback`, `-score`) plus **8 internal sub-agents** (endpoint/risk/infra analyzers, merge, resolve-gaps, review, generate-test-file, score-test-function), seven of them forked for parallel execution.
- **Deterministic-first:** procedural logic lives in Python scripts with no LLM calls (validation, AST function extraction, score parsing, frontmatter, Jira operations, component mapping). LLMs are used only for semantic analysis, code generation, and quality scoring.
- **Quality evidence gates:** `validate_quality_evidence.py` produces scope-coverage and actionability payloads; `enforce_citation_gate.py` and `cap_scope_fidelity.py` deterministically cap rubric scores (Scope Fidelity, Specificity, Actionability) when evidence contradicts a scorer's rating. Scorer compliance is not trusted.
- **Citation gates:** acceptance-criterion citations, AC coverage, and interface coverage are checked before review or scoring; declared interfaces require `TC-E2E-*`/`TC-UI-*` references.
- **Gaps discipline:** in-scope items without a backing acceptance criterion are disclosed in `TestPlanGaps.md`, not converted into invented test objectives.
- **HITL gap resolution**, non-interactive CI mode, fork-based publish workflow, PR feedback resolution.

### Mapping to our architecture

| Our concept | odh-test-gen equivalent | Fit |
|---|---|---|
| Input driver | Jira STRAT strategy input (`StrategyDriver`) | Direct precedent for one driver |
| Analyze stage | Parallel endpoint/risk/infra analyzer sub-agents | Same decomposition we planned |
| Plan stage | TestPlan.md + TestPlanGaps.md with rubric and evidence gates | Working implementation, including our gaps-as-stated-output principle |
| Code stage | `test-plan-case-implement` (pytest) + `test-plan-generate-test-file` with AST checks and per-function scoring | Partial: black-box/UI target only |
| Execute stage | `test-plan-ui-verify` (Playwright against a live cluster) | Partial and not isolated: no sealed sandbox, no differential, no mutation |
| Review/publish | `/test-plan-publish` fork workflow, `/test-plan-resolve-feedback` | Matches our test-only PR boundary |
| Model-must-not-upgrade-evidence | Deterministic gates cap LLM rubric scores | Strongest single pattern to adopt |
| Checkpoint/feedback model | Session context, `preserve_review_state.py`, HITL gap resolution | Partial; not a durable cross-stage state machine |

### Gaps relative to our design

- No sealed execution: generated code runs where the developer runs; UI verification targets a live cluster.
- No differential testing, mutation scoring, or stability checks.
- No provenance or signed records; no immutable analysis context.
- No failure register; no fail-closed self-verification.
- Black-box scope only (deliberately excludes unit/integration tests).
- Adoption problems are reported internally (see below), and the repository has moved to the Fullsend organization — ownership and maintenance path need confirmation.

### Third-party and open-source components used

Claude Code skills/plugin format; `opendatahub-io/skills-registry` marketplace; Python with `uv`; `pytest` + `pytest-cov`; `PyYAML`; `ruff`; `flake8` with `RedHatQE/flake8-plugins`; `pre-commit`; `markdownlint`; CodeRabbit; GitHub CLI (`gh`); Jira REST API; Playwright; `opendatahub-tests` as the automation target; `opendatahub-io/opendatahub-test-plans` as the artifact repository.

## Internal pipeline (from the ADLC session notes; reported, unverified)

- Trigger: a human sign-off label on a Jira STRAT. Two parallel tracks start: the test plan generator and the feature composer/API implementation. Parallel execution is deliberate: test plans verify high-level acceptance criteria without overfitting to implementation detail. A raised caveat (Bill Murdock): strats must state all impacted teams and repositories, or test-case generation stays ambiguous.
- A GitLab CI scheduled job (every three hours) checks for the sign-off label and runs plan creation in batches of two. A secondary gate script requires rubric score at least 8 (of 10, five criteria scored 0-2) with actionability 2 before generating test cases; 7 or low actionability triggers revise; below 7 triggers two rework rounds.
- Outputs are published to GitLab for review with rubric tables in Jira comments; gaps and review history are tracked; publishing to data repositories does not fail the pipeline on connectivity issues.
- Known limitations: no automatic deletion of obsolete test cases (coverage-duplication checks and updates exist); low user adoption; test plans contain too much text — reviewers reportedly paste them into an LLM rather than read them.
- Open migration: the CI orchestrator ("agentic CI") may move to Fullsendrunner; decision pending with Heather and stakeholders.

## Lessons for our design

1. **Deterministic gates over LLM self-assessment.** The cap-the-scorer pattern (evidence payloads can only lower a rubric score, never raise it) is the same rule as our "model must not upgrade evidence" principle and is demonstrated in this workflow. Options for our quality-evaluation gate are to reuse the pattern, implement an equivalent gate, or retain it as a comparison baseline.
2. **Concise artifacts are an adoption requirement, not a nicety.** Reported reviewer behavior (paste into an LLM instead of reading) validates the business case's reviewer-first measures and our one-page review-packet rule. Plans that are too long defeat their own gate.
3. **Gaps as first-class output.** In-scope items without acceptance-criterion backing become disclosed gaps, not invented objectives — the same three-states principle ("stated gap with its reason; nothing is silent").
4. **Retirement is missing and matters.** Their inability to delete or retire obsolete cases is a reported pain point; our relevance/retirement proposals (from the harness) address it and should stay in scope.
5. **Parallel-to-implementation preserves independence** but requires the input artifact to declare impacted repositories — an input-contract requirement for our StrategyDriver equivalent, not something the generator should infer.
6. **Deterministic/LLM split is viable at this scale** and matches our facts-from-tools principle: AST extraction, validation, mapping, and Jira operations in scripts; semantics and code in the model.

## Reuse or learn from: options

| Component | Options under consideration | Reason |
|---|---|---|
| Skill/sub-agent workflow structure | Reference; consume by contract; fork; or implement independently | Directly maps to our Analyze/Plan stages for a strategy driver |
| Deterministic gate scripts and rubric-cap pattern | Reuse; implement an equivalent; or benchmark against it | Aligns with our evidence rules; component license and ownership require verification |
| pytest/Playwright implementation skills | Reuse selectively; reference; or replace with native ecosystem adapters | Our Code stage is broader (native frameworks, sealed execution) |
| GitLab CI orchestrator | Reference; integrate; or replace with Lightwell invocation | Orchestration ownership and internal access remain open |
| test-plans-data collection | Reference; integrate through Lightwell; or keep separate | Evidence storage ownership remains a design question |

## Verification required before consumption

- License and contribution terms of `fullsend-ai/test-creator` (not stated in the fetched README).
- Ownership and maintenance path after the move to the Fullsend organization, and the Fullsendrunner migration decision.
- Internal access to the two GitLab repositories for the orchestrator and data contracts.
- Accuracy of the meeting notes against the recording (notes are Gemini-generated and flagged for verification).
