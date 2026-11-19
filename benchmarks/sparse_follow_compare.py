#!/usr/bin/env python3
"""Measure sparse 128-byte follow latency and 0/1/4-client idle cost serially.

Linux only. Run separately from bulk, traced or profiled measurements. Clients
use default fout (diagnostics only), fout with payload tee, or private reads.
Every probe must reach every client byte-for-byte, once and in order. Retain
per-probe latency CSV and per-process /proc CPU and peak RSS summaries.
"""

import argparse
import contextlib
import csv
import json
import os
from pathlib import Path
import random
import selectors
import shutil
import socket
import statistics
import subprocess
import time

from perf_followup_cases import clean_environment, stop


def process_metrics(pid):
    fields = Path(f"/proc/{pid}/stat").read_text().split()
    cpu = (int(fields[13]) + int(fields[14])) / os.sysconf("SC_CLK_TCK")
    status = dict(line.split(":", 1) for line in Path(f"/proc/{pid}/status").read_text().splitlines())
    return cpu, int(status["VmHWM"].split()[0]), int(status["VmRSS"].split()[0])


def idle_sample(server, clients, seconds):
    before = [process_metrics(p.pid)[0] for p in [server, *clients]]
    start = time.monotonic()
    time.sleep(seconds)
    elapsed = time.monotonic() - start
    after = [process_metrics(p.pid)[0] for p in [server, *clients]]
    delta = [right - left for left, right in zip(before, after)]
    return elapsed, delta[0], sum(delta[1:])


class Receiver:
    def __init__(self, selector):
        self.selector = selector
        self.buffers = {}
        self.lines = {}

    def add(self, process, index):
        self.buffers[index] = b""
        self.lines[index] = []
        self.selector.register(process.stdout, selectors.EVENT_READ, index)

    def read(self, timeout):
        received = []
        for key, _ in self.selector.select(timeout):
            block = os.read(key.fileobj.fileno(), 65536)
            if not block:
                raise RuntimeError(f"client {key.data} ended before completing probes")
            lines = (self.buffers[key.data] + block).split(b"\n")
            self.buffers[key.data] = lines.pop()
            now = time.monotonic()
            for line in lines:
                if line.startswith(b"SYNC "):
                    received.append((key.data, line + b"\n", now))
                else:
                    self.lines[key.data].append(line + b"\n")
                    received.append((key.data, line + b"\n", now))
        return received


def verify_payload(receiver, expected):
    if (not receiver.lines or any(lines != expected for lines in receiver.lines.values())
            or any(receiver.buffers.values())):
        raise RuntimeError("payload equality failed (missing, duplicate, reordered or partial line)")


def verify_tee(paths, expected):
    logged = b"".join(path.read_bytes() for path in sorted(paths))
    # Split only at log-record delimiters and retain the final fragment:
    # synthesizing newlines would accept a truncated file as byte-identical.
    payload = b"\n".join(line for line in logged.split(b"\n")
                         if not line.startswith(b"SYNC "))
    if payload != b"".join(expected):
        raise RuntimeError("file tee equality failed")


def ready(sock_port, process):
    deadline = time.monotonic() + 10
    while process.poll() is None and time.monotonic() < deadline:
        try:
            with socket.create_connection(("127.0.0.1", sock_port), timeout=.1):
                return
        except OSError:
            time.sleep(.05)
    raise RuntimeError("dserver did not become ready")


def one_run(root, key, directory, mode, probe_writer, build, round_number, bulk=None):
    directory.mkdir(parents=True)
    cache = directory / "cache"
    cache.mkdir()
    shutil.copyfile(str(key) + ".pub", cache / (os.environ["USER"] + ".authorized_keys"))
    subprocess.run(["ssh-keygen", "-q", "-t", "rsa", "-b", "2048", "-m", "PEM",
                    "-N", "", "-f", str(cache / "ssh_host_key")], check=True)
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    known = directory / "known_hosts"
    known.write_text(f"[127.0.0.1]:{port} " + " ".join(
        (cache / "ssh_host_key.pub").read_text().split()[:2]) + "\n")
    config = directory / "dtail.json"
    config.write_text(json.dumps({"Client": {"KnownHostsPath": str(known)},
                                  "Server": {"SharedReadsDisable": mode == "private"}}))
    target = directory / "follow.log"
    target.touch()
    environment = clean_environment(key)
    clients = []
    with contextlib.ExitStack() as stack:
        server_log = stack.enter_context((directory / "server.log").open("wb"))
        server = subprocess.Popen([str(root / "dserver"), "--cfg", str(config),
                                   "--logger", "stdout", "--logLevel", "error",
                                   "--bindAddress", "127.0.0.1", "--port", str(port)],
                                  cwd=directory, env=environment, stdout=server_log, stderr=server_log)
        stack.callback(stop, server)
        ready(port, server)
        selector = stack.enter_context(selectors.DefaultSelector())
        receiver = Receiver(selector)
        load = os.getloadavg()[0]
        idle0 = idle_sample(server, clients, 2)
        append = stack.enter_context(target.open("ab", buffering=0))
        for index in range(4):
            error = stack.enter_context((directory / f"client{index}.err").open("wb"))
            command = [str(root / "dtail"), "--cfg", str(config), "--plain", "--noColor",
                       "--logger", "fout", "--logDir", str(directory / f"log{index}"),
                       "--logLevel", "error", "--no-auth-key", "--files", str(target),
                       "--servers", f"127.0.0.1:{port}"]
            if mode == "tee":
                command.append("--log-payload")
            process = subprocess.Popen(command, env=environment, stdin=subprocess.DEVNULL,
                                       stdout=subprocess.PIPE, stderr=error)
            stack.callback(process.stdout.close)
            stack.callback(stop, process)
            clients.append(process)
            receiver.add(process, index)
            deadline = time.monotonic() + 10
            while True:
                append.write(f"SYNC {index}\n".encode())
                if any(i == index and line.startswith(b"SYNC ")
                       for i, line, _ in receiver.read(.2)):
                    break
                if time.monotonic() > deadline:
                    raise TimeoutError(f"client {index} did not become live")
            if index == 0:
                idle1 = idle_sample(server, clients, 2)
        idle4 = idle_sample(server, clients, 2)
        start_cpu = [process_metrics(p.pid)[0] for p in [server, *clients]]
        random_source = random.Random(417)
        expected = []
        latencies = []
        for probe in range(20):
            time.sleep(random_source.uniform(.25, .45))
            line = f"PROBE {probe:06d} ".encode().ljust(127, b"x") + b"\n"
            expected.append(line)
            start = time.monotonic()
            append.write(line)
            seen = set()
            while len(seen) != 4:
                for index, actual, received_at in receiver.read(.5):
                    if actual.startswith(b"SYNC "):
                        continue
                    if actual != line or index in seen:
                        raise RuntimeError(f"duplicate/out-of-order probe: {directory}")
                    seen.add(index)
                    latency = (received_at - start) * 1000
                    latencies.append(latency)
                    probe_writer.writerow([mode, build, round_number, probe, index, latency])
                if time.monotonic() - start > 5:
                    raise TimeoutError(f"probe not delivered: {directory}")
        metrics = [process_metrics(p.pid) for p in [server, *clients]]
        # Optional untimed full-data parity check, separate from probe metrics.
        if bulk is not None:
            payload = bulk.read_bytes()
            if not payload or not payload.endswith(b"\n"):
                raise ValueError("bulk fixture must be nonempty and newline-terminated")
            expected.extend(payload.splitlines(keepends=True))
            append.write(payload)
            deadline = time.monotonic() + 60
            while any(len(lines) < len(expected) for lines in receiver.lines.values()):
                receiver.read(.5)
                if time.monotonic() > deadline:
                    raise TimeoutError(f"bulk payload not delivered: {directory}")
        # Drain a full flush interval to detect any duplicate trailing output.
        deadline = time.monotonic() + .25
        while time.monotonic() < deadline:
            receiver.read(max(0, deadline - time.monotonic()))
        verify_payload(receiver, expected)
        if mode == "tee":
            for index in range(4):
                verify_tee((directory / f"log{index}").glob("*.log"), expected)
        else:
            for path in directory.glob("log*/*.log"):
                if path.stat().st_size:
                    raise RuntimeError(f"default logger teed payload: {path}")
        active = [value[0] - before for value, before in zip(metrics, start_cpu)]
        return [load, *idle0, *idle1, *idle4, active[0], sum(active[1:]),
                metrics[0][1], sum(value[1] for value in metrics[1:]), metrics[0][2],
                statistics.median(latencies), sorted(latencies)[int(.95 * len(latencies)) - 1], max(latencies)]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for option in ("before", "after", "workdir", "key"):
        parser.add_argument("--" + option, type=lambda value: Path(value).resolve(), required=True)
    parser.add_argument("--iterations", type=int, default=6)
    parser.add_argument("--bulk", type=lambda value: Path(value).resolve(),
                        help="append this fixture after probes for untimed full-output parity")
    args = parser.parse_args()
    if args.iterations < 1:
        parser.error("iterations must be positive")
    args.workdir.mkdir(parents=True, exist_ok=False)
    with (args.workdir / "results.csv").open("w") as summary, (args.workdir / "probes.csv").open("w") as probes:
        writer, probe_writer = csv.writer(summary), csv.writer(probes)
        writer.writerow(["mode", "build", "round", "load1", "idle0_s", "idle0_server_cpu_s", "idle0_client_cpu_s",
                         "idle1_s", "idle1_server_cpu_s", "idle1_client_cpu_s", "idle4_s", "idle4_server_cpu_s",
                         "idle4_client_cpu_s", "active_server_cpu_s", "active_client_cpu_s", "server_peak_rss_kib",
                         "clients_peak_rss_sum_kib", "server_rss_kib", "latency_median_ms", "latency_p95_ms", "latency_max_ms"])
        probe_writer.writerow(["mode", "build", "round", "probe", "client", "latency_ms"])
        for round_number in range(1, args.iterations + 1):
            for mode in ("shared", "private", "tee"):
                order = ("before", "after") if round_number % 2 else ("after", "before")
                for build in order:
                    print(f"Running {mode} {build} round {round_number}", flush=True)
                    values = one_run(getattr(args, build), args.key,
                                     args.workdir / f"{mode}-{build}-{round_number}", mode,
                                     probe_writer, build, round_number, args.bulk)
                    writer.writerow([mode, build, round_number, *values])
                    summary.flush()
                    probes.flush()
    print("All sparse follow stdout/file output checks passed.", flush=True)


if __name__ == "__main__":
    main()
