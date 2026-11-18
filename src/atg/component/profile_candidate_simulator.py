"""Run fixture-backed Analyze, Plan, and Code phases and report profile gaps."""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
import re
import threading
import xml.etree.ElementTree as ET
from pathlib import Path
from tempfile import TemporaryDirectory
from urllib.error import HTTPError
from urllib.request import Request, urlopen

from atg.adapters.generation.deterministic import ReplayGenerationAdapter
from atg.adapters.repository.local import LocalRepositoryContextAdapter
from atg.application.execution_profile_candidate import build_execution_profile_candidate
from atg.component.run_state_server import create_server
from atg.domain.generation import canonical_patch
from atg.domain.repository_analysis import (
    CapabilityObservation,
    ContextFact,
    RepositorySubject,
)
from atg.ports.analyzer import AnalyzerEvidenceResult, RawAnalysisArtifact
from atg.ports.generation import GeneratedCandidate, GenerationProvenance, PatchCandidate
from atg.ports.repository import RepositoryContextRequest


_REPOSITORY_ID = "https://example.invalid/profile-candidate-fixture.git"
_REVISION = "a" * 40
_RUN_ID = "run-profile-candidate-simulation"
_REQUEST_ID = "request-profile-candidate-simulation"
_ANALYSIS_ID = "analysis-profile-candidate-simulation"
_PLAN_ID = "plan-profile-candidate-simulation"
_GENERATION_ID = "generation-profile-candidate-simulation"
_OBJECTIVE_ID = "objective-profile-candidate-simulation"
_MODEL_ENDPOINT = {"kind": "model-endpoint", "id": "offline-replay", "version": "1"}


class FixtureMavenAnalyzer:
    """Read only the simulator's local fixture and emit replayable build facts."""

    capabilities = ("maven-build-metadata",)
    producer_identity = {
        "kind": "repository-analyzer",
        "id": "offline-maven-fixture-analyzer",
        "version": "1",
    }

    def __init__(self, repository_root: Path) -> None:
        self._root = repository_root

    def collect(self, subject: RepositorySubject) -> AnalyzerEvidenceResult:
        pom_path = self._root / "pom.xml"
        wrapper_path = self._root / ".mvn" / "wrapper" / "maven-wrapper.properties"
        pom_bytes = _bounded_read(pom_path, 256_000)
        wrapper_bytes = _bounded_read(wrapper_path, 16_000)
        project = ET.fromstring(pom_bytes)
        namespace = {"m": "http://maven.apache.org/POM/4.0.0"}
        application_id = ":".join(
            value
            for value in (
                project.findtext("m:groupId", namespaces=namespace)
                or project.findtext("m:parent/m:groupId", namespaces=namespace),
                project.findtext("m:artifactId", namespaces=namespace),
            )
            if value
        )
        application_version = project.findtext("m:version", namespaces=namespace)
        framework_version = project.findtext(
            "m:parent/m:version", namespaces=namespace
        )
        java_version = project.findtext(
            "m:properties/m:maven.compiler.release", namespaces=namespace
        )
        wrapper_match = re.search(
            rb"apache-maven/([0-9]+\.[0-9]+\.[0-9]+)/", wrapper_bytes
        )
        if not all((application_id, application_version, framework_version, java_version)) or wrapper_match is None:
            raise ValueError("synthetic Maven fixture is missing required metadata")

        pom_digest = _digest(pom_bytes)
        wrapper_digest = _digest(wrapper_bytes)
        facts = (
            ContextFact(
                "fact-simulated-application-identity",
                "discovered",
                f"The fixture POM declares application {application_id}:{application_version}.",
                "pom.xml",
                ("evidence-simulated-pom",),
            ),
            ContextFact(
                "fact-simulated-java-runtime",
                "discovered",
                f"The fixture POM configures Java release {java_version}.",
                "pom.xml",
                ("evidence-simulated-pom",),
            ),
            ContextFact(
                "fact-simulated-framework",
                "discovered",
                f"The fixture POM uses Spring Boot parent version {framework_version}.",
                "pom.xml",
                ("evidence-simulated-pom",),
            ),
            ContextFact(
                "fact-simulated-maven-wrapper",
                "discovered",
                f"The fixture Maven Wrapper requests Maven {wrapper_match.group(1).decode()}.",
                ".mvn/wrapper/maven-wrapper.properties",
                ("evidence-simulated-wrapper",),
            ),
        )
        subject_document = {
            "kind": "git-repository",
            "id": subject.repository_id,
            "version": subject.revision,
            "digest": subject.content_digest,
        }
        evidence = (
            _evidence(
                "evidence-simulated-pom",
                subject_document,
                "fixture://pom.xml",
                pom_digest,
                "The synthetic POM declares the fixture application's Maven coordinates, Java release, and Spring Boot version.",
            ),
            _evidence(
                "evidence-simulated-wrapper",
                subject_document,
                "fixture://.mvn/wrapper/maven-wrapper.properties",
                wrapper_digest,
                "The synthetic Maven Wrapper properties declare the Maven distribution version.",
            ),
        )
        facts_document = {
            "simulation_only": True,
            "application_id": application_id,
            "application_version": application_version,
            "language": "java",
            "runtime": "OpenJDK",
            "runtime_version": java_version,
            "framework": "Spring Boot",
            "framework_version": framework_version,
            "build_tool": "Maven",
            "build_tool_version": wrapper_match.group(1).decode(),
        }
        return AnalyzerEvidenceResult(
            facts=facts,
            evidence=evidence,
            capabilities=(
                CapabilityObservation(
                    "maven-build-metadata",
                    "available",
                    required=True,
                    detail="Parsed only the synthetic local fixture; no Maven command was run.",
                ),
            ),
            findings=(),
            artifacts=(
                RawAnalysisArtifact(
                    "artifact-simulated-maven-facts",
                    "application/vnd.atg.simulation.maven-facts+json",
                    facts_document,
                ),
            ),
        )


def simulate_profile_candidate() -> dict[str, object]:
    """Run real reference phase services with synthetic repository/model inputs."""
    with TemporaryDirectory(prefix="atg-profile-candidate-sim-") as directory:
        repository_root = Path(directory)
        _write_fixture_repository(repository_root)
        repository_context = LocalRepositoryContextAdapter().collect(
            RepositoryContextRequest(
                RepositorySubject(_REPOSITORY_ID, _REVISION), str(repository_root)
            )
        )
        repository_digest = repository_context.profile.scanned_content_digest
        subject = RepositorySubject(_REPOSITORY_ID, _REVISION, repository_digest)
        code = _generated_test()
        patch = canonical_patch({}, code)
        fake_generator = ReplayGenerationAdapter(
            {
                _GENERATION_ID: (
                    GeneratedCandidate(
                        PatchCandidate(patch, code),
                        _fake_generation_provenance(),
                    ),
                )
            }
        )
        capabilities = (
            "repository-discovery",
            "test-discovery",
            "maven-build-metadata",
        )
        server = create_server(
            capabilities=capabilities,
            repository_roots={(_REPOSITORY_ID, _REVISION): str(repository_root)},
            analyzers=(FixtureMavenAnalyzer(repository_root),),
            generation_adapter=fake_generator,
        )
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            base_url = f"http://127.0.0.1:{server.server_address[1]}"
            admission = _post(
                base_url,
                "/admissions",
                _admission_document(repository_digest),
                idempotency_key="profile-candidate-simulation-admission",
            )
            analysis = _post(
                base_url,
                "/analyses",
                _analysis_document(repository_digest),
            )
            if not isinstance(analysis.get("checkpoint"), dict):
                raise RuntimeError(
                    "simulated Analyze phase did not create its review checkpoint: "
                    + json.dumps(analysis.get("manifest", {}), sort_keys=True)
                )
            analysis_review = _approve_checkpoint(
                base_url,
                analysis["checkpoint"],
                "decision-simulated-analysis-review",
            )
            plan = _post(
                base_url,
                "/plans",
                {
                    "plan_id": _PLAN_ID,
                    "analysis_id": _ANALYSIS_ID,
                    "run_id": _RUN_ID,
                    "expected_revision": _get(base_url, f"/runs/{_RUN_ID}")["revision"],
                    "occurred_at": _timestamp(),
                },
            )
            plan_review = _approve_checkpoint(
                base_url,
                plan["checkpoint"],
                "decision-simulated-plan-review",
            )
            generation = _post(
                base_url,
                "/generations",
                _generation_document(
                    repository_digest,
                    _get(base_url, f"/runs/{_RUN_ID}")["revision"],
                ),
                idempotency_key="profile-candidate-simulation-generation",
            )
            analysis_record = server.store.get_analysis(_ANALYSIS_ID)
            plan_record = server.store.get_plan(_PLAN_ID)
            generation_record = server.store.get_generation(_GENERATION_ID)
            candidate = build_execution_profile_candidate(
                analysis_record,
                plan_record,
                generation_record,
                simulated_reviews=[analysis_review, plan_review],
            )
            run_state = _get(base_url, f"/runs/{_RUN_ID}")
            candidate["run_state"] = {
                "status": run_state.get("status"),
                "revision": run_state.get("revision"),
                "active_plan_id": run_state.get("active_plan_id"),
                "generation_in_progress": run_state.get("generation_in_progress"),
            }
            candidate["simulation_activity"] = {
                "external_network_calls": 0,
                "subprocesses": 0,
                "maven_commands": 0,
                "repository_checkouts": 0,
                "local_api": "loopback-only",
            }
            candidate["admission_status"] = admission.get("status")
            candidate["phase_response_statuses"] = {
                "analyze": analysis["manifest"].get("outcome"),
                "plan": candidate["phase_outputs"]["plan"].get("status"),
                "code": generation["manifest"].get("status"),
            }
            return candidate
        finally:
            server.shutdown()
            thread.join(timeout=5)
            server.server_close()
            server.store.close()


def _write_fixture_repository(root: Path) -> None:
    (root / ".mvn" / "wrapper").mkdir(parents=True)
    (root / "src/main/java/sample").mkdir(parents=True)
    (root / "src/test/java/sample").mkdir(parents=True)
    (root / "pom.xml").write_text(
        """<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <parent>
    <groupId>org.springframework.boot</groupId>
    <artifactId>spring-boot-starter-parent</artifactId>
    <version>2.7.18</version>
    <relativePath/>
  </parent>
  <groupId>example.sim</groupId>
  <artifactId>profile-candidate-fixture</artifactId>
  <version>1.0.0</version>
  <properties><maven.compiler.release>17</maven.compiler.release></properties>
</project>
""",
        encoding="utf-8",
    )
    (root / ".mvn/wrapper/maven-wrapper.properties").write_text(
        "distributionUrl=https://repo.maven.apache.org/maven2/org/apache/maven/apache-maven/3.9.9/apache-maven-3.9.9-bin.zip\n",
        encoding="utf-8",
    )
    (root / "src/main/java/sample/SampleService.java").write_text(
        "package sample;\n\npublic final class SampleService {\n    public int value() { return 1; }\n}\n",
        encoding="utf-8",
    )
    (root / "src/test/java/sample/SampleServiceTest.java").write_text(
        "package sample;\n\nclass SampleServiceTest { }\n", encoding="utf-8"
    )


def _generated_test() -> dict[str, str]:
    return {
        "src/test/java/sample/GeneratedSampleServiceTest.java": """package sample;

import static org.junit.jupiter.api.Assertions.assertEquals;
import org.junit.jupiter.api.Test;

class GeneratedSampleServiceTest {
    @Test
    void checksSampleServiceValue() {
        assertEquals(1, new SampleService().value());
    }
}
"""
    }


def _fake_generation_provenance() -> GenerationProvenance:
    return GenerationProvenance(
        model={"kind": "model", "id": "simulated-model", "version": "1"},
        model_endpoint=_MODEL_ENDPOINT,
        tool={"kind": "generation-tool", "id": "replay-adapter", "version": "1"},
        worker={"kind": "generation-worker", "id": "simulated-worker", "version": "1"},
        prompt_digest=_digest(b"synthetic prompt"),
        config_digest=_digest(b"synthetic configuration"),
        invocation_digest=_digest(b"synthetic invocation"),
    )


def _admission_document(repository_digest: str) -> dict[str, object]:
    return {
        "contract": "atg.request",
        "version": "1.0",
        "request_id": _REQUEST_ID,
        "run_id": _RUN_ID,
        "submitted_at": _timestamp(),
        "requested_by": "offline-simulation-user",
        "tenant_id": "offline-simulation",
        "authorization_ref": "simulation-only-not-authority",
        "requested_capabilities": [
            "repository-discovery",
            "test-discovery",
            "maven-build-metadata",
        ],
        "mode": "full",
        "objective_ids": [_OBJECTIVE_ID],
        "source": {
            "identity": {
                "kind": "simulation-fixture",
                "id": "profile-candidate-fixture",
                "version": "1",
            },
            "locator": "https://example.invalid/profile-candidate-fixture.git",
        },
        "repository": {
            "kind": "git-repository",
            "id": _REPOSITORY_ID,
            "version": _REVISION,
            "digest": repository_digest,
        },
    }


def _analysis_document(repository_digest: str) -> dict[str, object]:
    observed_at = _timestamp()
    subject = {
        "kind": "git-repository",
        "id": _REPOSITORY_ID,
        "version": _REVISION,
        "digest": repository_digest,
    }
    evidence_id = "evidence-simulated-objective"
    return {
        "analysis_id": _ANALYSIS_ID,
        "request_id": _REQUEST_ID,
        "run_id": _RUN_ID,
        "repository": subject,
        "expected_revision": 1,
        "occurred_at": observed_at,
        "objective_refs": [_OBJECTIVE_ID],
        "requested_capabilities": [
            "repository-discovery",
            "test-discovery",
            "maven-build-metadata",
        ],
        "supplied_evidence": [
            {
                "contract": "atg.evidence",
                "version": "1.0",
                "evidence_id": evidence_id,
                "category": "repository",
                "subject": subject,
                "source": {
                    "identity": {
                        "kind": "simulation-fixture",
                        "id": "profile-candidate-objective",
                        "version": "1",
                    },
                    "locator": "https://example.invalid/profile-candidate-fixture/objective",
                },
                "observed_at": observed_at,
                "claims": [
                    "The synthetic objective is to characterize SampleService.value()."
                ],
                "artifacts": [],
                "limitations": [
                    {
                        "code": "simulation-only",
                        "description": "This objective is fixture input, not a real user request.",
                        "severity": "info",
                    }
                ],
            }
        ],
        "supplied_objectives": [
            {
                "contract": "atg.objective",
                "version": "1.0",
                "objective_id": _OBJECTIVE_ID,
                "statement": "Characterize the sample service's value behavior.",
                "kind": "behavior",
                "driver_type": "user_objective",
                "priority": "medium",
                "test_level": "unit",
                "acceptance": ["SampleService.value() returns 1."],
                "evidence_refs": [evidence_id],
                "target_refs": ["SampleService"],
            }
        ],
    }


def _generation_document(repository_digest: str, expected_revision: int) -> dict[str, object]:
    return {
        "generation_id": _GENERATION_ID,
        "plan_id": _PLAN_ID,
        "run_id": _RUN_ID,
        "expected_revision": expected_revision,
        "created_at": _timestamp(),
        "base": {
            "kind": "git-tree",
            "id": "profile-candidate-base",
            "version": _REVISION,
            "digest": repository_digest,
        },
        "candidate": {
            "kind": "git-tree",
            "id": "profile-candidate-subject",
            "version": _REVISION,
            "digest": repository_digest,
        },
        "model_endpoint": _MODEL_ENDPOINT,
    }


def _approve_checkpoint(base_url: str, checkpoint: dict, decision_id: str) -> dict[str, object]:
    state = _get(base_url, f"/runs/{_RUN_ID}")
    occurred_at = _timestamp()
    _post(
        base_url,
        f"/checkpoints/{checkpoint['checkpoint_id']}/decision",
        {
            "run_id": _RUN_ID,
            "expected_revision": state["revision"],
            "occurred_at": occurred_at,
            "decision": "approved",
            "decision_id": decision_id,
            "actor_id": "simulated-reviewer",
            "actor_role": "simulation-only",
            "authorization_ref": "simulation-only-not-authority",
            "reason": "Synthetic decision to exercise the next phase gate only.",
        },
    )
    return {
        "checkpoint_id": checkpoint["checkpoint_id"],
        "decision_id": decision_id,
        "decision": "approved",
        "simulated": True,
        "authorization_ref": "simulation-only-not-authority",
    }


def _post(
    base_url: str,
    path: str,
    body: dict[str, object],
    *,
    idempotency_key: str | None = None,
) -> dict[str, object]:
    headers = {"Content-Type": "application/json"}
    if idempotency_key:
        headers["Idempotency-Key"] = idempotency_key
    request = Request(
        base_url + path,
        data=json.dumps(body).encode("utf-8"),
        headers=headers,
        method="POST",
    )
    return _send(request)


def _get(base_url: str, path: str) -> dict[str, object]:
    return _send(Request(base_url + path, method="GET"))


def _send(request: Request) -> dict[str, object]:
    try:
        with urlopen(request, timeout=5) as response:
            document = json.loads(response.read())
    except HTTPError as exc:
        try:
            detail = exc.read().decode("utf-8", errors="replace")
        finally:
            exc.close()
        raise RuntimeError(f"local phase API returned HTTP {exc.code}: {detail}") from exc
    if not isinstance(document, dict):
        raise RuntimeError("local phase API returned a non-object response")
    return document


def _evidence(
    evidence_id: str,
    subject: dict[str, object],
    locator: str,
    content_digest: str,
    claim: str,
) -> dict[str, object]:
    return {
        "contract": "atg.evidence",
        "version": "1.0",
        "evidence_id": evidence_id,
        "category": "repository",
        "subject": subject,
        "source": {
            "identity": {
                "kind": "repository-analyzer",
                "id": "offline-maven-fixture-analyzer",
                "version": "1",
            },
            "locator": locator,
            "content_digest": content_digest,
        },
        "observed_at": _timestamp(),
        "claims": [claim],
        "artifacts": [],
        "limitations": [
            {
                "code": "simulation-only",
                "description": "The analyzed file belongs to a temporary synthetic fixture.",
                "severity": "info",
            }
        ],
    }


def _bounded_read(path: Path, maximum: int) -> bytes:
    content = path.read_bytes()
    if not content or len(content) > maximum:
        raise ValueError(f"fixture file {path.name} is empty or oversized")
    return content


def _digest(content: bytes) -> str:
    return "sha256:" + hashlib.sha256(content).hexdigest()


def _timestamp() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace(
        "+00:00", "Z"
    )


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Simulate Analyze -> Plan -> Code and emit execution-profile gaps."
    )
    parser.parse_args()
    report = simulate_profile_candidate()
    print(json.dumps(report, indent=2, sort_keys=True))


if __name__ == "__main__":
    main()
