#!/usr/bin/env python3
"""Measure one command without depending on an external GNU time binary."""

from __future__ import annotations

import argparse
import resource
import subprocess
import sys
import time
from pathlib import Path


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--stdout", required=True, type=Path)
    parser.add_argument("--stderr", required=True, type=Path)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if args.command[:1] == ["--"]:
        args.command = args.command[1:]
    if not args.command:
        parser.error("a command is required after --")
    return args


def main() -> int:
    args = parse_args()
    args.stdout.parent.mkdir(parents=True, exist_ok=True)
    args.stderr.parent.mkdir(parents=True, exist_ok=True)

    before = resource.getrusage(resource.RUSAGE_CHILDREN)
    started = time.perf_counter_ns()
    try:
        with args.stdout.open("wb") as stdout, args.stderr.open("ab") as stderr:
            result = subprocess.run(
                args.command,
                stdin=subprocess.DEVNULL,
                stdout=stdout,
                stderr=stderr,
                check=False,
            )
        returncode = result.returncode
    except OSError as error:
        with args.stderr.open("ab") as stderr:
            stderr.write(f"unable to execute command: {error}\n".encode())
        returncode = 127
    finished = time.perf_counter_ns()
    after = resource.getrusage(resource.RUSAGE_CHILDREN)

    elapsed = (finished - started) / 1_000_000_000
    user = after.ru_utime - before.ru_utime
    system = after.ru_stime - before.ru_stime
    # Linux reports ru_maxrss in KiB. The comparison harness is Linux-only
    # because dserver and its profiling workflow are exercised on Linux.
    max_rss_kib = after.ru_maxrss
    print(
        f"{elapsed:.9f}\t{user:.9f}\t{system:.9f}\t"
        f"{max_rss_kib}\t{returncode}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
