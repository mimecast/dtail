# Upstream versus local benchmark plan

This comparison measures the current DTail fork against the upstream
`github.com/mimecast/dtail` implementation. The initial upstream checkout is:

- repository: `https://github.com/mimecast/dtail.git`
- directory: `../dtail-upstream`
- branch: `master`
- commit: `91d35001488dd036b1f1def30cc435d3ab25c5f1`

To recreate the sibling checkout at that revision:

```bash
git clone https://github.com/mimecast/dtail.git ../dtail-upstream
git -C ../dtail-upstream checkout 91d35001488dd036b1f1def30cc435d3ab25c5f1
```

The harness records both commit IDs, remotes, dirty state, Go versions, kernel,
CPU governor, binary hashes, and data hashes in `metadata.txt`. A later upstream
revision can be selected by checking it out in that sibling repository; every
result remains tied to the exact recorded commit.

## Preparation and validation

Run this while other work is still using the machine:

```bash
make benchmark-upstream-smoke
```

Smoke mode performs only setup and compatibility checks. It builds both trees
with the same `go` binary and these flags:

```text
-p=1 -trimpath -buildvcs=false
```

It then generates small deterministic inputs and checks the following in both
serverless mode and matched server mode (upstream client with upstream dserver,
local client with local dserver):

- `dcat` output is byte-identical to the input and between implementations.
- low-hit and high-hit `dgrep` output is byte-identical to `grep` and between
  implementations.
- `dmap` count and aggregate results contain every input record and are
  identical after normalizing unordered group rows.
- `dtail` follow delivers every line before its end marker.

Both sides receive the same command flags, input paths, RSA key size, SSH bind
address, logger, and log level. The common legacy
`DTAIL_SSH_PRIVATE_KEYFILE_PATH` variable selects the key because both versions
support it. The harness explicitly clears integration-test mode and the retired
turbo environment variables. It uses each implementation's normal default
runtime behavior, including the local fork's auth-key reconnect support.

The aggregate data deliberately has eight group values. DTail limits displayed
groups to ten and map iteration order is unspecified; the historical workload
could exceed that cap and make the two processes display different random
subsets. Eight groups exercise the same aggregation path while allowing a
semantic equality check.

The follow command uses the common `--max 2147483647` flag. Upstream otherwise
sets its tail reader to drop lines whenever its 100-entry output channel is
full, which lets a faster-looking upstream run do less work. A maximum that the
test cannot reach selects upstream's lossless context-aware path without
changing the records returned. The local fork receives the identical flag.

Smoke mode does not generate full data, write `results.csv`, or report timing.
Its temporary work directory is printed and retained for inspection.

## Full comparison

Wait until the host is idle and its power/thermal state is stable. Then run:

```bash
./benchmarks/upstream_vs_local_bench.sh run \
  --workdir /tmp/dtail-upstream-vs-local
```

The explicit `run` mode prevents an accidental benchmark during ordinary
validation. It refuses to run if either checkout has tracked changes. Untracked
files are recorded in the metadata but do not block the run.

The suite first repeats all smoke checks, then generates shared deterministic
data and runs the same end-to-end workload classes as the historical
turbo-versus-normal harness:

| Scenario | Transport | Input | Default observations |
|---|---|---:|---:|
| dcat | serverless | 100 MiB | 7 |
| dcat | serverless | 1 GiB | 3 |
| dgrep, 0.1% hit rate | serverless | 100 MiB | 7 |
| dgrep, 10% hit rate | serverless | 100 MiB | 7 |
| dmap aggregate | serverless | 100 MiB | 5 |
| dcat | matched SSH server | 100 MiB | 7 |
| dgrep, 0.1% hit rate | matched SSH server | 100 MiB | 7 |
| dgrep, 10% hit rate | matched SSH server | 100 MiB | 7 |
| dmap count | matched SSH server | 100 MiB | 5 |
| dmap aggregate | matched SSH server | 100 MiB | 5 |
| dtail follow burst | matched SSH server | 10 MiB | 3 |

Each implementation gets one warmup per scenario. Measured order alternates
upstream/local and local/upstream on successive observations to reduce bias from
temperature, cache state, and background drift. Output is sent to `/dev/null`
during measured bulk reads so results do not consume another gigabyte per
observation; semantic output has already passed the smoke gate.

Pass `--iterations N` to give every scenario the same observation count. The
default counts preserve the historical suite's balance between large and small
workloads.

## Metrics and decision rule

`results.csv` contains every observation. The primary metric is elapsed time;
throughput is input MiB divided by elapsed seconds. The harness also records:

- client user and system CPU time;
- client peak RSS;
- dserver CPU consumed during each server-mode observation;
- dserver RSS after each server-mode observation;
- delivered record counts for follow workloads.

`summary.csv` and `report.md` report medians. Local speedup is calculated as:

```text
upstream median seconds / local median seconds
```

A value above `1.0x`, or a positive local percentage, means the fork is faster.
Elapsed-time gains should be accepted only when repeated observations agree and
CPU use does not regress enough to change the operational conclusion. RSS is a
separate resource result; it should not be folded into one composite score.

If a scenario fails or follow drops records, treat its performance result as
invalid and investigate the raw stderr/server logs before comparing speed.

## Repetition

One full run provides paired observations, but machine-level noise can still
affect every sample. For a result used in a release or optimization decision,
run the suite in at least three separate idle periods with fresh work
directories. Compare per-run medians and keep the raw CSV, summary, report, and
metadata together. Do not pool observations from runs made under different CPU
governors, Go toolchains, or commits.
