---
name: dtail-client
description: Use DTail client commands to read, search, follow, and aggregate local or distributed logs, and check dserver health. Apply when an agent needs to run or explain dcat, dgrep, dtail, dmap, or dtailhealth; not for administering dserver.
---

# DTail client

Use DTail to answer log questions across hosts or on the local machine. Choose the command by the desired result:

| Need | Command |
| --- | --- |
| Read complete files or snapshots | `dcat` |
| Search existing files with a Go regular expression | `dgrep` |
| Follow new lines, optionally filtering them | `dtail` |
| Aggregate existing logs or CSV data | `dmap` |
| Aggregate new lines continuously | `dtail --query` |
| Check a dserver endpoint | `dtailhealth` |

## Working procedure

1. Identify the requested hosts, files or journal unit, time scope, and output form. A missing `--servers` selects **local serverless mode**, so do not omit it when the user intended remote hosts. If a remote target is unspecified, obtain it from the user's context or ask for it.
2. For a finite answer, prefer `dgrep`, `dcat`, or `dmap`. For a live request, use `dtail` and set a suitable end condition (`--shutdownAfter` in seconds, an interactive quit, or a controlled interrupt). Start with a specific file or glob and a specific filter when possible; expand only if the question needs it.
3. Put command flags before positional paths. Quote globs, regular expressions, and MapReduce queries so the shell does not expand or alter them. Use `--files` and `--query` for unambiguous commands.
4. Check the command's status and diagnostics before calling a result complete. A zero-match `dgrep` returns `0`; `dmap` can write a partial result and warn while still exiting `0`. See [results and limits](references/results-and-limits.md).
5. Report the hosts and source paths queried, the relevant result or summary, and any failed or missing sources. Avoid treating a subset of hosts as a fleet-wide answer.

## Common commands

```sh
dcat --servers 'web01,web02:2223' --files '/var/log/app/*.log'
dgrep --servers servers.txt --files '/var/log/app/*.log' --grep 'ERROR|FATAL' --before 2 --after 3
dtail --servers servers.txt --files 'journal:myapp.service' --grep ERROR --shutdownAfter 60
dmap --servers servers.txt --files '/var/log/dserver/*.log' --query 'from STATS select $hostname,max($goroutines) group by $hostname'
dcat --plain --logger stdout --files /etc/hostname  # local, no dserver
dtailhealth --server web01:2222
```

Read [client operations](references/client-operations.md) for server selection, SSH, config, all client flags, local input, compression, and command-specific options. Read [MapReduce](references/mapreduce.md) when writing a `--query`, selecting a log format, reading CSV, or producing an outfile. Read [results and limits](references/results-and-limits.md) for interactive reloads, output, exit codes, capability requirements, and incomplete results.

This skill describes the clients in this repository. When a deployed binary may differ, inspect its `-h` output and version before relying on a newer flag or capability. The full user documentation is in `doc/quickstart.md`, `doc/examples.md`, `doc/querylanguage.md`, `doc/logformats.md`, and `doc/interactive-query.md` at the repository root.
