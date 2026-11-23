"""Strict stdlib protocol checks, including bindings JSON Schema cannot express."""

from __future__ import annotations

import hashlib
import json
import math
import re
from typing import TypedDict
from urllib.parse import unquote, urlsplit

MAX_TASK_BYTES = 2 * 1024 * 1024
MAX_OUTPUT_BYTES = 1024 * 1024
MAX_DIAGNOSTIC_BYTES = 2048
LIMITS = {
    "timeout_seconds": (1, 600),
    "max_output_bytes": (4096, MAX_OUTPUT_BYTES),
    "max_context_bytes": (1, 1024 * 1024),
    "max_requests": (1, 8),
    "max_output_tokens": (1, 32768),
}
ID = r"[A-Za-z0-9][A-Za-z0-9._:/-]*"
DIGEST = r"sha256:[A-Fa-f0-9]{64}"
COMMIT = r"(?:[A-Fa-f0-9]{40}|[A-Fa-f0-9]{64})"
PATH = r"[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*"
TEST_PATH = r"src/test/java/(?:[A-Za-z_][A-Za-z0-9_]*/)*[A-Za-z_][A-Za-z0-9_]*Test\.java"
TEST_ID = r"[A-Za-z_$][A-Za-z0-9_.$]*#[A-Za-z_$][A-Za-z0-9_$]*"
USAGE_KEYS = {"input_tokens", "output_tokens", "requests"}
TASK_KEYS = {
    "schema_version", "job_id", "run_id", "attempt", "phase", "objective",
    "profile_digest", "policy_digest", "harness", "model", "source", "prior", "limits",
}
RESULT_KEYS = {
    "schema_version", "job_id", "run_id", "attempt", "phase", "request_digest",
    "source_commit", "profile_digest", "policy_digest", "harness", "model", "status",
    "output", "usage", "versions", "diagnostic",
}
REVIEW_LIMITATION = (
    "Human review required; suggested_test_ids are non-authoritative. "
    "No execution or approval is attested."
)


class Citation(TypedDict):
    path: str
    start_line: int
    end_line: int


class Fact(TypedDict):
    claim: str
    citations: list[Citation]


class AnalysisOutput(TypedDict):
    summary: str
    facts: list[Fact]
    gaps: list[str]
    limitations: list[str]


class Scenario(TypedDict):
    name: str
    source_paths: list[str]
    test_path: str
    behavior: str
    oracle: str


class PlanOutput(TypedDict):
    summary: str
    scenarios: list[Scenario]
    limitations: list[str]


class GeneratedFile(TypedDict):
    path: str
    content: str


class CodeOutput(TypedDict):
    summary: str
    files: list[GeneratedFile]
    suggested_test_ids: list[str]
    limitations: list[str]


class ProtocolError(ValueError):
    """Only fixed category codes, never untrusted values, cross this boundary."""


def digest(raw: bytes) -> str:
    return "sha256:" + hashlib.sha256(raw).hexdigest()


def encode(document: object) -> bytes:
    return json.dumps(document, ensure_ascii=False, allow_nan=False, separators=(",", ":")).encode("utf-8")


def _pairs(pairs: list[tuple[str, object]]) -> dict:
    result = {}
    for key, value in pairs:
        if key in result:
            raise ProtocolError("duplicate_key")
        result[key] = value
    return result


def _nonfinite(_: str) -> None:
    raise ProtocolError("nonfinite_number")


def _json_tree(value: object, depth: int = 0) -> None:
    if depth > 32:
        raise ProtocolError("json_depth")
    if isinstance(value, str):
        try:
            value.encode("utf-8")
        except UnicodeError:
            raise ProtocolError("invalid_utf8") from None
    elif isinstance(value, float) and not math.isfinite(value):
        raise ProtocolError("nonfinite_number")
    elif isinstance(value, dict):
        for key, child in value.items():
            _json_tree(key, depth + 1)
            _json_tree(child, depth + 1)
    elif isinstance(value, list):
        for child in value:
            _json_tree(child, depth + 1)


def decode(raw: bytes, maximum: int) -> dict:
    if not isinstance(raw, bytes) or not raw or len(raw) > maximum:
        raise ProtocolError("json_size")
    try:
        value = json.loads(raw.decode("utf-8"), object_pairs_hook=_pairs, parse_constant=_nonfinite)
        _json_tree(value)
    except (ValueError, UnicodeError, RecursionError) as error:
        if isinstance(error, ProtocolError):
            raise
        raise ProtocolError("invalid_json") from None
    if type(value) is not dict:
        raise ProtocolError("object_required")
    return value


def _object(value: object, required: set[str], optional: set[str] = frozenset()) -> dict:
    if type(value) is not dict or not required <= value.keys() or value.keys() - required - optional:
        raise ProtocolError("object_keys")
    return value


def _text(value: object, maximum: int = 16384, pattern: str | None = None, empty: bool = False) -> str:
    if (type(value) is not str or len(value.encode("utf-8")) > maximum
            or (not empty and not value.strip()) or "\x00" in value):
        raise ProtocolError("invalid_string")
    if pattern is not None and re.fullmatch(pattern, value) is None:
        raise ProtocolError("string_pattern")
    return value


def _integer(value: object, minimum: int, maximum: int) -> int:
    if type(value) is not int or not minimum <= value <= maximum:
        raise ProtocolError("integer_bounds")
    return value


def _array(value: object, minimum: int = 0, maximum: int = 128) -> list:
    if type(value) is not list or not minimum <= len(value) <= maximum:
        raise ProtocolError("array_bounds")
    return value


def _strings(value: object, *, pattern: str | None = None, maximum: int = 16384, unique: bool = False) -> list[str]:
    result = [_text(item, maximum, pattern) for item in _array(value)]
    if unique and len(set(result)) != len(result):
        raise ProtocolError("duplicate_value")
    return result


def _path(value: object, test: bool = False) -> str:
    value = _text(value, 240, TEST_PATH if test else PATH)
    if any(part in (".", "..") for part in value.split("/")):
        raise ProtocolError("unsafe_path")
    return value


def _url(value: object, repository: bool = False) -> None:
    value = _text(value, 2048)
    try:
        parsed = urlsplit(value)
        port = parsed.port
        if (parsed.scheme not in (("https",) if repository else ("http", "https"))
                or not parsed.hostname or parsed.username is not None or parsed.password is not None
                or parsed.query or parsed.fragment or any(c.isspace() or ord(c) < 32 for c in value)
                or "@" in unquote(parsed.netloc) or "\\" in value or (port is not None and port == 0)):
            raise ValueError
    except ValueError:
        raise ProtocolError("unsafe_url") from None


def validate_task(raw: bytes, expected_digest: str | None = None) -> tuple[dict, str]:
    request_digest = digest(raw)
    if expected_digest is not None and expected_digest != request_digest:
        raise ProtocolError("request_digest_mismatch")
    task = _object(decode(raw, MAX_TASK_BYTES), TASK_KEYS)
    if task["schema_version"] != "1" or task["phase"] not in ("analyze", "plan", "code"):
        raise ProtocolError("task_version_or_phase")
    if task["harness"] not in ("pydantic_ai", "aider"):
        raise ProtocolError("unknown_harness")
    for key in ("job_id", "run_id"):
        _text(task[key], 256, ID)
    for key in ("profile_digest", "policy_digest"):
        _text(task[key], 71, DIGEST)
    _integer(task["attempt"], 1, 100)
    _text(task["objective"])
    model = _object(task["model"], {"provider", "endpoint", "name"})
    if model["provider"] != "openai_compatible":
        raise ProtocolError("unknown_provider")
    _url(model["endpoint"])
    _text(model["name"], 256, ID)
    limits = _object(task["limits"], set(LIMITS))
    for name, bounds in LIMITS.items():
        _integer(limits[name], *bounds)
    source = _object(task["source"], {"repository_url", "commit", "tree_id", "artifact_ref", "files"})
    _url(source["repository_url"], repository=True)
    _text(source["commit"], 64, COMMIT)
    for key in ("tree_id", "artifact_ref"):
        _text(source[key], 256, ID)
    paths = set()
    for item in _array(source["files"], minimum=1, maximum=64):
        item = _object(item, {"path", "digest", "content"})
        path = _path(item["path"])
        if path in paths:
            raise ProtocolError("duplicate_source_path")
        paths.add(path)
        _text(item["content"], MAX_OUTPUT_BYTES, empty=True)
        _text(item["digest"], 71, DIGEST)
        if item["digest"].lower() != digest(item["content"].encode("utf-8")):
            raise ProtocolError("source_digest_mismatch")
    prior = _object(task["prior"], set(), {
        "analysis_ref", "analysis", "plan_ref", "plan", "previous_candidate_ref", "repair_feedback",
    })
    for key, value in prior.items():
        if key.endswith("_ref"):
            _text(value, 256, ID)
        elif key in ("analysis", "plan"):
            if type(value) is not dict or len(value) > 128 or key + "_ref" not in prior:
                raise ProtocolError("prior_reference_required")
        else:
            _text(value, empty=True)
    if len(prompt_bytes(task)) > limits["max_context_bytes"]:
        raise ProtocolError("context_budget_exceeded")
    return task, request_digest


def prompt_bytes(task: dict) -> bytes:
    """One explicit data envelope; prior reference names never become CLI arguments."""
    return encode({"phase": task["phase"], "objective": task["objective"],
                   "source": task["source"], "prior": task["prior"]})


def validate_output(output: dict, task: dict) -> None:
    phase = task["phase"]
    required = {"summary", "limitations"} | {
        "analyze": {"facts", "gaps"}, "plan": {"scenarios"}, "code": {"files", "suggested_test_ids"},
    }[phase]
    _object(output, required)
    _text(output["summary"])
    _strings(output["limitations"])
    source = {item["path"]: item["content"] for item in task["source"]["files"]}
    if phase == "analyze":
        _strings(output["gaps"])
        for fact in _array(output["facts"]):
            _object(fact, {"claim", "citations"})
            _text(fact["claim"])
            for citation in _array(fact["citations"], minimum=1):
                _object(citation, {"path", "start_line", "end_line"})
                path = _path(citation["path"])
                start = _integer(citation["start_line"], 1, MAX_OUTPUT_BYTES)
                end = _integer(citation["end_line"], 1, MAX_OUTPUT_BYTES)
                if path not in source:
                    raise ProtocolError("invalid_citation")
                content = source[path]
                lines = content.count("\n") + int(bool(content) and not content.endswith("\n"))
                if not start <= end <= lines:
                    raise ProtocolError("invalid_citation")
    elif phase == "plan":
        names = set()
        for scenario in _array(output["scenarios"], minimum=1):
            _object(scenario, {"name", "source_paths", "test_path", "behavior", "oracle"})
            for key in ("name", "behavior", "oracle"):
                _text(scenario[key])
            if scenario["name"] in names:
                raise ProtocolError("duplicate_scenario")
            names.add(scenario["name"])
            _path(scenario["test_path"], test=True)
            paths = _strings(scenario["source_paths"], maximum=240, unique=True)
            if not paths or any(path not in source for path in paths):
                raise ProtocolError("unknown_source_path")
    else:
        _strings(output["suggested_test_ids"], pattern=TEST_ID, maximum=512, unique=True)
        planned = None
        if "plan" in task["prior"]:
            validate_output(task["prior"]["plan"], {**task, "phase": "plan"})
            planned = {scenario["test_path"] for scenario in task["prior"]["plan"]["scenarios"]}
        paths = set()
        for item in _array(output["files"], minimum=1, maximum=32):
            _object(item, {"path", "content"})
            path = _path(item["path"], test=True)
            content = _text(item["content"], 128 * 1024)
            if len(content.encode("utf-8")) > 128 * 1024 or not content.endswith("\n"):
                raise ProtocolError("test_content_bounds")
            if planned is not None and path not in planned:
                raise ProtocolError("unplanned_test_path")
            if path in paths or path in source:
                raise ProtocolError("test_path_collision")
            paths.add(path)


def validate_result(raw: bytes, task: dict, request_digest: str) -> dict:
    result = _object(decode(raw, task["limits"]["max_output_bytes"]), RESULT_KEYS)
    for key in ("schema_version", "job_id", "run_id", "attempt", "phase", "profile_digest", "policy_digest", "harness", "model"):
        if type(result[key]) is not type(task[key]) or result[key] != task[key]:
            raise ProtocolError("result_binding_mismatch")
    if result["request_digest"] != request_digest or result["source_commit"] != task["source"]["commit"]:
        raise ProtocolError("result_binding_mismatch")
    status = result["status"]
    if status not in ("completed", "blocked", "inconclusive", "failed"):
        raise ProtocolError("result_status")
    if status == "completed":
        validate_output(result["output"], task)
    elif result["output"] != {}:
        raise ProtocolError("noncompleted_output")
    usage = _object(result["usage"], USAGE_KEYS)
    for name, count in usage.items():
        if count is not None:
            _integer(count, 0, 2**63 - 1)
    if ((usage["requests"] is not None and usage["requests"] > task["limits"]["max_requests"])
            or (usage["output_tokens"] is not None and usage["output_tokens"] > task["limits"]["max_output_tokens"])):
        raise ProtocolError("usage_budget_exceeded")
    versions = result["versions"]
    if type(versions) is not dict or not 1 <= len(versions) <= 16:
        raise ProtocolError("versions_bounds")
    for name, version in versions.items():
        _text(name, 64)
        _text(version, 128)
    _text(result["diagnostic"], MAX_DIAGNOSTIC_BYTES, empty=True)
    if len(result["diagnostic"].encode("utf-8")) > MAX_DIAGNOSTIC_BYTES:
        raise ProtocolError("diagnostic_budget_exceeded")
    return result
