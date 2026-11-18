"""Optional legacy pipeline compatibility projection for canonical ATG results."""

from __future__ import annotations

from collections.abc import Mapping

from atg.domain.digests import digests_equal
from atg.domain.generation import validate_changed_files
from atg.ports.generation import MaterializationResult, PipelineCompatibilityResult


def to_pipeline_compatibility(
    generation_record,
    materialization: MaterializationResult | None,
    *,
    pr_url: str | None = None,
    chains_git_url: str | None = None,
    chains_git_commit: str | None = None,
    run_ref: str | None = None,
    evidence_ref: str | None = None,
) -> PipelineCompatibilityResult:
    """Project optional legacy fields without changing the canonical record."""
    manifest = generation_record.manifest
    status = manifest.get("status")
    if status not in {"generated", "verified", "rejected", "inconclusive"}:
        raise ValueError("unsupported generation terminal status")
    changed_test_count = 0
    if status in {"generated", "verified"}:
        if materialization is None:
            raise ValueError("successful compatibility result requires materialization")
        subjects = (manifest.get("base"), manifest.get("candidate"))
        if dict(materialization.workspace_identity) not in subjects:
            raise ValueError("compatibility workspace does not match a generation subject")
        if (
            not digests_equal(materialization.patch_digest, manifest.get("patch_digest"))
            or not digests_equal(
                materialization.materialized_test_digest,
                manifest.get("materialized_test_digest"),
            )
        ):
            raise ValueError("compatibility materialization differs from canonical generation")
        handoff = manifest.get("materialization_handoff")
        if not isinstance(handoff, Mapping) or handoff.get("status") != "ready":
            raise ValueError("compatibility result requires a canonical materialization handoff")
        allowed_paths = handoff.get("allowed_paths")
        if not isinstance(allowed_paths, (list, tuple)) or not all(
            isinstance(path, str) for path in allowed_paths
        ):
            raise ValueError("canonical materialization handoff has invalid allowed paths")
        canonical_allowed = tuple(allowed_paths)
        if not isinstance(materialization.allowed_paths, (list, tuple)) or not all(
            isinstance(path, str) for path in materialization.allowed_paths
        ):
            raise ValueError("materialization result has invalid allowed paths")
        handoff_files = handoff.get("changed_files")
        generation_files = manifest.get("changed_files")
        if any(
            not isinstance(paths, (list, tuple))
            or not all(isinstance(path, str) for path in paths)
            for paths in (handoff_files, generation_files, materialization.changed_files)
        ):
            raise ValueError("compatibility path inventories must be lists of strings")
        if tuple(materialization.allowed_paths) != canonical_allowed:
            raise ValueError("compatibility allowlist differs from canonical handoff")
        try:
            changed_files = validate_changed_files(
                materialization.changed_files, canonical_allowed
            )
            canonical_changed = validate_changed_files(handoff_files, canonical_allowed)
            generation_changed = validate_changed_files(generation_files, canonical_allowed)
        except ValueError as exc:
            raise ValueError("compatibility output contains an invalid path") from exc
        if set(changed_files) != set(canonical_changed):
            raise ValueError("compatibility inventory differs from canonical handoff")
        if set(generation_changed) != set(canonical_changed):
            raise ValueError("generation inventory differs from canonical handoff")
        changed_test_count = len(canonical_changed)
    for name, value in (
        ("PR_URL", pr_url),
        ("CHAINS-GIT_URL", chains_git_url),
        ("CHAINS-GIT_COMMIT", chains_git_commit),
        ("RUN_REF", run_ref),
        ("EVIDENCE_REF", evidence_ref),
    ):
        if value is not None and (
            not isinstance(value, str)
            or len(value) > 2048
            or any(character in value for character in "\x00\r\n")
        ):
            raise ValueError(f"{name} must be a bounded string")
    if changed_test_count == 0 and pr_url:
        raise ValueError("PR_URL must be empty when no tests were added")
    return PipelineCompatibilityResult(
        tests_added=changed_test_count,
        chains_git_url=chains_git_url or "",
        chains_git_commit=chains_git_commit or "",
        pr_url=pr_url or "",
        run_ref=run_ref or "",
        evidence_ref=evidence_ref or "",
        terminal_status=str(status),
    )
