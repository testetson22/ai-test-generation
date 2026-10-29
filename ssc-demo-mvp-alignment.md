# SSC Demo MVP Alignment

Date: Oct 29, 2026

## Purpose

This document aligns the AI Test Generation MVP with `/Users/tstetson/Public/workspaces/ssc-demo`. It is a planning and discovery artifact, not a new architecture decision and not a commitment to the demo's current agent implementation.

The objective is a VPE-initiated Java/VEX test-generation run that fits the existing supply-chain demonstration and can replace the current test-generation implementation without replacing the surrounding platform flow.

## Current Demo Baseline

The demo is an OpenShift/Tekton/Argo GitOps environment with GitLab, RHTPA/Trustify, RHTAS, Quay, Artifactory, Keycloak, and an example Maven/Spring Boot application. Tekton resources are GitOps-managed from the configured repository and normally run in namespace `tssc-app-ci`. The default selected Maven pipeline is `maven-build-ci-pipeline-ai.yaml`; the other Maven variants are excluded because they share the same `metadata.name`.

There are two relevant test-generation paths:

| Demo path | Current behavior | MVP alignment |
|---|---|---|
| `agentic-test-generation` | Standalone `clone -> verify commit -> Maven package -> ai-generate-tests -> mvn verify -> tests-only PR/MR` pipeline | Primary compatibility shell for a VPE-initiated test-generation run |
| `maven-build-ci` AI branch | Existing build/SBOM/RHTPA chain, then `conforma-policy-check -> ai-generate-tests -> ai-select-cve -> ai-remediate-dependency -> mvn verify -> PR/MR` when enabled | Existing test-generation slot; candidate for a later integrated flow, but current ordering does not supply a VPE decision to the test task |

The current `ai-generate-tests` Task:

- Assumes Java/Maven and generates JUnit tests under `src/test`.
- Receives only subdirectory, agent image, instructions, source paths, and retry count.
- Uses a configurable coding-agent image and `ai-agent-config`/`ai-agent-secret`.
- Supports OpenAI-compatible endpoints through `AI_BASE_URL`, including RHOAI-style endpoints.
- Copies the repository to a pod-local scratch directory, generates tests, verifies changed tests with Maven, and retries with failure output.
- Copies only generated tests back to the shared workspace.
- Returns `TESTS_ADDED`; the pipeline then runs `mvn verify` and opens a tests-only PR/MR.

The current task does not consume VEX or VPE policy, compare base and candidate subjects, emit signed differential evidence, or expose Analyze/Plan checkpoints. It is therefore a useful replacement seam and smoke-test baseline, not the MVP contract by itself.

## Recommended Replacement Seam

Keep the demo's platform and pipeline shell initially. Replace the implementation behind the test-generation slot rather than replacing Argo, Tekton, GitLab, RHTPA, or the current SCM workflow.

```text
VPE VEX/policy event or approved platform request
  -> normalized AI Test Generation request
  -> Tekton PipelineRun / service adapter
       -> AI Test Generation service
            -> preflight
            -> Analyze -> Plan -> Code
            -> isolated base/candidate execution
            -> signed differential evidence
       -> compatibility result/artifact adapter
  -> existing demo PR/MR and Lightwell evidence path
```

### Initial integration recommendation

1. Use `agentic-test-generation` as the primary compatibility shell because it already represents the test-generation slot without coupling test generation to CVE selection or dependency remediation.
2. Replace or wrap only the `ai-generate-tests` Task with an adapter that invokes the service. Keep `mvn verify` and `open-pr-tests` as compatibility steps while the service contract is proven.
3. Add VPE/VEX and base/candidate parameters to the adapter request; do not encode them in prompt text or overload the current six-field CVE decision.
4. Return the existing `TESTS_ADDED` and `PR_URL` results for demo compatibility, plus references to checkpoints, generated patch, execution results, signed record, and limitations.
5. Add differential execution and evidence publication as explicit tasks or service callbacks before claiming fix validation. The current one-tree `mvn verify` step is not sufficient.
6. Keep the integrated `maven-build-ci` AI branch as a later composition option after the standalone service-adapter path works. Its current test-generation task runs before `ai-select-cve`, so it is not currently a VPE-initiated fix-pinning flow.

This approach allows the demo to show the new service in the existing test-generation position while leaving the high-level Analyze -> Plan -> Code -> Execute design intact.

## Compatibility Contract

The demo currently exposes a task/pipeline contract. The service adapter should translate between it and the MVP domain contract.

### Inputs from VPE/platform

The MVP request should include, at minimum:

- VEX document and statement references, format/version, digest, status, product/component identity, and action/justification.
- VPE policy decision or policy-bundle reference, revision, entrypoint, obligations, and input digest when applicable.
- Vulnerability identity and supporting evidence references.
- Repository URL, immutable base commit, candidate/fixed commit or artifact, and intended change/PR reference.
- Java repository profile or permission to discover it: Maven/Gradle, test framework, module/subdirectory, artifact mirror, and approved commands.
- SCM provider/host, repository, target branch, PR/MR or issue reference, and publication mode.
- Run ID, tenant/authorization context, idempotency key, and callback/evidence destination.

The current demo's `CVE_ID`, `PACKAGE`, `CURRENT_VERSION`, `FIXED_VERSION`, `JUSTIFICATION`, and `SELECTED` values can be retained as compatibility fields, but they are insufficient as the canonical request.

### Outputs to Tekton/platform

The adapter should preserve current results while adding MVP evidence references:

- `TESTS_ADDED` and `PR_URL` for existing demo consumers.
- Run/checkpoint status and terminal classification.
- Exact generated patch and materialized test digests.
- Base and candidate execution result references.
- Signed differential record and attestation verification reference.
- VEX/VPE source references and policy evaluation provenance.
- Quality-gate findings, limitations, blocked/inconclusive state, and stated gaps.
- Human review/publication decision and audit-store evidence reference.

Large source, logs, reports, and patches should remain in content-addressed artifact storage. Tekton results should carry bounded references and status, not full documents.

## What Must Be Learned From the Existing Demo

Before implementation, reproduce the demo as-is and record:

1. Which Argo Applications synchronize the Tekton Tasks, ConfigMap, selected Maven pipeline, and triggers.
2. Which namespace, service accounts, RBAC, PVCs, Secrets, and ConfigMaps are required.
3. How the GitLab issue comment webhook reaches the EventListener and creates a `PipelineRun`.
4. How the current task receives the source repository, revision, workspace, Maven settings, agent image, and AI endpoint configuration.
5. How VEX is represented and loaded: the demo seeds OpenVEX plus CycloneDX artifacts in the Lightwell repository, uploads/uses them through RHTPA, and performs VEX suppression during vulnerability analysis.
6. How the current six-field CVE decision is produced, reviewed, passed to remediation, and linked to the resulting PR/MR.
7. What artifacts and Tekton results survive PipelineRun pruning and what must be copied to the audit/evidence store.
8. What the existing tests-only PR communicates to a reviewer and what the release/policy gate can consume automatically.

The setup runbook is the operational starting point. It requires a cluster with the relevant operators and storage, configures the AI backend before installation, waits for Argo/Tekton resources, creates the AI/SCM/Maven secrets, loads Lightwell remediation data, and smoke-tests `agentic-cve-analysis` followed by `/remediate` or `/generate-tests`. The demo repository is GitOps-managed: local changes have no effect until the configured Argo source repository/branch is updated.

## MVP Setup and Swap Sequence

### Establish the baseline

- Confirm the cluster, domain, storage, namespace, Argo source repository/branch, selected Maven pipeline, GitLab project, and example Java repository.
- Run the existing `/generate-tests` path and capture PipelineRun logs, generated test diff, Maven results, PR/MR, AI endpoint/model configuration, and elapsed/cost data.
- Run the VPE/RHTPA path and capture the vulnerability report, VEX suppression behavior, remediation report, policy result, issue, and selected decision fields.
- Identify which records are ephemeral and copy baseline artifacts before cleanup/pruning.

### Insert the service adapter

- Add a service endpoint/profile and credentials through the demo's GitOps-managed configuration; do not hard-code credentials in Task YAML.
- Introduce an adapter Task or wrapper at the existing `ai-generate-tests` slot.
- Pass a fixture VPE/VEX request with immutable base/candidate identities.
- Have the adapter call the service, poll or receive a callback, and materialize the approved test patch into the existing workspace contract.
- Preserve `mvn verify` and PR/MR creation initially, while attaching the service evidence link and signed-record reference.
- Add a differential execution path and verify that the exact generated test is run against both sides before presenting a fix-pinning conclusion.

### Exercise the VPE-initiated path

The first end-to-end demonstration should be:

```text
VPE VEX finding for a Java dependency
  -> approved test-generation request
  -> service preflight and Analyze/Plan checkpoint
  -> native Java test generation
  -> base/candidate execution
  -> signed differential evidence
  -> tests-only or linked remediation PR/MR
```

The demo can initially use a platform/SCM trigger or a manually created Tekton `PipelineRun` carrying the same request fixture. A direct production VPE webhook should not be invented until the VPE output schema, authentication, and delivery contract are confirmed.

## Current Gaps to Close

| Gap | Current demo behavior | MVP planning response |
|---|---|---|
| VPE trigger | RHTPA/Conforma and issue/comment flows; no explicit VPE service contract | Define a fixture and adapter contract before choosing webhook/event transport |
| VEX input | Sample OpenVEX is seeded and used for RHTPA suppression; the test task does not receive it | Pass immutable VEX document/statement references into the service |
| Fix comparison | Current test generation validates one tree with `mvn verify` | Add explicit base/candidate subjects and differential execution |
| Self-verification | Current task has bounded generation retries, not service preflight/failure-register checks | Add stage-zero preflight to the service adapter flow |
| Evidence | Pipeline results and PR/MR are primary visible outputs | Add signed differential record and audit-store/publication references |
| Checkpoints | Current flow is largely linear; issue review is outside the task | Preserve service Analyze/Plan checkpoint state and map approval to PipelineRun continuation |
| Ownership | Demo owns pipeline tasks and SCM publication; Lightwell/evidence ownership is still to confirm | Keep the adapter contract neutral and record owner decisions during the spike |
| Tool selection | Current demo uses aider/Claude Code options and Maven/JUnit tasks | Treat these as baseline fixtures, not final service-tool selections |

## Non-Goals for the First Swap

- Replacing the demo's Argo, Tekton, GitLab, RHTPA, RHTAS, Quay, or artifact infrastructure.
- Supporting Python, Go, or multiple Java frameworks before the Java pilot path is reproducible.
- Replacing VPE/RHTPA vulnerability prioritization or authoring a new VEX record.
- Introducing a new agent framework or committing to the current agent CLI as the service architecture.
- Claiming full production-simulation or active-exploitation validation.
- Making unattended merge, remediation, VEX updates, or release approval automatic.

## Research-Spike Questions

The spike should compare the current Task-based adapter with a direct service/PipelineRun integration, using the same VPE/VEX fixture:

- Where should the service run relative to the demo cluster and Lightwell platform?
- Should Tekton own execution and PR publication initially, or should the service return artifacts to a dedicated execution Task?
- How are VPE VEX/policy documents retrieved, authenticated, versioned, and preserved?
- How are base and candidate revisions/artifacts made available and kept equivalent for Java/Maven execution?
- Which signed evidence format and audit-store interface can the demo consume?
- How are asynchronous service callbacks, PipelineRun status, retries, cancellation, and cleanup mapped?
- What additional Task results or CRDs are needed without making Tekton results the long-term evidence store?
- Which baseline metrics demonstrate improvement over the current task: latency, model cost, retry rate, test acceptance, review time, evidence completeness, and differential correctness?

## Sources in `ssc-demo`

- `README.md` and `automation/gitops/RUNBOOK.md` for cluster setup and GitOps operation.
- `pipelines/README.md` for pipeline contracts, trigger setup, current test-generation behavior, VEX handling, and smoke tests.
- `pipelines/pipelines/agentic-test-generation.yaml` for the standalone replacement seam.
- `pipelines/tasks/ai-generate-tests.yaml` for the current Java/JUnit agent task contract.
- `pipelines/pipelines/maven-build-ci-pipeline-ai.yaml` for the integrated test-generation slot.
- `pipelines/triggers/agentic-issue-triggers.yaml` for current `/generate-tests` and `/remediate` event wiring.
- `pipelines/config/ai-agent-config.yaml` for configurable OpenAI-compatible model endpoints.
- `automation/gitops/components/lightwell-repo/files/seed/commons-lang3-3.14.0.rhlw-00001.vex.json` for the demo OpenVEX shape.
- `automation/gitops/components/lightwell-repo/values.yaml` and `README.md` for VEX/CDX seed and ingestion behavior.
