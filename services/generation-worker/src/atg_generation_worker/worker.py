"""One authorized stage attempt; no lifecycle, retry, approval or execution authority."""

from __future__ import annotations

from contextlib import contextmanager, redirect_stderr, redirect_stdout
import copy
import io
import logging
import time

from .harnesses import AiderHarness, HarnessBlocked, PydanticAIHarness, versions
from .protocol import (
    MAX_DIAGNOSTIC_BYTES, ProtocolError, REVIEW_LIMITATION, decode, encode,
    validate_output, validate_result, validate_task,
)


class _Discard(io.TextIOBase):
    def write(self, value: str) -> int:
        return len(value)


@contextmanager
def quiet_sdk():
    """Discard rather than accumulate SDK logs; never replay raw provider errors."""
    previous = logging.root.manager.disable
    try:
        logging.disable(logging.CRITICAL)
        with redirect_stdout(_Discard()), redirect_stderr(_Discard()):
            yield
    finally:
        logging.disable(previous)


def sanitize(value: str, secrets: tuple[str, ...] = ()) -> str:
    for secret in sorted((item for item in secrets if item), key=len, reverse=True):
        value = value.replace(secret, "[REDACTED]")
    value = "".join(character if ord(character) >= 32 and character != "\x7f" else " " for character in value)
    return value.encode("utf-8")[:MAX_DIAGNOSTIC_BYTES].decode("utf-8", errors="ignore")


def _contains_secret(value: object, secret: str) -> bool:
    if isinstance(value, str):
        return secret in value
    if isinstance(value, dict):
        return any(_contains_secret(key, secret) or _contains_secret(child, secret) for key, child in value.items())
    if isinstance(value, list):
        return any(_contains_secret(child, secret) for child in value)
    return False


def run_task(raw: bytes, *, expected_digest: str | None = None, allow_model_calls: bool = False,
             credential: str = "", harness=None) -> dict:
    task, request_digest = validate_task(raw, expected_digest)
    if credential and _contains_secret(task, credential):
        raise ProtocolError("credential_present_in_task")
    # Result identity comes only from the validated, immutable-to-the-adapter task snapshot.
    result = {key: copy.deepcopy(task[key]) for key in (
        "schema_version", "job_id", "run_id", "attempt", "phase", "profile_digest", "policy_digest", "harness", "model",
    )}
    result.update(request_digest=request_digest, source_commit=task["source"]["commit"], status="blocked", output={},
                  usage={"input_tokens": None, "output_tokens": None, "requests": None},
                  versions=versions(task["harness"]), diagnostic="model_calls_disabled")
    if allow_model_calls:
        harness = harness or {"pydantic_ai": PydanticAIHarness, "aider": AiderHarness}[task["harness"]]()
        try:
            if task["phase"] in ("plan", "code") and "analysis" not in task["prior"]:
                raise HarnessBlocked("prior_analysis_required")
            if task["phase"] == "code" and "plan" not in task["prior"]:
                raise HarnessBlocked("prior_plan_required")
            started = time.monotonic()
            with quiet_sdk():
                response = harness.run(copy.deepcopy(task), credential, allow_model_calls=True)
            if time.monotonic() - started > task["limits"]["timeout_seconds"]:
                raise TimeoutError
            if response.status not in ("completed", "blocked", "inconclusive", "failed"):
                raise ProtocolError("result_status")
            result["usage"] = response.usage
            result["status"] = response.status
            result["diagnostic"] = sanitize(response.diagnostic, (credential,))
            if response.status == "completed":
                output = decode(response.output, task["limits"]["max_output_bytes"])
                if credential and _contains_secret(output, credential):
                    raise ProtocolError("credential_present_in_output")
                validate_output(output, task)
                if REVIEW_LIMITATION not in output["limitations"]:
                    output["limitations"].append(REVIEW_LIMITATION)
                result["output"] = output
                result["diagnostic"] = "unreviewed_generation_no_execution_evidence; live_integration_not_verified"
            elif response.output != b"{}":
                raise ProtocolError("noncompleted_output")
            validate_result(encode(result) + b"\n", task, request_digest)
        except HarnessBlocked as error:
            result.update(status="blocked", output={}, diagnostic=sanitize(str(error), (credential,)))
        except TimeoutError:
            result.update(status="inconclusive", output={}, diagnostic="stage_timeout")
        except ProtocolError as error:
            result.update(status="inconclusive", output={}, diagnostic=sanitize(str(error), (credential,)))
        except Exception:
            result.update(status="failed", output={}, diagnostic="harness_failed; live_integration_not_verified")
    # Known secrets must never appear in metadata, usage, diagnostics, or generated files.
    if credential and _contains_secret(result, credential):
        result.update(status="blocked", output={}, diagnostic="credential_present_in_result")
        result["usage"] = {"input_tokens": None, "output_tokens": None, "requests": None}
        if _contains_secret(result, credential):
            raise ProtocolError("credential_present_in_metadata")
    try:
        validate_result(encode(result) + b"\n", task, request_digest)
    except ProtocolError:
        result.update(status="inconclusive", output={}, diagnostic="invalid_or_oversized_result",
                      usage={"input_tokens": None, "output_tokens": None, "requests": None})
        validate_result(encode(result) + b"\n", task, request_digest)
    return result
