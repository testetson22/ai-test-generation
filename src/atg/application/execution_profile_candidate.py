"""Build a non-dispatchable profile proposal from Analyze, Plan, and Code artifacts."""

from __future__ import annotations

from collections.abc import Mapping

from atg.domain.schema_validation import ContractValidationError, validate_contract_document


def build_execution_profile_candidate(
    analysis_record,
    plan_record,
    generation_record,
    *,
    simulated_reviews: list[dict[str, object]],
) -> dict[str, object]:
    """Summarize phase evidence without implying profile approval or execution."""
    analysis = analysis_record.manifest
    plan = plan_record.manifest
    generation = generation_record.manifest
    run_id = analysis.get("run_id")
    if (
        not run_id
        or plan.get("run_id") != run_id
        or generation.get("run_id") != run_id
        or plan.get("analysis_id") != analysis.get("analysis_id")
        or generation.get("plan_id") != plan.get("plan_id")
    ):
        raise ValueError("Analyze, Plan, and Code artifacts are not bound to the same run")

    repository_profile = _artifact_content(
        analysis_record, "application/vnd.atg.repository-profile+json"
    )
    test_inventory = _artifact_content(
        analysis_record, "application/vnd.atg.test-inventory+json"
    )
    maven_facts = _artifact_content(
        analysis_record, "application/vnd.atg.simulation.maven-facts+json"
    )
    analysis_artifact = _artifact_reference(
        analysis_record, "application/vnd.atg.analysis+json"
    )
    repository_profile_artifact = _artifact_reference(
        analysis_record, "application/vnd.atg.repository-profile+json"
    )
    inventory_artifact = _artifact_reference(
        analysis_record, "application/vnd.atg.test-inventory+json"
    )
    maven_facts_artifact = _artifact_reference(
        analysis_record, "application/vnd.atg.simulation.maven-facts+json"
    )
    plan_artifact = _artifact_reference(plan_record, "application/vnd.atg.plan+json")
    code_patch_artifact = _artifact_reference(generation_record, "text/x-diff")
    code_tests_artifact = _artifact_reference(
        generation_record, "application/vnd.atg.materialized-tests+json"
    )

    subject = analysis["subject"]
    languages = repository_profile.get("languages", [])
    build_systems = repository_profile.get("build_systems", [])
    target = {
        "language": languages[0] if len(languages) == 1 else None,
        "framework": maven_facts.get("framework"),
        "framework_version": maven_facts.get("framework_version"),
        "runtime": {
            "kind": "runtime",
            "id": maven_facts.get("runtime"),
            "version": maven_facts.get("runtime_version"),
        },
        "build_tool": {
            "kind": "build-tool",
            "id": build_systems[0] if len(build_systems) == 1 else None,
            "version": maven_facts.get("build_tool_version"),
        },
    }
    generated_paths = list(generation.get("changed_files", []))
    attempt_checks = [
        {
            "attempt": attempt.get("attempt"),
            "status": attempt.get("status"),
            "checks": attempt.get("checks", []),
        }
        for attempt in generation.get("attempts", [])
    ]

    candidate_profile = {
        "contract": "atg.execution_profile",
        "version": "1.0",
        "application": {
            "kind": "application",
            "id": maven_facts.get("application_id"),
            "version": maven_facts.get("application_version"),
        },
        "source_policy": {
            "repository": subject.get("id"),
            "revision_policy": "immutable",
            "allowed_source_roots": repository_profile.get("source_roots", []),
        },
        "target": target,
        "commands": [],
        "test_selection": {
            "command_id": None,
            "selector": None,
            "report_media_type": None,
            "minimum_test_count": None,
        },
    }

    profile_gaps = [
        _gap(
            "profile_id/profile_version",
            "profile owner",
            "Assign a stable profile identity and version after the candidate is reviewed.",
        ),
        _gap(
            "worker_image",
            "execution platform",
            "Select an approved worker image and bind it by OCI digest; Analyze, Plan, and Code do not select images.",
        ),
        _gap(
            "commands",
            "profile owner and execution platform",
            "Plan currently emits intent steps, not bounded argv, working directories, and timeouts verified in the worker.",
        ),
        _gap(
            "test_selection",
            "profile owner and test framework adapter",
            "Code identifies generated paths, but no focused selector, supported report type, or measured minimum test count is established.",
        ),
        _gap(
            "limits",
            "execution platform",
            "CPU, memory, PID, wall-time, output, and artifact limits require platform policy and workload measurements.",
        ),
        _gap(
            "network_policy",
            "security/platform owner",
            "Decide whether execution has no network or a specific dependency-host allowlist.",
        ),
        _gap(
            "credential_policy",
            "security/platform owner",
            "Confirm workload credentials remain absent and identify any separately authorized acquisition credential references.",
        ),
        _gap(
            "application_lifecycle",
            "test/profile owner",
            "Determine whether tests need application startup, health checks, reset, or cleanup commands.",
        ),
        _gap(
            "profile_approval",
            "authorized reviewer",
            "The phase-review decisions in this simulation are synthetic and do not authorize an execution profile.",
        ),
        _gap(
            "profile_artifact_digest",
            "profile owner",
            "A final profile artifact digest can only be computed after required fields are completed and approved.",
        ),
    ]
    integration_gaps = [
        _gap(
            "target_repository_analysis",
            "integration owner",
            "Analyze ran against a synthetic temporary repository, not the pinned application repository.",
        ),
        _gap(
            "ai_generation_harness",
            "generation provider owner",
            "Code used ReplayGenerationAdapter with synthetic model, tool, and worker identities; no AI harness was invoked.",
        ),
        _gap(
            "maven_execution_provider",
            "execution provider owner",
            "No Maven command, repository worker, or external execution provider was invoked.",
        ),
    ]
    if generation.get("status") == "inconclusive":
        integration_gaps.append(
            _gap(
                "java_validation",
                "language execution adapter",
                "The replayed Java candidate was not compiled or collected by Maven; the reference validator reports these checks as unsupported.",
            )
        )
    try:
        validate_contract_document(candidate_profile, "execution-profile")
    except ContractValidationError as exc:
        schema_validation = {"valid": False, "detail": str(exc)}
    else:
        schema_validation = {"valid": True, "detail": "candidate unexpectedly passed the full-profile schema"}

    return {
        "simulation_only": True,
        "simulation_note": (
            "Phase inputs and reviewer decisions are fixtures. Repository, framework, and toolchain facts describe the synthetic fixture only."
        ),
        "run_id": run_id,
        "execution_profile_approval": "not_submitted",
        "dispatchable": False,
        "profile_digest": None,
        "bound_subject": dict(subject),
        "candidate_profile_schema_validation": schema_validation,
        "phase_reviews": simulated_reviews,
        "phase_outputs": {
            "analyze": {
                "analysis_id": analysis.get("analysis_id"),
                "outcome": analysis.get("outcome"),
                "repository_profile_digest": analysis.get("profile_digest"),
                "languages": languages,
                "build_systems": build_systems,
                "source_roots": repository_profile.get("source_roots", []),
                "test_roots": repository_profile.get("test_roots", []),
                "discovered_test_files": test_inventory,
                "facts": analysis.get("fact_summary", []),
                "simulated_build_facts": maven_facts,
            },
            "plan": {
                "plan_id": plan.get("plan_id"),
                "status": plan.get("status"),
                "allowed_paths": plan.get("allowed_paths", []),
                "steps": plan.get("steps", []),
            },
            "code": {
                "generation_id": generation.get("generation_id"),
                "status": generation.get("status"),
                "changed_files": generated_paths,
                "patch_digest": generation.get("patch_digest"),
                "validation_attempts": attempt_checks,
                "model": generation.get("model"),
                "tool": generation.get("tool"),
                "worker": generation.get("worker"),
            },
        },
        "candidate_profile": candidate_profile,
        "field_evidence": {
            "application": [maven_facts_artifact],
            "source_policy": [repository_profile_artifact],
            "target": [maven_facts_artifact, repository_profile_artifact],
            "commands": [plan_artifact],
            "test_selection": [inventory_artifact, plan_artifact, code_tests_artifact],
            "test_roots": [inventory_artifact, plan_artifact],
            "generated_test_paths": [code_tests_artifact],
            "generated_patch": [code_patch_artifact],
            "analysis_manifest": [analysis_artifact],
            "plan_intent": [plan_artifact],
        },
        "profile_gaps": profile_gaps,
        "integration_gaps": integration_gaps,
    }


def _artifact_content(record, media_type: str):
    artifact = _find_artifact(record, media_type)
    content = artifact.get("content")
    if not isinstance(content, (Mapping, list)):
        raise ValueError(f"phase artifact {media_type!r} has no usable content")
    return content


def _artifact_reference(record, media_type: str) -> dict[str, object]:
    metadata = _find_artifact(record, media_type).get("artifact")
    if not isinstance(metadata, Mapping):
        raise ValueError(f"phase artifact {media_type!r} has no metadata")
    return {
        "artifact_id": metadata.get("artifact_id"),
        "media_type": metadata.get("media_type"),
        "digest": metadata.get("digest"),
    }


def _find_artifact(record, media_type: str) -> Mapping[str, object]:
    for artifact in record.artifacts:
        metadata = artifact.get("artifact")
        if isinstance(metadata, Mapping) and metadata.get("media_type") == media_type:
            return artifact
    raise ValueError(f"phase artifact {media_type!r} is missing")


def _gap(field: str, owner: str, reason: str) -> dict[str, str]:
    return {"field": field, "owner": owner, "reason": reason}
