"""Component tests for the offline execution-profile candidate simulation."""

from __future__ import annotations

import re
import unittest

from atg.component.profile_candidate_simulator import simulate_profile_candidate
from atg.domain.schema_validation import ContractValidationError, validate_contract_document


class ProfileCandidateSimulatorTests(unittest.TestCase):
    def test_runs_reference_phases_and_reports_separate_profile_and_integration_gaps(self):
        report = simulate_profile_candidate()

        self.assertTrue(report["simulation_only"])
        self.assertFalse(report["dispatchable"])
        self.assertEqual(report["execution_profile_approval"], "not_submitted")
        self.assertIsNone(report["profile_digest"])
        self.assertEqual(report["candidate_profile_schema_validation"]["valid"], False)
        self.assertEqual(
            report["simulation_activity"],
            {
                "external_network_calls": 0,
                "subprocesses": 0,
                "maven_commands": 0,
                "repository_checkouts": 0,
                "local_api": "loopback-only",
            },
        )

        phases = report["phase_outputs"]
        self.assertEqual(phases["analyze"]["outcome"], "needs_human_review")
        self.assertEqual(phases["analyze"]["languages"], ["java"])
        self.assertEqual(phases["analyze"]["build_systems"], ["maven"])
        self.assertEqual(phases["analyze"]["simulated_build_facts"]["framework_version"], "2.7.18")
        self.assertEqual(phases["plan"]["status"], "approved")
        self.assertEqual(phases["plan"]["allowed_paths"], ["src/test/"])
        self.assertEqual(phases["code"]["status"], "inconclusive")
        self.assertEqual(
            phases["code"]["changed_files"],
            ["src/test/java/sample/GeneratedSampleServiceTest.java"],
        )
        self.assertTrue(all(review["simulated"] for review in report["phase_reviews"]))

        profile_gap_fields = {gap["field"] for gap in report["profile_gaps"]}
        self.assertTrue(
            {
                "worker_image",
                "commands",
                "test_selection",
                "limits",
                "network_policy",
                "credential_policy",
                "profile_approval",
            }.issubset(profile_gap_fields)
        )
        integration_gap_fields = {gap["field"] for gap in report["integration_gaps"]}
        self.assertTrue(
            {
                "target_repository_analysis",
                "ai_generation_harness",
                "maven_execution_provider",
                "java_validation",
            }.issubset(integration_gap_fields)
        )

        for evidence_refs in report["field_evidence"].values():
            self.assertTrue(evidence_refs)
            for evidence in evidence_refs:
                self.assertRegex(evidence["digest"], re.compile(r"^sha256:[0-9a-f]{64}$"))

    def test_partial_candidate_is_rejected_by_the_approved_profile_contract(self):
        report = simulate_profile_candidate()
        with self.assertRaises(ContractValidationError):
            validate_contract_document(report["candidate_profile"], "execution-profile")


if __name__ == "__main__":
    unittest.main()
