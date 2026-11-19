#!/usr/bin/env python3
"""Serial, warm-cache application checks for the 2026-09 performance follow-up.

Run only after the cold-cache/full and shared-read suites have stopped. Every
timed run writes actual output and verifies its SHA-256 against the paired
baseline. Disposable payload files are removed only after verification; compact
CSV metrics, errors, commands and hashes remain. No fsync/durability claim.
"""

import argparse
import contextlib
import csv
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import socket
import subprocess
import time


HERE = Path(__file__).resolve().parent
COMMON = ["--plain", "--noColor", "--logLevel", "error", "--no-auth-key"]


def clean_environment(key):
    environment = {name: value for name, value in os.environ.items()
                   if not name.startswith("DTAIL_") and name != "GODEBUG"}
    environment["DTAIL_AUTH_KEY_PATH"] = str(key)
    return environment


def select_cases(available, name=None, transport=None):
    selected = [case for case in available
                if (not name or case[0] == name) and (not transport or case[3] == transport)]
    if not selected:
        raise ValueError(f"no cases match name={name!r}, transport={transport!r}")
    return selected


def digest(path):
    result = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            result.update(block)
    return result.hexdigest()


def stop(process):
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


@contextlib.contextmanager
def server(root, work, key):
    work.mkdir(parents=True)
    cache = work / "cache"
    cache.mkdir()
    shutil.copyfile(str(key) + ".pub", cache / (os.environ["USER"] + ".authorized_keys"))
    subprocess.run(["ssh-keygen", "-q", "-t", "rsa", "-b", "2048", "-m", "PEM",
                    "-N", "", "-f", str(cache / "ssh_host_key")], check=True)
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    pubkey = (cache / "ssh_host_key.pub").read_text().split()
    known = work / "known_hosts"
    known.write_text(f"[127.0.0.1]:{port} {' '.join(pubkey[:2])}\n")
    config = work / "client.json"
    config.write_text(json.dumps({"Client": {"KnownHostsPath": str(known)}}))
    with (work / "server.log").open("wb") as log:
        process = subprocess.Popen([str(root / "dserver"), "--cfg", "none",
                                    "--logger", "stdout", "--logLevel", "error",
                                    "--bindAddress", "127.0.0.1", "--port", str(port)],
                                   cwd=work, env=clean_environment(key), stdout=log, stderr=log)
        try:
            deadline = time.monotonic() + 10
            while True:
                if process.poll() is not None:
                    raise RuntimeError(f"server exited: {work}")
                try:
                    with socket.create_connection(("127.0.0.1", port), timeout=.1):
                        break
                except OSError:
                    if time.monotonic() > deadline:
                        raise TimeoutError(f"server readiness: {work}")
                    time.sleep(.05)
            yield ["--cfg", str(config), "--servers", f"127.0.0.1:{port}"]
        finally:
            stop(process)


def fixtures(work):
    data = work / "data"
    data.mkdir()
    for density, divisor in (("dense", 2), ("sparse", 1000)):
        with (data / f"tiny-{density}.log").open("wb") as output:
            for index in range(262144):  # 32 MiB, exactly 128 bytes per line.
                prefix = f"{'ERROR' if index % divisor == 0 else 'INFO'} {index:09d} "
                output.write(prefix.encode().ljust(127, b"x") + b"\n")
    for groups in (1000, 10000, 100000):
        with (data / f"groups-{groups}.log").open("w") as output:
            for repeat in range(4):
                for index in range(groups):
                    output.write("INFO|0626-140021|1|stats.go:56|1|4|2|0.01|1h|"
                                 f"MAPREDUCE:STATS|hostname=host{index:06d}|"
                                 f"v={(index * 37 + repeat) % 997 + 1}\n")
    return data


def cases(data, full):
    # tuple: name, executable, options, transport, logger, file-payload, oracle.
    for transport in ("serverless", "server"):
        source = full / "normal_1gib.log"
        yield ("cat-original-1gib", "dcat", ["--files", str(source)],
               transport, "stdout", False, digest(source))
        yield ("grep-original-high", "dgrep",
               ["--files", str(full / "normal_100mib.log"), "--regex", "ERROR"],
               transport, "stdout", False, None)
        for selection, fields, group in (
                ("count", "count($line)", "$hostname"),
                ("aggregate", "count($line),avg($goroutines),max($goroutines),sum($goroutines)",
                 "$goroutines")):
            yield (f"full-map-{selection}", "dmap",
                   ["--files", str(full / "stats_100mib.log"), "--query",
                    f"from STATS select {fields} group by {group}"],
                   transport, "stdout", False, None)
        for density in ("dense", "sparse"):
            source = data / f"tiny-{density}.log"
            yield (f"cat-{density}", "dcat", ["--files", str(source)],
                   transport, "stdout", False, digest(source))
            for context in ("none", "max", "after", "before"):
                args = ["--files", str(source), "--regex", "ERROR"]
                if context != "none":
                    args += ["--" + context, "2147483647" if context == "max" else "5"]
                yield (f"grep-{density}-{context}", "dgrep", args,
                       transport, "stdout", False, None)
        for logger, tee in (("stdout", False), ("fout", False), ("fout", True), ("file", False)):
            # Serverless direct output intentionally bypasses a file-only sink.
            if transport == "serverless" and logger == "file":
                continue
            source = full / "normal_100mib.log"
            yield (f"logger-{logger}-tee{int(tee)}", "dcat", ["--files", str(source)],
                   transport, logger, tee, digest(source))
    for groups in (1000, 10000, 100000):
        for selection in ("count", "mixed"):
            fields = "hostname,count($line)"
            if selection == "mixed":
                fields += ",sum(v),percentage(v),percentile(v)"
            for limit in (10, groups):
                query = (f"from STATS select {fields} group by hostname "
                         f"order by count($line) limit {limit}")
                yield (f"map-{groups}-{selection}-limit{limit}", "dmap",
                       ["--files", str(data / f"groups-{groups}.log"), "--query", query],
                       "serverless", "stdout", False, None)
                if groups == 100000 and selection == "mixed" and limit == groups:
                    yield (f"map-{groups}-{selection}-csv", "dmap",
                           ["--files", str(data / f"groups-{groups}.log"), "--query",
                            query + " outfile " + str(data.parent / "result.csv")],
                           "serverless", "stdout", False, None)


def capture(command, directory, environment):
    directory.mkdir(parents=True)
    output, error = directory / "stdout", directory / "stderr"
    started_load = os.getloadavg()[0]
    measured = subprocess.run(
        ["python3", str(HERE / "measure_command.py"), "--stdout", str(output),
         "--stderr", str(error), "--", "timeout", "--kill-after=5", "180", *command],
        env=environment, stdout=subprocess.PIPE, text=True, check=True)
    elapsed, user, system, rss, status = measured.stdout.strip().split("\t")
    if status != "0":
        raise RuntimeError(f"command failed rc={status}; see {error}")
    return output, [elapsed, user, system, rss, started_load]


def run(args):
    args.workdir.mkdir(parents=True, exist_ok=False)
    data = fixtures(args.workdir)
    selected = select_cases(cases(data, args.data_dir), args.case, args.transport)
    environment = clean_environment(args.key)
    roots = {"before": args.before, "after": args.after}
    with contextlib.ExitStack() as stack:
        connections = {name: stack.enter_context(server(root, args.workdir / name, args.key))
                       for name, root in roots.items()}
        results = stack.enter_context((args.workdir / "results.csv").open("w"))
        commands = stack.enter_context((args.workdir / "commands.jsonl").open("w"))
        writer = csv.writer(results)
        writer.writerow(["case", "transport", "build", "round", "elapsed_s", "user_s",
                         "sys_s", "max_rss_kib", "load1", "stdout_sha256", "file_sha256",
                         "csv_sha256"])
        for name, executable, options, transport, logger, tee, oracle in selected:
            print(f"Running {transport} {name}", flush=True)
            reference = None
            # Round zero warms both revisions and establishes parity, untimed
            # for reporting purposes. Subsequent rounds alternate build order.
            for round_number in range(args.iterations + 1):
                order = ("before", "after") if round_number % 2 == 0 else ("after", "before")
                for build in order:
                    directory = args.workdir / "runs" / f"{transport}-{name}-{round_number}-{build}"
                    command = [str(roots[build] / executable), *COMMON, "--logger", logger,
                               "--logDir", str(directory / "log"), *options]
                    command += connections[build] if transport == "server" else ["--cfg", "none"]
                    if tee:
                        command.append("--log-payload")
                    csv_output = args.workdir / "result.csv" if name.endswith("-csv") else None
                    if csv_output and csv_output.exists():
                        csv_output.unlink()
                    commands.write(json.dumps(command) + "\n")
                    output, metrics = capture(command, directory, environment)
                    stdout_hash = digest(output)
                    daily = sorted((directory / "log").glob("*.log"))
                    file_hash = digest(daily[0]) if len(daily) == 1 else ""
                    if len(daily) > 1:
                        raise RuntimeError("measurement crossed daily rotation; rerun in a fresh directory")
                    if oracle:
                        payload_hash = file_hash if logger == "file" else stdout_hash
                        if payload_hash != oracle:
                            raise RuntimeError(f"payload differs from input: {directory}")
                        if tee:
                            if file_hash != oracle:
                                raise RuntimeError(f"file tee differs from input: {directory}")
                        elif logger == "fout":
                            if daily and daily[0].stat().st_size != 0:
                                raise RuntimeError(f"default fout logged payload: {directory}")
                    if executable == "dmap" and name.startswith("map-"):
                        if csv_output:
                            with csv_output.open() as rows:
                                row_count = sum(1 for _ in csv.reader(rows)) - 1
                            expected_count = 100000
                        else:
                            row_count = sum(bool(re.match(rb"\s*host\d+\s*\|", row))
                                            for row in output.read_bytes().splitlines())
                            expected_count = int(name.rsplit("limit", 1)[1])
                        if row_count != expected_count:
                            raise RuntimeError(f"wrong group count {row_count}, expected {expected_count}: {directory}")
                    csv_hash = digest(csv_output) if csv_output else ""
                    hashes = stdout_hash, file_hash, csv_hash
                    if reference is None:
                        reference = hashes
                    if hashes != reference:
                        raise RuntimeError(f"paired output mismatch: {directory}")
                    if round_number:
                        writer.writerow([name, transport, build, round_number, *metrics, *hashes])
                        results.flush()
                    # Only disposable generated payloads, never source files.
                    output.unlink()
                    for path in daily:
                        path.unlink()
                    if csv_output:
                        csv_output.unlink()
    print("All paired application output checks passed.", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for option in ("before", "after", "workdir", "data-dir", "key"):
        parser.add_argument("--" + option, type=lambda value: Path(value).resolve(), required=True)
    parser.add_argument("--iterations", type=int, default=6)
    parser.add_argument("--case", help="run only this exact case name")
    parser.add_argument("--transport", choices=("server", "serverless"))
    args = parser.parse_args()
    if args.iterations < 1:
        parser.error("iterations must be positive")
    run(args)


if __name__ == "__main__":
    main()
