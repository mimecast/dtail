DTail Server Configuration Guide
================================

This page documents the configuration of `dserver`, the DTail server daemon. It
covers read permissions, connection limits, journal reads, shared reads and the
built-in job scheduler.

## The configuration file

`dserver` reads a single JSON configuration file. The location depends on how
it is started:

* With the `-cfg` flag, exactly that file is read. The provided
  `dserver.service.example` systemd unit passes
  `-cfg /etc/dserver/dtail.json`.
* Without `-cfg`, `dserver` searches `~/.config/dtail/dtail.conf` first and
  falls back to `~/.dtail.conf` if that does not exist. The first file found
  wins; files are never merged.
* With `-cfg none`, no configuration file is read at all.

The same file can hold a `Common`, a `Server` and a `Client` section; only
`Common` and `Server` apply to `dserver`. The `Common` section carries the
SSH port (`SSHPort`, default `2222`), the log directory and the cache
directory, among other things.

A commented example configuration ships in
[examples/dtail.json.example](../examples/dtail.json.example). The JSON schema
in [examples/dtail.schema.json](../examples/dtail.schema.json) describes every
supported key; a unit test keeps it mechanically in sync with the code, so a
new configuration field can not silently be missing from the schema. Unknown
keys in old configuration files are ignored by the lenient config decoder.

`dserver`'s log level (`--logLevel`, or `Common.LogLevel`) only decides what
`dserver` writes to its own log. The warnings and errors it sends to clients
(e.g. a missing file, a glob matching too many files, a split long line) do
not depend on it: a `dserver` at `--logLevel error` or `none` still sends a
client the missing-file warning, with its full text. How the client shows
these warnings is described in
[Server warnings and log levels](./examples.md#server-warnings-and-log-levels).

## Read permissions

The `Server.Permissions` map controls which files each SSH user may read:

```json
"Permissions": {
  "Default": [
    "readfiles:^/var/log/.*$"
  ],
  "Users": {
    "jamesblake": [
      "readfiles:!^/var/log/secret/.*$",
      "readfiles:^/var/log/.*$"
    ]
  }
}
```

* `Default` applies to every user who has no own entry; `Users` maps a user
  name to its own rule list, which replaces the default list.
* A rule has the form `readfiles:<regex>`. The `readfiles:` prefix is
  optional and the default; a bare regex means the same thing.
* A rule prefixed with `!` (as in `readfiles:!<regex>`) is a deny rule.
  **Deny wins**: the list is scanned in order and the first matching deny
  rule rejects the path immediately, no matter what allow rules follow.
* If no allow rule matches, the path is denied (deny by default).
* The regex is matched against the resolved absolute file path (symlinks are
  resolved first), so write rules for the real path, not for a symlink.
* The DTail permissions are checked in addition to the operating system's
  file permissions: the user still needs OS read access to the file (or the
  `dserver` run user does, when built without Linux ACL support).
* Journal targets are strings like `journal:nginx.service`, and the rule must
  match that full target string, e.g. `"readfiles:^journal:.*\\.service$"`.

`dserver`'s own scheduled and continuous jobs (see below) run as internal
users and bypass these permission checks; they read what the job
configuration tells them to read.

## Connection and session limits

Basic SSH server settings:

| Key | Default | Meaning |
| --- | --- | --- |
| `Server.SSHBindAddress` | `0.0.0.0` | Address the SSH server binds to. |
| `Common.SSHPort` | `2222` | Port the SSH server listens on. |
| `Server.HostKeyPath` | `./cache/ssh_host_key` | Private SSH host key (generated if missing). The legacy `Server.HostKeyFile` key is still accepted. |
| `Server.HostKeyBits` | `4096` | Key size in bits when generating a new host key. |
| `Server.AuthorizedKeysPath` | *(empty)* | One `authorized_keys` file for all users. Empty uses the per-user key cache and the usual `~/.ssh/authorized_keys` lookup. |
| `Server.KeyExchanges`, `Server.Ciphers`, `Server.MACs` | *(empty)* | Restrict the allowed SSH algorithms. |

Connection, session and read limits:

| Key | Default | Meaning |
| --- | --- | --- |
| `Server.MaxConnections` | `10` | Maximum concurrent user connections. Connections still in their SSH handshake count too. |
| `Server.MaxConcurrentCats` | `2` | Maximum concurrent one-shot reads (`dcat`, `dgrep`, `dmap`) per server. |
| `Server.MaxConcurrentTails` | `50` | Maximum concurrent follow reads (`dtail`) per server. |
| `Server.IdleSessionTimeoutS` | `900` | Rolling inactivity timeout for authenticated sessions, in seconds. Connections with no SSH activity are closed about the timeout plus a few seconds later; active follow sessions stay connected as every read or write refreshes the deadline. Values of `0` or less use the default. |
| `Server.MaxLineLength` | `1048576` | Maximum line length in bytes before a line of a file read, or an entry of a `journal:` read, is split into multiple lines. The length counts a line's bytes without its `\n`, but including the `\r` of a CRLF line ending. Lines of up to this length are read whole, also when the limit is above 1 MiB; a longer line is cut into pieces of exactly this length, in follow reads (`dtail`) as in one-shot reads (`dcat`, `dgrep`, `dmap`), and the client gets a long-line warning. `journal:` targets cut their entries the same way, with the same warning, in follow reads as in one-shot reads, so a long entry no longer ends the journal read; only an unterminated last entry, which a stopped `journalctl` can leave, is dropped (its pieces cut off before are kept). Memory per concurrent read: a one-shot file read holds a pooled buffer of 1 MiB + 1 byte. A `journal:` read, one-shot or follow, holds two for the whole `journalctl` run, one scanning its stdout (the entries) and one its stderr; a `journalctl` stderr line longer than 1 MiB is forwarded to the client cut to 1 MiB, with a note, and its rest is dropped. For a line longer than 1 MiB (only with a limit above 1 MiB) the read (a journal read with its stdout scan) grows a buffer of its own by doubling, held alongside the pooled one, of up to about twice the longest line it read, capped at `MaxLineLength` + 1 bytes; while it grows, the previous and the new buffer exist together for a moment (up to about 1.5 times the final buffer). A follow file read holds the unfinished line, up to `MaxLineLength` bytes, likewise in a buffer that grows. `Server.OutputBufferMaxBytes` must be at least this plus 128 KiB; dserver refuses to start when it is not, or when `MaxLineLength` is so large that this sum exceeds the largest integer. |
| `Server.MaxGlobTargets` | `1000` | Maximum number of file paths a read command's glob may expand to. Excess paths are dropped and the client is told `read: more files than the server reads`. |
| `Server.MaxCommandFrameSize` | `1048576` | Maximum size in bytes of a single command frame. Larger frames are rejected and the session closed. |
| `Server.ReadGlobRetryIntervalMs` | `5000` | Retry interval in milliseconds while waiting for a glob to match a file (e.g. a log file that appears later in the day). |
| `Server.ReadRetryIntervalMs` | `2000` | Retry interval in milliseconds for re-reading in tail/cat retry loops. |

## Output path limits

These keys bound the memory a single session's pending output can retain and
the time the server waits on a slow client:

| Key | Default | Meaning |
| --- | --- | --- |
| `Server.OutputBufferMaxBytes` | `2097152` (2 MiB) | Maximum payload backing memory retained per SSH session while the output waits for the client. Producers apply backpressure when the cap is reached. Must be at least `MaxLineLength` + 131072 (two 64 KiB writer batches), otherwise dserver refuses to start. A session using escaped wire framing (current clients and servers) raises its own cap to at least 2 × `MaxLineLength` + 131072, as escaping can almost double a line frame, so the configured value does not need to cover that. |
| `Server.OutputFlushTimeoutMs` | `2000` | Maximum time the session reader may make no progress while output is drained, in milliseconds. A client that keeps reading, however slowly, or that paused reading while dserver is still writing to it (a pager, a stopped pipeline) is waited for; the idle session timeout bounds a client that never comes back. |
| `Server.OutputReadRetryIntervalMs` | `1` | Compatibility fallback interval for detecting a stale output generation (for older writers that provide no cancellation signal). Values below the implementation's safety minimum are clamped. |
| `Server.OutputEOFAckTimeoutMs` | `2000` | Maximum time to wait for the output EOF acknowledgement after signaling EOF, in milliseconds. |

## Auth-key fast reconnect

The client can register a public key with `dserver` over an already
authenticated session; later connections then authenticate against that
in-memory key without touching `authorized_keys` or a YubiKey. The server side
of the feature is controlled by three keys:

| Key | Default | Meaning |
| --- | --- | --- |
| `Server.AuthKeyEnabled` | `true` | Enable in-memory auth-key registration and fast reconnect. |
| `Server.AuthKeyTTLSeconds` | `86400` | Time to live of a cached auth key in seconds. |
| `Server.AuthKeyMaxPerUser` | `5` | Maximum number of cached auth keys per user. |

See the [Auth-Key Fast-Reconnect](./auth-key-fast-reconnect.md) documentation
for the full protocol description.

## Journal reads

On hosts with systemd, clients can read the journal instead of a file by
passing a `journal:unit.service` target (e.g.
`dcat server1 'journal:nginx.service'`).

* The server advertises the capability `journal-v1`, and only on Linux and
  only when `journalctl` is available on `PATH`. Clients reject journal
  targets when the server does not advertise it.
* A non-follow read (`dcat`, `dgrep`, `dmap`) reads the current journal
  snapshot once. A follow read (`dtail`) runs `journalctl` with `-f -n 0` and
  keeps restarting it until the client disconnects.
* Each journal entry is one line. Entries longer than `Server.MaxLineLength`
  are cut into pieces of exactly that length, with a long-line warning to
  the client, as file reads cut their lines; a long entry does not end the
  read.
* Journal targets are authorized like files, but the permission rule must
  match the full target string including the `journal:` prefix, for example
  `"readfiles:^journal:.*\\.service$"`.

## Shared reads

Shared reads are on by default; set `Server.SharedReadsDisable` to `true` to
turn them off. Sessions that follow (tail) the same uncompressed file share a
single reader of it in `dserver`; each session still passes its own permission
check and filters and numbers its lines itself. A session that falls too far
behind is evicted from the shared reader, keeps reading with a private reader
and rejoins the shared reader once it caught up. Scheduled jobs that the
scheduler starts in the same wave and that read the same files of the same
`dserver` share a one-shot read in the same way.

## Scheduled and continuous jobs

`dserver` can run MapReduce jobs on its own schedule. Both job arrays live in
the `Server` section:

```json
"Schedule": [
  {
    "Name": "daily-errors",
    "Enable": true,
    "Files": "/var/log/app/app-$today.log",
    "Query": "select count(*)",
    "Outfile": "out/daily-errors-$today.map",
    "Servers": ["localhost"],
    "TimeRange": [9, 17]
  }
],
"Continuous": [
  {
    "Name": "live-errors",
    "Enable": true,
    "Files": "/var/log/app/errors.log",
    "Query": "select count(*)",
    "Outfile": "out/live-errors.map",
    "RestartOnDayChange": true
  }
]
```

Job fields (both job types share them unless noted):

| Field | Applies to | Meaning |
| --- | --- | --- |
| `Name` | both | Job name. The job's SSH connections authenticate with this name, so it doubles as a shared secret. |
| `Enable` | both | Set to `true` to run the job at all. |
| `Files` | both | Comma-separated file list or glob to read. Supports the date placeholders below. |
| `Query` | both | A [DTail query language](./querylanguage.md) MapReduce query. |
| `Outfile` | both | Where the MapReduce result is written. Supports the date placeholders below. |
| `Servers` | both | Server list, joined with commas into the client's `--servers` value. Whitespace around entries and empty entries are ignored. Without a `Discovery` module, a single entry naming an existing file is read as a server list file (one server per line; blank lines, `#` comment lines and a leading UTF-8 byte order mark are ignored). Empty, or only blank entries, means this `dserver` itself, without a `Discovery` module or with `COMMA`: its `Server.SSHBindAddress` at `Common.SSHPort`, or the loopback address (`127.0.0.1`, or `::1` for `::`) when it listens on every address (`""`, `0.0.0.0`, `::ffff:0.0.0.0` or `::`). With `FILE`, an empty `Servers` makes `FILE` try to open a file named after this `dserver`'s address (e.g. `127.0.0.1`), so the job fails. |
| `Discovery` | both | Optional server discovery module (the client's `--discovery` value): `COMMA` or `FILE`, case-insensitive (builds may add more). It only selects how `Servers` is read; for `COMMA` and `FILE` the servers always come from `Servers`. `COMMA` reads `Servers` as the server list, `FILE` reads the server list file `Servers` names, e.g. `"Discovery": "file", "Servers": ["/etc/dserver/serverlist"]`. A `MODULE:options` suffix is only for discovery modules a build adds; `COMMA` and `FILE` take no options and reject them (in any case): `comma:server1,server2` or `file:/path/to/serverlist` fails the job's client with `discovery module "comma" takes no options; put the servers in Servers/--servers`, so a scheduled job writes no outfile and is retried after its backoff, and a continuous job logs the error and retries at its next run. Put the servers, or the server list file, in `Servers`. |
| `AllowFrom` | both | Optional list of addresses the job's SSH connections are accepted from (hostnames are resolved). A job with empty `Servers` on a `dserver` listening on every address connects from the loopback address, so its `AllowFrom` must resolve to `127.0.0.1` or `::1` (e.g. `localhost`). |
| `TimeRange` | `Schedule` only | `[startHour, endHour]` (24h clock); the job runs when `startHour <= hour < endHour`. `[0, 24]` means the whole day. |
| `RestartOnDayChange` | `Continuous` only | Restart the job when the date placeholders roll over at midnight. |

`Files` and `Outfile` support date placeholders, replaced with the date in
`YYYYMMDD` form: `$today`, `$tomorrow`, `$yesterday` (1 day ago), `$yyesterday`
(2 days ago) and `$yyyesterday` (3 days ago). These are calendar days in the
scheduling server's local timezone, including across daylight-saving changes.
Both fields use the same time captured for that run.

**Failure semantics (scheduled jobs).** A scheduled job only writes its
outfile while its run completed successfully. Within the job's `TimeRange`, a
failed run (a server that could not be reached, or a file that could not be
read) writes **no** outfile, leaves an outfile of an earlier run untouched,
and the job retries with a growing backoff (1, 2, 4, ... minutes, up to an
hour) as long as the range lasts. Once the `TimeRange` has ended, the
scheduler keeps trying with so-called final runs: if only file read failures
occurred by then, it writes the partial result of what could be read; if
there were transport failures (no connection at all), it still writes
nothing and keeps retrying. 24 hours after the `TimeRange` ended, the
scheduler gives up on the outfile entirely. A `dserver` restart forgets all
failure state: within the `TimeRange` the job then runs strictly again, but a
`dserver` restarted after the range ended does not know about the failed run
and writes no outfile for that period.

Continuous jobs and interactive `dmap` with an outfile keep writing interim
and final results whatever the exit status.

A job whose servers name no server at all, e.g. a server list file of only
blank and comment lines, fails before it connects anywhere (`no server to
connect to`, logged at `WARN` as `Unable to create job <name>`). A scheduled
job counts that as a failed run (see above). A continuous job writes no
outfile, so an earlier outfile is not overwritten with empty results, and it
runs again a minute later, as after any other exit, without a backoff.

## A minimal example configuration

```json
{
  "Common": {
    "SSHPort": 2222,
    "LogDir": "log",
    "CacheDir": "cache"
  },
  "Server": {
    "SSHBindAddress": "0.0.0.0",
    "HostKeyPath": "cache/ssh_host_key",
    "MaxConnections": 50,
    "MaxConcurrentCats": 8,
    "MaxConcurrentTails": 100,
    "IdleSessionTimeoutS": 900,
    "MapreduceLogFormat": "default",
    "AuthKeyEnabled": true,
    "AuthKeyTTLSeconds": 86400,
    "AuthKeyMaxPerUser": 5,
    "Permissions": {
      "Default": [
        "readfiles:^/var/log/.*$",
        "readfiles:!^/var/log/secret/.*$"
      ],
      "Users": {
        "alice": [
          "readfiles:^/var/log/.*$",
          "readfiles:^journal:.*\\.service$"
        ]
      }
    },
    "Schedule": [
      {
        "Name": "daily-nginx-errors",
        "Enable": true,
        "Files": "/var/log/nginx/access.log-$today",
        "Query": "select count(*)",
        "Outfile": "out/nginx-errors-$today.map",
        "TimeRange": [0, 24]
      }
    ]
  }
}
```
