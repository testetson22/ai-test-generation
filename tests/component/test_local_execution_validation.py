"""Conservative checks in the opt-in local HTTP validation client."""

import copy
import importlib.util
import json
from pathlib import Path
import unittest


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location(
    "validate_local_execution", ROOT / "scripts" / "validate_local_execution.py"
)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class LocalExecutionValidationTests(unittest.TestCase):
    def setUp(self):
        self.identity = "example.SampleTest#checksBehavior"
        self.xml = (
            b'<testsuite tests="1"><testcase classname="example.SampleTest" '
            b'name="checksBehavior"/></testsuite>'
        )
        self.evidence = {
            "exit_code": 0,
            "succeeded": True,
            "partial": False,
            "summary": {
                "media_type": "application/xml",
                "evidence_complete": True,
                "reported_test_ids": [self.identity],
                "executed_test_ids": [self.identity],
                "failed_test_ids": [],
                "skipped_test_ids": [],
                "missing_test_ids": [],
                "unexpected_test_ids": [],
            },
            "artifacts": {"reports/TEST-example.SampleTest.xml": "report"},
        }

    def client(self, raw=None):
        content = self.xml if raw is None else raw

        class FixtureClient:
            def artifact(self, digest):
                if digest != "report":
                    raise AssertionError("unexpected artifact")
                return content

        return FixtureClient()

    def test_requires_raw_xml_substantiating_nonempty_inventory(self):
        MODULE.verify_reports(self.client(), self.evidence, {self.identity})
        missing = copy.deepcopy(self.evidence)
        missing["artifacts"] = {}
        with self.assertRaisesRegex(ValueError, "raw XML"):
            MODULE.verify_reports(self.client(), missing, {self.identity})
        with self.assertRaisesRegex(ValueError, "must not be empty"):
            MODULE.verify_reports(self.client(), self.evidence, set())

    def test_missing_or_nonpassing_summary_is_not_success(self):
        variants = []
        for field, value in (("exit_code", 1), ("succeeded", False), ("partial", True)):
            altered = copy.deepcopy(self.evidence)
            altered[field] = value
            variants.append(altered)
        for field in ("executed_test_ids", "failed_test_ids", "skipped_test_ids", "missing_test_ids"):
            altered = copy.deepcopy(self.evidence)
            del altered["summary"][field]
            variants.append(altered)
        altered = copy.deepcopy(self.evidence)
        altered["summary"]["skipped_test_ids"] = [self.identity]
        variants.append(altered)
        for evidence in variants:
            with self.subTest(evidence=evidence), self.assertRaises(ValueError):
                MODULE.verify_reports(self.client(), evidence, {self.identity})

    def test_raw_skipped_failed_duplicate_or_unsafe_xml_is_rejected(self):
        variants = [
            self.xml.replace(b"/>", b"><skipped/></testcase>"),
            self.xml.replace(b"/>", b"><failure/></testcase>"),
            self.xml.replace(b"/>", b"><error/></testcase>"),
            self.xml.replace(b"</testsuite>", b'<testcase classname="example.SampleTest" name="checksBehavior"/></testsuite>'),
            b'<!DOCTYPE testsuite><testsuite/>',
        ]
        for raw in variants:
            with self.subTest(raw=raw), self.assertRaises(ValueError):
                MODULE.verify_reports(self.client(raw), self.evidence, {self.identity})

    def test_client_rejects_nonlocal_or_credential_bearing_urls(self):
        for url in ("http://example.com", "http://127.0.0.1.evil.example", "http://user@127.0.0.1", "https://127.0.0.1", "http://127.0.0.1/other"):
            with self.subTest(url=url), self.assertRaises(ValueError):
                MODULE.Client(url, "x" * 32)

    def test_cancellation_uses_actual_termination_manifest_and_requires_partial_evidence(self):
        run = {"run_id": "run", "status": "cancelled", "cancel_requested_at": "2026-11-23T17:00:00Z",
               "generation_attempts": [], "pr_url": "", "evidence_ref": "manifest"}
        manifest = {"run_id": "run", "termination_confirmed": True,
                    "worker_ids": ["worker"], "evidence_refs": ["partial"]}
        evidence = {"source_commit": "a" * 40, "partial": True, "phase": "cancelled",
                    "succeeded": False, "worker_id": "worker", "artifacts": {"command.log": "log"}}

        class FixtureClient:
            def __init__(self, data):
                self.cache = data

            def artifact(self, ref):
                return self.cache[ref]

        data = {"manifest": json.dumps(manifest).encode(), "partial": json.dumps(evidence).encode(), "log": b"interrupted"}
        result = MODULE.verify_cancellation(FixtureClient(data), run, {"commit": "a" * 40})
        self.assertTrue(result["termination_confirmed"])
        self.assertEqual(["partial"], result["partial_evidence_refs"])
        manifest["termination_confirmed"] = False
        with self.assertRaisesRegex(ValueError, "manifest"):
            MODULE.verify_cancellation(FixtureClient(dict(data, manifest=json.dumps(manifest).encode())), run, {"commit": "a" * 40})
        evidence["partial"] = False
        with self.assertRaisesRegex(ValueError, "no active worker"):
            MODULE.verify_cancellation(FixtureClient(dict(data, partial=json.dumps(evidence).encode())), run, {"commit": "a" * 40})


if __name__ == "__main__":
    unittest.main()
