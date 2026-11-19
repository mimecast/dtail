#!/usr/bin/env python3
"""Summarize paired upstream-versus-local DTail benchmark results."""

from __future__ import annotations

import argparse
import csv
import statistics
from collections import defaultdict
from pathlib import Path


IMPLEMENTATIONS = ("upstream", "local")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--input", required=True, type=Path)
    parser.add_argument("--csv", required=True, type=Path)
    parser.add_argument("--markdown", required=True, type=Path)
    return parser.parse_args()


def optional_float(value: str) -> float | None:
    return float(value) if value else None


def median(values: list[float]) -> float | None:
    return statistics.median(values) if values else None


def format_number(value: float | None, digits: int = 3) -> str:
    return "" if value is None else f"{value:.{digits}f}"


def main() -> int:
    args = parse_args()
    observations: dict[tuple[str, str], list[dict[str, str]]] = defaultdict(list)
    failures: list[dict[str, str]] = []
    with args.input.open(newline="") as source:
        for row in csv.DictReader(source):
            if row["status"] != "ok":
                failures.append(row)
                continue
            observations[(row["scenario"], row["implementation"])].append(row)

    scenarios = sorted(
        {scenario for scenario, _ in observations}
        | {row["scenario"] for row in failures}
    )
    summaries: list[dict[str, str]] = []
    for scenario in scenarios:
        by_implementation: dict[str, dict[str, float | int | None]] = {}
        for implementation in IMPLEMENTATIONS:
            rows = observations.get((scenario, implementation), [])
            elapsed_values = [float(row["elapsed_seconds"]) for row in rows]
            elapsed = median(elapsed_values)
            input_bytes = max((int(row["input_bytes"]) for row in rows), default=0)

            cpu_values: list[float] = []
            client_rss_values: list[float] = []
            server_rss_values: list[float] = []
            for row in rows:
                user = optional_float(row["client_user_seconds"])
                system = optional_float(row["client_system_seconds"])
                server = optional_float(row["server_cpu_seconds"])
                if user is not None and system is not None:
                    cpu_values.append(user + system + (server or 0.0))
                client_rss = optional_float(row["client_max_rss_kib"])
                if client_rss is not None:
                    client_rss_values.append(client_rss)
                server_rss = optional_float(row["server_rss_kib"])
                if server_rss is not None:
                    server_rss_values.append(server_rss)

            throughput = None
            if elapsed and input_bytes:
                throughput = input_bytes / (1024 * 1024) / elapsed
            by_implementation[implementation] = {
                "count": len(rows),
                "elapsed": elapsed,
                "throughput": throughput,
                "cpu": median(cpu_values),
                "client_rss": median(client_rss_values),
                "server_rss": median(server_rss_values),
            }

        upstream = by_implementation["upstream"]
        local = by_implementation["local"]
        upstream_elapsed = upstream["elapsed"]
        local_elapsed = local["elapsed"]
        speedup = None
        change_percent = None
        if isinstance(upstream_elapsed, float) and isinstance(local_elapsed, float) and local_elapsed:
            speedup = upstream_elapsed / local_elapsed
            change_percent = (speedup - 1.0) * 100.0

        summaries.append(
            {
                "scenario": scenario,
                "samples_per_implementation": str(
                    min(int(upstream["count"]), int(local["count"]))
                ),
                "failed_observations": str(
                    sum(1 for row in failures if row["scenario"] == scenario)
                ),
                "upstream_median_seconds": format_number(upstream_elapsed, 6),
                "local_median_seconds": format_number(local_elapsed, 6),
                "local_speedup_x": format_number(speedup, 3),
                "local_change_percent": format_number(change_percent, 2),
                "upstream_mib_per_second": format_number(upstream["throughput"]),
                "local_mib_per_second": format_number(local["throughput"]),
                "upstream_median_total_cpu_seconds": format_number(upstream["cpu"], 6),
                "local_median_total_cpu_seconds": format_number(local["cpu"], 6),
                "upstream_median_client_rss_kib": format_number(upstream["client_rss"], 0),
                "local_median_client_rss_kib": format_number(local["client_rss"], 0),
                "upstream_median_server_rss_kib": format_number(upstream["server_rss"], 0),
                "local_median_server_rss_kib": format_number(local["server_rss"], 0),
            }
        )

    args.csv.parent.mkdir(parents=True, exist_ok=True)
    fieldnames = list(summaries[0]) if summaries else ["scenario"]
    with args.csv.open("w", newline="") as destination:
        writer = csv.DictWriter(destination, fieldnames=fieldnames)
        writer.writeheader()
        writer.writerows(summaries)

    with args.markdown.open("w") as destination:
        destination.write("# Upstream versus local DTail benchmark\n\n")
        destination.write(
            "Positive change means the local fork completed faster. CPU includes "
            "the client and, for server-mode scenarios, the dserver CPU delta.\n\n"
        )
        destination.write(
            "| Scenario | Samples | Failures | Upstream median | Local median | Local change | "
            "Speedup | Upstream MiB/s | Local MiB/s |\n"
        )
        destination.write(
            "|---|---:|---:|---:|---:|---:|---:|---:|---:|\n"
        )
        for row in summaries:
            change = row["local_change_percent"]
            destination.write(
                f"| {row['scenario']} | {row['samples_per_implementation']} | "
                f"{row['failed_observations']} | "
                f"{row['upstream_median_seconds']} s | {row['local_median_seconds']} s | "
                f"{change}% | {row['local_speedup_x']}x | "
                f"{row['upstream_mib_per_second']} | {row['local_mib_per_second']} |\n"
            )
        if failures:
            destination.write(f"\nFailed observations: {len(failures)}. See the raw CSV.\n")

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
