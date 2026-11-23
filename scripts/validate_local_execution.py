#!/usr/bin/env python3
"""Opt-in black-box check of the real local execution-only HTTP service."""

from __future__ import annotations

import argparse
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import sys
import time
from urllib import error, parse, request
import uuid
import xml.etree.ElementTree as ET


MAX_ARTIFACT_BYTES = 40 * 1024 * 1024
DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ValueError(message)


class Client:
    def __init__(self, url: str, token: str):
        target = parse.urlsplit(url)
        require(target.scheme == "http" and not target.username and not target.query
                and not target.fragment and target.path in ("", "/"), "expected a local HTTP service URL")
        require(ipaddress.ip_address(target.hostname or "").is_loopback, "service must use a literal loopback IP")
        require(len(token) >= 24 and not any(c in token for c in "\r\n\x00"), "a development bearer token is required")
        self.url = url.rstrip("/")
        self.token = token
        self.cache: dict[str, bytes] = {}
        self.total_bytes = 0

    def call(self, path: str, body: bytes | None = None, key: str | None = None) -> bytes:
        headers = {"Authorization": "Bearer " + self.token}
        if body is not None:
            headers["Content-Type"] = "application/json"
        if key:
            headers["Idempotency-Key"] = key
        req = request.Request(self.url + path, data=body, headers=headers)
        with request.urlopen(req, timeout=15) as response:
            data = response.read(MAX_ARTIFACT_BYTES + 1)
            require(len(data) <= MAX_ARTIFACT_BYTES, "HTTP response exceeds limit")
            return data

    def json(self, path: str, body: bytes | None = None, key: str | None = None) -> dict:
        result = json.loads(self.call(path, body, key))
        require(isinstance(result, dict), "expected a JSON object")
        return result

    def artifact(self, digest: str) -> bytes:
        require(isinstance(digest, str) and DIGEST.fullmatch(digest) is not None, "invalid artifact reference")
        if digest not in self.cache:
            data = self.call("/development/artifacts/" + digest)
            require("sha256:" + hashlib.sha256(data).hexdigest() == digest, "downloaded artifact digest mismatch")
            self.total_bytes += len(data)
            require(self.total_bytes <= 160 * 1024 * 1024 and len(self.cache) < 256, "artifact inventory exceeds limit")
            self.cache[digest] = data
        return self.cache[digest]


def verify_reports(client: Client, evidence: dict, expected: set[str]) -> None:
    require(bool(expected), "expected inventory must not be empty")
    require(evidence.get("exit_code") == 0 and evidence.get("succeeded") is True
            and evidence.get("partial") is False, "worker did not report a complete successful command")
    summary = evidence["summary"]
    require(summary.get("evidence_complete") is True and summary.get("media_type") == "application/xml", "incomplete report summary")
    for field in ("failed_test_ids", "skipped_test_ids", "missing_test_ids", "unexpected_test_ids"):
        require(field in summary and summary[field] in (None, []), "nonpassing or missing report outcome: " + field)
    for field in ("reported_test_ids", "executed_test_ids"):
        values = summary.get(field)
        require(isinstance(values, list) and len(values) == len(expected)
                and set(values) == expected, "wrong or empty test inventory: " + field)
    observed: set[str] = set()
    reports = 0
    for name, digest in evidence["artifacts"].items():
        data = client.artifact(digest)
        if not (name.startswith("reports/TEST-") and name.endswith(".xml")):
            continue
        reports += 1
        require(len(data) <= 10_000_000 and b"<!DOCTYPE" not in data.upper()
                and b"<!ENTITY" not in data.upper(), "unsafe or oversized XML")
        for case in ET.fromstring(data).iter("testcase"):
            identity = case.attrib["classname"] + "#" + case.attrib["name"]
            require(identity not in observed, "duplicate XML test identity")
            require(not any(c.tag in ("failure", "error", "skipped") for c in case), "nonpassing raw XML testcase")
            observed.add(identity)
    require(reports > 0 and observed == expected, "raw XML does not substantiate the expected inventory")


def verify_result(client: Client, run: dict, supplied: dict, profile: dict) -> dict:
    require(run["status"] == "completed" and run["stage"] == "done", "run did not complete")
    require(run["generation_attempts"] == [] and run["pr_url"] == "", "execution-only run invoked generation or PR publication")
    require(run["request"]["input_mode"] == "artifact_input"
            and run["request"]["completion_mode"] == "patch_only", "wrong execution/publication mode")
    require(run["resolved_commit"] == profile["commit"], "wrong application revision")
    paths = sorted(f["path"] for f in supplied["files"])
    require(run["changed_test_paths"] == paths and run["tests_added"] == len(paths), "accepted paths do not match supplied files")
    manifest = json.loads(client.artifact(run["result_artifact_ref"]))
    require(manifest["run_id"] == run["run_id"] and manifest["cleanup_confirmed"] is True,
            "result lacks attributable confirmed cleanup")
    require(manifest["request"] == run["request"] and manifest["patch_digest"] == run["patch_digest"], "result input/candidate binding mismatch")
    client.artifact(manifest["provisional_ref"])
    require(json.loads(client.artifact(manifest["test_artifact_digest"])) == supplied, "retained test artifact differs from admitted input")
    patch = client.artifact(manifest["patch_digest"]).decode("utf-8")
    expected_patch = ""
    for file in sorted(supplied["files"], key=lambda f: f["path"]):
        lines = file["content"].removesuffix("\n").split("\n")
        path = file["path"]
        expected_patch += (f"diff --git a/{path} b/{path}\nnew file mode 100644\n--- /dev/null\n"
                           f"+++ b/{path}\n@@ -0,0 +1,{len(lines)} @@\n"
                           + "".join("+" + line + "\n" for line in lines))
    require(patch == expected_patch, "retained patch differs from the supplied test files")
    baseline = {class_name + "#" + method for class_name, methods in profile["baseline_tests"].items() for method in methods}
    candidate = set(supplied["expected_test_ids"])
    require(len(baseline) == 121 and candidate and baseline.isdisjoint(candidate), "invalid independent fixture inventory")
    counts, worker_ids, images = {}, {}, set()
    for op in manifest["operations"]:
        require(op["state"] == "completed", "unresolved operation in completed result")
        if not op["kind"].startswith("maven:"):
            continue
        result = op["result"]
        require(result["Succeeded"] is True, "Maven result was not successful")
        evidence = json.loads(client.artifact(result["EvidenceRef"]))
        require(evidence["source_commit"] == profile["commit"]
                and evidence["profile_digest"] == run["request"]["profile_digest"], "worker provenance mismatch")
        phase = evidence["phase"]
        expected = {"package": baseline, "focused_tests": candidate, "verify": baseline | candidate}[phase]
        require(phase not in counts, "duplicate phase evidence")
        verify_reports(client, evidence, expected)
        counts[phase], worker_ids[phase] = len(expected), evidence["worker_id"]
        images.add(evidence["image_id"])
    require(set(counts) == {"package", "focused_tests", "verify"} and len(images) == 1, "missing phases or mixed worker images")
    source_seen = False
    for ref in manifest["worker_evidence"]:
        evidence = json.loads(client.artifact(ref))
        require(evidence["source_commit"] == profile["commit"], "worker source mismatch")
        for child in evidence["artifacts"].values():
            client.artifact(child)
        source_seen |= evidence["phase"] == "source" and evidence["succeeded"] is True
    require(source_seen, "missing independently retained source acquisition evidence")
    client.artifact(run["request"]["profile_digest"])
    return {"run_id": run["run_id"], "status": "completed", "mode": "artifact_input_patch_only",
            "source_commit": run["resolved_commit"], "profile_digest": run["request"]["profile_digest"],
            "image_id": next(iter(images)), "test_counts": counts, "worker_ids": worker_ids,
            "result_artifact_ref": run["result_artifact_ref"], "verified_artifacts": len(client.cache),
            "verified_artifact_bytes": client.total_bytes}


def verify_cancellation(client: Client, run: dict, profile: dict) -> dict:
    require(run["status"] == "cancelled" and bool(run.get("cancel_requested_at")),
            "run lacks an explicit cancellation request")
    require(run["generation_attempts"] == [] and run["pr_url"] == "", "unexpected generation/publication")
    manifest = json.loads(client.artifact(run["evidence_ref"]))
    require(manifest["run_id"] == run["run_id"] and manifest["termination_confirmed"] is True,
            "cancellation manifest does not match the run")
    partial = []
    for ref in manifest["evidence_refs"]:
        evidence = json.loads(client.artifact(ref))
        require(evidence["source_commit"] == profile["commit"], "cancelled worker source mismatch")
        for child in evidence["artifacts"].values():
            client.artifact(child)
        if evidence["partial"]:
            require(evidence["phase"] == "cancelled" and evidence["succeeded"] is False
                    and evidence["worker_id"] in manifest["worker_ids"], "invalid partial cancellation evidence")
            partial.append(ref)
    require(bool(partial), "no active worker cancellation was demonstrated")
    return {"run_id": run["run_id"], "status": "cancelled", "termination_confirmed": True,
            "evidence_ref": run["evidence_ref"], "worker_ids": manifest["worker_ids"],
            "partial_evidence_refs": partial, "verified_artifacts": len(client.cache)}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default="http://127.0.0.1:18080")
    parser.add_argument("--artifact", type=Path, default=Path("deploy/development-harness/local-spring-test-artifact.json"))
    parser.add_argument("--profile", type=Path, default=Path("workers/java17-maven/profile.json"))
    parser.add_argument("--run-id", help="observe an existing run instead of submitting another")
    parser.add_argument("--timeout-seconds", type=int, default=1800)
    parser.add_argument("--output", type=Path, help="write verified summary to a new file")
    parser.add_argument("--cancel-after-baseline-start", action="store_true",
                        help="exercise cancellation during Maven instead of completing the run")
    args = parser.parse_args()
    require(1 <= args.timeout_seconds <= 3600, "timeout must be between 1 and 3600 seconds")
    client = Client(args.url, os.environ.get("ATG_DEVELOPMENT_BEARER_TOKEN", ""))
    supplied_raw, profile_raw = args.artifact.read_bytes(), args.profile.read_bytes()
    require(len(supplied_raw) <= 1_048_576 and len(profile_raw) <= 131072, "fixture input exceeds limit")
    supplied, profile = json.loads(supplied_raw), json.loads(profile_raw)
    if args.run_id:
        require(re.fullmatch(r"[A-Za-z0-9._:-]{1,256}", args.run_id) is not None, "invalid run ID")
        run_id = args.run_id
    else:
        uploaded = client.json("/development/test-artifacts", supplied_raw)
        created = client.json("/development/test-generation-runs",
                              json.dumps({"input_artifact_digest": uploaded["digest"]}).encode(),
                              "local-validation:" + uuid.uuid4().hex)
        run_id = created["run"]["run_id"]
    print(json.dumps({"run_id": run_id, "action": "observe_execution_only"}), flush=True)
    deadline, previous = time.monotonic() + args.timeout_seconds, None
    cancel_at, cancel_sent = None, False
    while time.monotonic() < deadline:
        try:
            response = client.json("/development/test-generation-runs/" + run_id)
        except error.HTTPError:
            raise
        except (error.URLError, TimeoutError, ConnectionError):
            time.sleep(1)
            continue
        run = response["run"]
        state = (run["status"], run["stage"], run["revision"])
        if state != previous:
            print(json.dumps({"status": state[0], "stage": state[1], "revision": state[2]}), flush=True)
            previous = state
        if args.cancel_after_baseline_start and not cancel_sent and state[:2] == ("running", "baseline_package"):
            if cancel_at is None:
                cancel_at = time.monotonic() + 3
            elif time.monotonic() >= cancel_at:
                client.json("/development/test-generation-runs/" + run_id + "/cancel",
                            json.dumps({"expected_revision": run["revision"]}).encode(),
                            "local-cancellation:" + uuid.uuid4().hex)
                cancel_sent = True
        expected_terminal = "cancelled" if args.cancel_after_baseline_start else "completed"
        if run["status"] == expected_terminal:
            summary = (verify_cancellation(client, run, profile) if args.cancel_after_baseline_start
                       else verify_result(client, run, supplied, profile))
            if args.output:
                with args.output.open("x", encoding="utf-8") as output:
                    json.dump(summary, output, indent=2)
                    output.write("\n")
            print(json.dumps(summary, indent=2), flush=True)
            return 0
        require(run["status"] not in ("completed", "failed", "cancelled", "outcome_unknown"),
                "run terminated without success: " + run["status"] + "; inspect authenticated status/operations/evidence")
        time.sleep(1)
    raise ValueError("validation deadline reached; run remains inspectable and was not automatically cancelled")


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (ValueError, KeyError, OSError, error.HTTPError, ET.ParseError) as exc:
        print("local execution validation failed: " + str(exc), file=sys.stderr)
        raise SystemExit(1)
