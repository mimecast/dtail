# Interactive Query Reload for DTail

## Overview

All DTail clients — `dtail`, `dgrep`, `dcat`, and `dmap` — accept the
`--interactive-query` flag. On capable servers the session is held open after
the initial workload finishes, and the client listens for control commands on
the controlling TTY. This makes it possible to adjust a running query
without paying for a new SSH handshake and reconnect round-trip per change.

## Control commands

The interactive session understands four control commands:

* `:reload <flags>` apply a new workload by reusing the current session when
  the active servers support it
* `:show` print the current interactive state, including capability counts
* `:help` print the interactive command help text
* `:quit` stop the interactive session

## Reload flags

The flags accepted by `:reload` depend on the mode the client runs in. Flags
not listed keep their current value; every reload requires at least one flag.

Shared flags (all clients):

* `--files` file(s) to read
* `--plain` plain output mode
* `--quiet` quiet output mode
* `--timeout` maximum time the `dserver` collects data until disconnection

Mode-specific flags:

* `dtail` (grep-driven) and `dgrep`: `--grep`/`--regex`, `--before`, `--after`,
  `--max`, `--invert`, plus the shared flags
* `dmap`, and query-driven `dtail` (started with `--query`): `--query`, plus
  the shared flags
* `dcat`: the shared flags only

## Examples

Keep a `dtail` run open and switch the grep pattern in-flight:

```bash
dtail --servers app01 --files /var/log/app.log --grep ERROR --interactive-query
# then type:
:reload --grep WARN
```

Change the match and its context in a running `dgrep`:

```bash
dgrep --servers app01 --files /var/log/app.log --grep ERROR --interactive-query
# then type:
:reload --grep WARN --before 2 --after 3
```

Adapt a distributed MapReduce query without reconnecting:

```bash
dmap --servers app01 --files /var/log/app.log \
  --query 'from STATS select count($line) group by hostname' \
  --interactive-query
# then type:
:reload --query "from STATS select count($line),avg(latency) group by hostname"
```

## How it works

* On startup, an interactive client first tries a `SESSION START` handshake
  when the remote side advertises the `query-update-v1` capability.
* If a server is older or does not advertise that capability, startup falls
  back to the legacy command stream automatically, so mixed-version
  client/server combinations still run the original workload normally.
* Live `:reload` updates require every active server to advertise
  `query-update-v1`; otherwise the reload is rejected with an error message
  and the current workload keeps running unchanged.
* On capable servers, DTail reuses the existing SSH session and sends
  `SESSION UPDATE` messages instead of reconnecting.
* Every successful reload advances a generation boundary; late output from the
  previous workload is dropped so stale matches do not leak into the new result
  stream.

## See also

* The [DTail Query Language](./querylanguage.md) for `--query` syntax.
* The [Server Configuration Guide](./server-configuration.md) for the
  server-side settings.