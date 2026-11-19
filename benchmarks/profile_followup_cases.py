#!/usr/bin/env python3
"""Replay selected serverless commands from perf_followup_cases.py with profiles.

Use only a trusted, locally generated commands.jsonl. Run after all timing
measurements have stopped. Profiles and profiling-process RSS are diagnostics,
not timing observations; stdout is discarded and file teeing still uses a real
file. Short CPU captures may have too few samples for attribution.
"""

import argparse
import json
from pathlib import Path
import subprocess

from perf_followup_cases import clean_environment


CASES = ("grep-sparse-none", "grep-sparse-max", "grep-sparse-before",
         "logger-fout-tee1", "map-100000-count-limit10", "map-100000-mixed-limit100000")


def selected_commands(commands):
    selected = {}
    wanted = {f"serverless-{case}-0-{build}" for case in CASES for build in ("before", "after")}
    for command in commands:
        name = Path(command[command.index("--logDir") + 1]).parent.name
        if name not in wanted:
            continue
        if "--servers" in command or name in selected:
            raise ValueError(f"invalid or repeated profile command: {name}")
        selected[name] = command
    if set(selected) != wanted:
        raise ValueError(f"missing profile commands: {sorted(wanted - set(selected))}")
    return selected


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--commands", type=Path, required=True)
    parser.add_argument("--workdir", type=Path, required=True)
    args = parser.parse_args()
    with args.commands.open() as source:
        selected = selected_commands(json.loads(line) for line in source)
    args.workdir = args.workdir.resolve()
    args.workdir.mkdir(parents=True, exist_ok=False)
    for name, command in selected.items():
        directory = args.workdir / name
        directory.mkdir()
        command = list(command)
        command[command.index("--logDir") + 1] = str(directory / "log")
        command += ["--profile", "--profiledir", str(directory)]
        (directory / "command.json").write_text(json.dumps(command) + "\n")
        print(f"Profiling {name}", flush=True)
        with (directory / "process-metrics.tsv").open("w") as metrics:
            subprocess.run(["python3", str(Path(__file__).with_name("measure_command.py")),
                            "--stdout", "/dev/null", "--stderr", str(directory / "metrics.log"),
                            "--", "timeout", "--kill-after=5", "180", *command],
                           env=clean_environment(Path("/unused-serverless-key")),
                           stdout=metrics, check=True)
        status = (directory / "process-metrics.tsv").read_text().strip().split("\t")[-1]
        if status != "0":
            raise RuntimeError(f"profile command failed rc={status}: {directory}")
        profiles = sorted(directory.glob("*.prof"))
        if len(profiles) != 3:
            raise RuntimeError(f"expected CPU, heap and allocation profiles: {directory}")
        for profile in profiles:
            with profile.with_suffix(".txt").open("w") as output:
                subprocess.run(["go", "tool", "pprof", "-top", "-nodecount=15",
                                command[0], str(profile)], stdout=output, stderr=subprocess.STDOUT,
                               check=True)
    print("All profile replays completed.", flush=True)


if __name__ == "__main__":
    main()
