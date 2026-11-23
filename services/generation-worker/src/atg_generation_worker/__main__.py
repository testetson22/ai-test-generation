"""Pure-JSON stdout CLI. Invalid task/input errors are fixed, bounded stderr codes."""

from __future__ import annotations

import argparse
import os
from pathlib import Path
import stat
import sys
import tempfile

from .harnesses import probe
from .protocol import MAX_TASK_BYTES, ProtocolError, decode, encode, validate_task
from .worker import run_task


class _Parser(argparse.ArgumentParser):
    def error(self, message: str) -> None:
        self.exit(2, "invalid_input: invalid_arguments\n")


def credential_from_file(path: Path) -> str:
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_uid != os.geteuid():
            raise ProtocolError("unsafe_credential_file")
        raw = stream.read(8193)
    try:
        key = raw.decode("utf-8").removesuffix("\n")
    except UnicodeError:
        raise ProtocolError("invalid_credential_file") from None
    if not key or len(raw) > 8192 or len(key) > 4096 or any(ord(character) < 33 or ord(character) == 127 for character in key):
        raise ProtocolError("invalid_credential_file")
    return key


def atomic_write(path: Path, raw: bytes) -> None:
    # The output parent is supplied by the trusted wrapper, never by model output.
    descriptor, temporary = tempfile.mkstemp(prefix=".atg-result-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(raw)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        parent = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(parent)
        finally:
            os.close(parent)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def main(argv: list[str] | None = None) -> int:
    parser = _Parser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("probe")
    run = commands.add_parser("run")
    run.add_argument("--task-file", "--taskfile", type=Path)
    run.add_argument("--output-file", "--outputfile", type=Path)
    run.add_argument("--request-digest")
    run.add_argument("--credential-file", type=Path)
    run.add_argument("--allow-model-calls", action="store_true")
    args = parser.parse_args(argv)
    try:
        if args.command == "probe":
            sys.stdout.buffer.write(encode(probe()) + b"\n")
            return 0
        if args.task_file:
            with args.task_file.open("rb") as stream:
                raw = stream.read(MAX_TASK_BYTES + 1)
        else:
            raw = sys.stdin.buffer.read(MAX_TASK_BYTES + 1)
        validate_task(raw, args.request_digest)
        if args.output_file and any(path and args.output_file.resolve() == path.resolve()
                                    for path in (args.task_file, args.credential_file)):
            raise ProtocolError("output_input_collision")
        credential = ""
        if args.allow_model_calls:
            credential = credential_from_file(args.credential_file) if args.credential_file else os.environ.get("OPENAI_API_KEY", "")
        result = run_task(raw, expected_digest=args.request_digest, credential=credential,
                          allow_model_calls=args.allow_model_calls)
        output = encode(result) + b"\n"
        if len(output) > decode(raw, MAX_TASK_BYTES)["limits"]["max_output_bytes"]:
            raise ProtocolError("result_size")
        if args.output_file:
            atomic_write(args.output_file, output)
        else:
            sys.stdout.buffer.write(output)
        return 0
    except ProtocolError as error:
        sys.stderr.write("invalid_input: " + str(error) + "\n")
        return 2
    except Exception:
        sys.stderr.write("worker_io_or_configuration_failed\n")
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
