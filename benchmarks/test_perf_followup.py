"""Small safety/oracle tests; application parity is checked by the real runs."""

import os
from pathlib import Path
import selectors
import shutil
import subprocess
import sys
import tempfile
import textwrap
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from perf_followup_cases import clean_environment, digest, select_cases, stop
from profile_followup_cases import CASES, selected_commands
from sparse_follow_compare import Receiver, ready, verify_payload, verify_tee


class HarnessTests(unittest.TestCase):
    def test_grep_confirmation_rejects_missing_arguments_and_invalid_rounds(self):
        script = Path(__file__).with_name("grep_pair_confirm.sh")
        for arguments, message in (([], "need before"),
                                   (["unused"] * 4 + ["ERROR", "0"], "rounds must be positive")):
            result = subprocess.run(["bash", str(script), *arguments], text=True,
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(message, result.stderr)

    def test_profile_selection_requires_complete_unique_serverless_commands(self):
        commands = [["dgrep", "--logDir", f"/tmp/serverless-{case}-0-{build}/log"]
                    for case in CASES for build in ("before", "after")]
        self.assertEqual(len(selected_commands(commands)), 12)
        for invalid in (commands[:-1], commands + commands[:1],
                        [commands[0] + ["--servers", "localhost"], *commands[1:]]):
            with self.assertRaises(ValueError):
                selected_commands(invalid)

    def test_case_selection_rejects_empty_and_unknown(self):
        cases = [("cat", "dcat", [], "server"), ("cat", "dcat", [], "serverless")]
        self.assertEqual(select_cases(cases, "cat", "server"), cases[:1])
        self.assertEqual(select_cases(cases), cases)
        for name, transport in (("typo", None), ("cat", "typo")):
            with self.assertRaisesRegex(ValueError, "no cases match"):
                select_cases(cases, name, transport)
        with self.assertRaises(ValueError):
            select_cases([])

    def test_environment_removes_configuration_and_profiling_overrides(self):
        with patch.dict(os.environ, {"PATH": "/bin", "DTAIL_PORT": "1",
                                    "DTAIL_AUTH_KEY_PATH": "wrong", "GODEBUG": "gctrace=1",
                                    "GOMAXPROCS": "4"}, clear=True):
            self.assertEqual(clean_environment(Path("/key")),
                             {"PATH": "/bin", "GOMAXPROCS": "4", "DTAIL_AUTH_KEY_PATH": "/key"})

    def test_digest_real_file_and_missing_file(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "payload"
            path.write_bytes(b"abc")
            self.assertEqual(digest(path),
                             "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
            with self.assertRaises(FileNotFoundError):
                digest(path.with_name("missing"))

    def test_stop_reaps_real_child_and_is_idempotent(self):
        process = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"])
        self.addCleanup(stop, process)
        stop(process)
        self.assertIsNotNone(process.returncode)
        stop(process)

    def test_ready_rejects_exited_server(self):
        process = subprocess.Popen([sys.executable, "-c", "raise SystemExit(7)"])
        process.wait(timeout=5)
        with self.assertRaisesRegex(RuntimeError, "did not become ready"):
            ready(1, process)

    def test_tee_checks_real_file_without_repairing_missing_newline(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "20260923.log"
            expected = [b"one\n", b"two\n"]
            path.write_bytes(b"SYNC 0\none\ntwo\n")
            verify_tee([path], expected)
            for invalid in (b"one\ntwo", b"one\n", b"two\none\n",
                            b"one\ntwo\none\n", b"one\ntwo\npartial"):
                path.write_bytes(invalid)
                with self.assertRaisesRegex(RuntimeError, "file tee equality"):
                    verify_tee([path], expected)
            with self.assertRaises(RuntimeError):
                verify_tee([], expected)


class ReceiverTests(unittest.TestCase):
    def setUp(self):
        self.selector = selectors.DefaultSelector()
        self.addCleanup(self.selector.close)
        reader, writer = os.pipe()
        self.reader = os.fdopen(reader, "rb", buffering=0)
        self.writer = os.fdopen(writer, "wb", buffering=0)
        self.addCleanup(self.reader.close)
        self.addCleanup(self.writer.close)
        self.receiver = Receiver(self.selector)
        self.receiver.add(SimpleNamespace(stdout=self.reader), 0)

    def test_fragmented_real_pipe_and_sync_exclusion(self):
        self.writer.write(b"SYNC 0\nPRO")
        self.assertEqual([(i, line) for i, line, _ in self.receiver.read(1)], [(0, b"SYNC 0\n")])
        self.writer.write(b"BE 1\nPROBE 2\n")
        self.assertEqual([line for _, line, _ in self.receiver.read(1)], [b"PROBE 1\n", b"PROBE 2\n"])
        verify_payload(self.receiver, [b"PROBE 1\n", b"PROBE 2\n"])

    def test_eof_is_failure(self):
        self.writer.close()
        with self.assertRaisesRegex(RuntimeError, "ended before"):
            self.receiver.read(1)

    def test_oracle_rejects_missing_duplicate_reordered_and_partial(self):
        expected = [b"one\n", b"two\n"]
        for actual in (expected[:1], expected + expected[:1], list(reversed(expected))):
            self.receiver.lines[0] = actual
            with self.assertRaisesRegex(RuntimeError, "payload equality"):
                verify_payload(self.receiver, expected)
        self.receiver.lines[0] = expected
        self.receiver.buffers[0] = b"partial"
        with self.assertRaises(RuntimeError):
            verify_payload(self.receiver, expected)
        with self.assertRaises(RuntimeError):
            verify_payload(Receiver(self.selector), expected)


class SharedWrapperTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        shutil.copyfile(Path(__file__).with_name("shared_read_compare.sh"),
                        self.root / "shared_read_compare.sh")
        # Exercise the actual wrapper's order/count/parity logic with a tiny
        # fixture producer instead of repeating expensive application loads.
        (self.root / "shared_read_bench.sh").write_text(textwrap.dedent('''\
            #!/usr/bin/env bash
            set -euo pipefail
            while getopts 'd:i:r:n:q:w:o:s' opt; do
                case "$opt" in
                    d) root=$OPTARG;; i) run=$OPTARG;; w) work=$OPTARG;; o) csv=$OPTARG;;
                esac
            done
            shift $((OPTIND - 1))
            if [[ ${TEST_FAIL:-} == binary && $root == */final ]]; then
                printf 'changed\\n' >> "$root/dserver"
            fi
            touch "$csv"
            [[ ${TEST_FAIL:-} != empty ]] || exit 0
            kind=plain
            [[ $1 != scheduled-gz ]] || kind=gz
            for mode in on off; do
                printf '%s,%s,%s,%s,4,1,1,n/a,n/a,0,1,0,yes\\n' \\
                    "$1" "$kind" "$mode" "$run" >> "$csv"
                dir="$work/runs/scheduled-$kind-$mode-$run"
                mkdir -p "$dir"
                for job in 0 1 2 3; do
                    printf 'job%s\\n' "$job" > "$dir/job$job.csv"
                    if [[ ${TEST_FAIL:-} == mismatch && $root == */final ]]; then
                        printf 'wrong\\n' >> "$dir/job$job.csv"
                    fi
                done
            done
            '''))
        for name in ("preplan", "baseline", "final"):
            (self.root / name).mkdir()
            for binary in ("dtail", "dserver"):
                path = self.root / name / binary
                path.touch(mode=0o755)

    def run_wrapper(self, failure="", rounds="2"):
        environment = dict(os.environ, TEST_FAIL=failure)
        return subprocess.run(["bash", str(self.root / "shared_read_compare.sh"),
                               str(self.root / "results"), str(self.root / "preplan"),
                               str(self.root / "baseline"), str(self.root / "final"), rounds],
                              env=environment, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                              text=True, timeout=15)

    def test_serial_order_and_output_counts(self):
        result = self.run_wrapper()
        self.assertEqual(result.returncode, 0, result.stderr)
        order = (self.root / "results/order.csv").read_text().splitlines()
        self.assertEqual(len(order), 25)
        self.assertEqual(order[1:4], ["1,follow,preplan", "1,follow,latest-baseline", "1,follow,final"])
        self.assertEqual(order[13:16], ["2,follow,latest-baseline", "2,follow,preplan", "2,follow,final"])
        for name in ("preplan", "latest-baseline", "final"):
            self.assertEqual(len((self.root / f"results/{name}/results.csv").read_text().splitlines()), 16)
        self.assertNotEqual(self.run_wrapper().returncode, 0)  # never overwrite evidence

    def test_empty_results_are_not_success(self):
        result = self.run_wrapper("empty")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("output mismatch", result.stderr)

    def test_existing_directory_without_marker_is_preserved(self):
        directory = self.root / "results"
        directory.mkdir()
        sentinel = directory / "metadata.txt"
        sentinel.write_text("existing evidence\n")
        result = self.run_wrapper()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("fresh comparison directory", result.stderr)
        self.assertEqual(sentinel.read_text(), "existing evidence\n")
        self.assertEqual(list(directory.iterdir()), [sentinel])

    def test_changed_binary_is_failure(self):
        result = self.run_wrapper("binary", "1")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("binaries changed", result.stderr)

    def test_dangling_directory_symlink_is_rejected(self):
        link = self.root / "results"
        link.symlink_to(self.root / "missing")
        result = self.run_wrapper()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("fresh comparison directory", result.stderr)
        self.assertTrue(link.is_symlink())
        self.assertFalse((self.root / "missing").exists())

    def test_cross_build_mismatch_is_failure(self):
        result = self.run_wrapper("mismatch", "1")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("cross-build output mismatch", result.stderr)

    def test_invalid_round_count_is_failure(self):
        result = self.run_wrapper(rounds="0")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("rounds must be positive", result.stderr)


if __name__ == "__main__":
    unittest.main()
