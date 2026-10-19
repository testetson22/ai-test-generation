# AI Test Generation Decision Record

## Status

Proposed. This record consolidates current decisions and unresolved questions. It is not approved until the required owners confirm the consensus items.

## Context

AI Test Generation is intended to integrate with Red Hat Lightwell and generate high-fidelity, repository-native tests for vulnerability findings, pull requests, dependency changes, bugs, contracts, test gaps, and user objectives.

Vulnerability Prioritization supplies applicability, SBOM, build-context, symbol, reachability, path-witness, and limitation evidence. AI Test Generation consumes that evidence and produces scoped test evidence without rewriting upstream assessments.

The service must distinguish what was proven from what could not be evaluated. A prioritized vulnerability is not, by itself, proof of runtime activation or exploitability.

## Decision Summary

The current architectural direction is a driver-agnostic, evidence-driven AI Test Generation service with the following user-facing lifecycle:

```text
Analyze -> Plan -> Code -> Execute
```

The service should support durable checkpoints and user feedback at each stage. It should generate native-framework tests, execute only within approved environments, preserve provenance, and deliver proposed tests through human review.

The current implementation direction is to build the AI Test Generation service while selectively consuming existing tools where they provide sufficient fidelity. This remains open to alternatives supported by stronger evidence.

## Confirmed Decisions

These decisions are established by the current discussions and should be treated as architectural constraints unless explicitly superseded.

### 1. Driver-agnostic input model

Vulnerability prioritization is one input driver, not the sole organizing assumption. Other supported or potential drivers include:

- Pull requests and first-party changes.
- Dependency upgrades and downgrades.
- Bug reports and regression requests.
- API, schema, and compatibility changes.
- Test-gap requests.
- Scheduled characterization or rescan jobs.
- Explicit user objectives.

### 2. Evidence boundary

Vulnerability Prioritization remains authoritative for its own applicability, SBOM, build-context, symbol, reachability, path-witness, and limitation evidence. AI Test Generation must consume references to that evidence rather than silently duplicate or reinterpret it.

Evidence, assessment, and recommendation remain separate concepts:

- **Evidence:** observed or tool-derived facts.
- **Assessment:** interpretation, priority, confidence, or classification.
- **Recommendation:** suggested next action.

The model must not create graph facts, invent reachability, or upgrade non-reproduction into safety.

The system that produces a downstream evidence-based assessment owns that generated evidence and assessment. Vulnerability Prioritization remains responsible for its upstream evidence; AI Test Generation owns evidence produced by its planning, generation, execution, and evaluation stages. Storage, publication, and platform record ownership are separate integration questions.

### 3. Four-stage user-facing lifecycle

The user-facing workflow is:

```text
Analyze -> Plan -> Code -> Execute
```

The internal implementation may contain additional stages such as intake, generation repair, triage, review, provenance, and feedback.

Each stage may produce a durable checkpoint and accept user feedback. Feedback should be scoped to the stage and decision it changes.

### 4. Flexible contextual inputs

A request may include or reference more than the prioritized vulnerability. Useful context includes:

- Repository test conventions and existing coverage.
- Build/test commands, fixtures, markers, and reports.
- Build manifests, lockfiles, generated sources, toolchains, and artifact identity.
- Service boundaries and documented application call sites.
- Required databases, queues, identity providers, and external services.
- Available sandbox, cluster, or dynamic-validation environments.
- Authorization, safety, network, credential, and resource policies.

Missing, stale, ambiguous, or unauthorized context must produce an explicit gap or `not_evaluable` result rather than model improvisation.

### 5. Analyze-stage feasibility assessment

Before generation, Analyze evaluates which validation layers are safe and technically possible for the run. Potential outcomes include:

- `static_only`
- `library_testable`
- `call_site_testable`
- `component_testable`
- `deployment_simulatable`
- `active_validation_authorized`
- `not_evaluable`

The feasibility result constrains Plan. An unavailable validation layer cannot be represented as a passing result.

### 6. Three-outcome behavior rule

Every reached behavior must end in one of these outcomes:

1. Validated by an executed test.
2. Handed to the team for execution in its environment.
3. Recorded as a stated gap with its reason.

Nothing is silently omitted and nothing is claimed without evidence.

### 7. Testing-only boundary

The service tests and proposes. It does not, by default:

- Modify production code.
- Modify dependency manifests.
- Merge changes.
- Publish releases.
- Delete tests or authoritative evidence.
- Rewrite Lightwell vulnerability records.

Remediation remains a separate workflow whose output returns to test validation as a new candidate revision.

### 8. Native-framework generation

Generated tests should follow the target repository's framework, conventions, fixtures, naming, cleanup, and execution commands. There is no requirement for a universal test runner.

### 9. Deterministic quality controls

Deterministic tooling should handle discovery, parsing, validation, citation, mapping, collection, assertion checks, and other enforceable rules wherever practical. Model output must not be accepted solely because it compiles or exits successfully.

### 10. Explicit uncertainty

The service must distinguish at least:

- `validated`
- `reproduced`
- `fixed_behavior`
- `not_reached`
- `reached_not_reproduced`
- `blocked`
- `inconclusive`
- `needs_human_review`
- `stated_gap`

### 11. Cross-cutting golden-reference validation

Every implementation option must be evaluated against a maintained golden-reference set covering known behaviors, failure modes, and representative generation cases.

Validation should be layered rather than exact-text-only:

- Exact comparison for stable machine-readable fields.
- Normalized structural or semantic comparison for plans and generated code.
- Behavioral comparison through execution.
- Evidence and provenance comparison.
- Human-review outcomes where applicable.

Passing golden references does not establish generalization. Differential execution, mutation sensitivity, stability, and held-out cases remain relevant.

Golden references or an equivalent expected-test-output set are required quality inputs, not optional benchmarking. Quality gates must also establish whether generated tests exercise the prioritized vulnerability, code change, or other stated objective rather than merely compile or pass.

### 12. Existing work is evidence, not commitment

The following are candidate sources for reuse or learning, not selected dependencies:

- `ai-test-harness`.
- `fullsend-ai/test-creator`, formerly `odh-test-gen`.
- Internal test-plan-generator and test-plans-data repositories.
- Native test runners, Playwright, static analysis, mutation, fuzzing, and provenance tools.

Each candidate requires license, ownership, maintenance, technical-fit, security, and integration review.

The working implementation direction is to build the service around the required Lightwell and evidence contracts while consuming existing components where they meet the required fidelity. A stronger reuse or integration proposal remains admissible.

### 13. Scope

Prioritized vulnerability assessment is the most important initial input. The service must remain straightforward to invoke for code changes, well-structured prompts, dependency changes, bugs, contracts, and other clearly defined objectives. Supporting additional inputs must not require redesigning the vulnerability-driven path.

## Locally Confirmed Working Decisions

These choices are confirmed for the current design and documentation work. They are working conventions, not group-approved architecture commitments, and remain subject to Lightwell or repository-owner constraints.

| Topic | Confirmed working choice | Revisit when |
|---|---|---|
| Documentation organization | Keep the business case, research, landscape, reliability findings, ADR plan, ledger, decision record, and ADR as separate Markdown artifacts | Architecture review requests a different repository structure |
| ADR presentation | Use the architecture-team template with concise tables and plain language modeled after the dependency-remediation document | Architecture review identifies a required format |
| Reference linking | Use repository-relative links locally and canonical GitHub links for published findings | Repository layout or publishing model changes |
| Input terminology | Use “service” for the product being designed and “driver” for an initiating input type | Lightwell adopts different domain terminology |
| Stage terminology | Use Analyze, Plan, Code, Execute for user-facing stages; permit finer internal stages | Usability research or platform integration requires different stage names |
| Evidence wording | Use explicit result states and limitations instead of unexplained safe/unsafe or pass/fail summaries | A governing Lightwell contract defines different states |
| Initial documentation examples | Use one Python and one Go example project in the business-case pilot framing | Sponsor or pilot owners define another scope |

These are working conventions, not architectural commitments.

## Decisions Requiring Documented Justification or Group Consensus

### 1. Implementation basis

Options:

- Assemble a Lightwell service from selected open-source and internal components.
- Directly extend an existing implementation.
- Build a new service with existing tools used only as references.
- Use a hybrid approach by capability or software stack.

Required evidence:

- License and contribution terms.
- Maintenance and ownership status.
- Capability and reliability comparison.
- Integration cost and lifecycle fit.
- Security and support requirements.

Owners: AI Test Generation owners, Lightwell engineering, legal/security stakeholders as applicable.

Current working position: build the service around the required evidence and Lightwell contracts while consuming existing tools selectively. This is not exclusive; a stronger reuse or integration proposal may replace it.

### 2. Platform orchestration

Options:

- Lightwell-native orchestration.
- Independent AI Test Generation service invoked by Lightwell.
- Federated or hybrid model.

Required evidence:

- Tenancy and authorization model.
- Checkpoint and feedback lifecycle.
- Failure and retry ownership.
- Evidence identity and retention.
- Operational ownership and service-level expectations.

Owners: Lightwell platform stakeholders and AI Test Generation owners.

### 3. Evidence storage and platform record ownership

Options:

- Lightwell-owned records.
- AI Test Generation-owned records linked to Lightwell.
- Federated records with reconciliation.

Required evidence:

- Existing Lightwell evidence APIs and identity model.
- Provenance and attestation requirements.
- Retention, audit, and tenant isolation policy.

Owners: Lightwell evidence/platform owners and Product Security.

Confirmed boundary: the producer of a downstream evidence-based assessment owns the evidence and assessment it generates. Vulnerability Prioritization owns its upstream evidence; AI Test Generation owns its generated test and runtime evidence. Storage, publication, retention, and platform record ownership remain open.

### 4. Runtime validation

Options:

- Static/contextual validation only for the first release.
- Library and application call-site validation.
- Component or deployment-shaped simulation.
- Separately approved active exploitation validation.

Required evidence:

- Artifact and production-profile fidelity.
- Sandbox and service availability.
- Authorization and safety policy.
- Cost, reproducibility, and observability.

Owners: Product Security, platform operators, QE, and Lightwell stakeholders.

### 5. Operating mode

Options:

- Human gates after Analyze and Plan.
- Policy-selected unattended runs for trusted repositories.
- Fully unattended operation.

Required evidence:

- Repository trust model.
- Reviewer acceptance and false-positive rates.
- Auditability and rollback.
- Cost and latency measurements.

Owners: Pilot teams, QE, Product Security, and platform owners.

### 6. Existing-tool reuse

Options:

- Reuse deterministic quality gates from existing work.
- Integrate existing stage or driver implementations by contract.
- Fork and extend selected projects.
- Implement equivalent behavior independently.

Required evidence:

- Tool reliability findings.
- License and ownership verification.
- Compatibility with Lightwell contracts.
- Long-term maintenance burden.

Owners: AI Test Generation owners and maintainers of candidate projects.

### 7. Golden-reference governance

Options:

- Golden references owned by the service team.
- Golden references reviewed by QE and product teams.
- Golden references stored and governed through Lightwell evidence services.
- A federated ownership model by driver or software stack.

Required evidence:

- Sensitivity and access classification.
- Drift and update process.
- Review and approval workflow.
- Evaluation cost and representative coverage.

Owners: AI Test Generation owners, QE, Lightwell stakeholders, and Product Security where sensitive cases are included.

Confirmed requirement: every implementation option must use golden references or an equivalent expected-test-output set, and quality gates must verify that tests exercise the stated vulnerability, change, or objective. The governance mechanism remains open.

### 8. Pilot quality and cost gates

The business case proposes targets for inference latency, false-positive rate, inference cost, evidence arrival, review time, and unique findings. Quality gates must also include golden-reference results and objective-exercise evidence: whether the generated tests exercise the prioritized vulnerability, code change, or other stated objective. The group must decide whether quantitative targets are hard exit gates, report-only measures, or initial targets to be re-baselined after measurement.

Owners: Sponsor, pilot teams, and AI Test Generation owners.

### 9. Deferred security and operational ownership

Detailed ownership for production security operations, sandbox operations, signing, retention, and long-term service operations is deferred beyond the POC. The POC still requires explicit authorization and safe execution constraints, but it does not need to settle the final organizational ownership model.

## Consequences

### Pros

- Established decisions are separated from unresolved architecture choices.
- Local editorial and documentation choices can proceed without blocking stakeholder decisions.
- Each consensus item identifies the evidence and owners required to resolve it.
- The record can support a focused ADR while linking detailed designs and research.
- Existing tools can be evaluated without prematurely committing to reuse.

### Cons

- The design remains intentionally provisional until Lightwell and other owners confirm the consensus items.
- Multiple ownership boundaries create coordination work.
- Golden-reference governance adds curation and maintenance cost.
- Runtime validation and tool reuse cannot be finalized from repository research alone.

## References

- [AI Test Generation ADR draft](https://github.com/testetson22/ai-test-generation/blob/main/adr-ai-test-generation.md)
- [AI Test Generation research](https://github.com/testetson22/ai-test-generation/blob/main/ai-test-generation-research.md)
- [Vulnerability-driven structure](https://github.com/testetson22/ai-test-generation/blob/main/vulnerability-driven-structure.md)
- [Existing work landscape](https://github.com/testetson22/ai-test-generation/blob/main/existing-work-landscape.md)
- [Tool reliability findings](https://github.com/testetson22/ai-test-generation/blob/main/tool-reliability-findings.md)
- [Decision ledger](https://github.com/testetson22/ai-test-generation/blob/main/decision-ledger.md)
- [AI Test Harness](https://github.com/croadfeldt/ai-test-harness)
- [Fullsend test creator](https://github.com/fullsend-ai/test-creator)
- [Vulnerability Impact Prioritization ADR](../Evidence-Driven%20Vulnerability%20Impact%20Prioritization.md)
