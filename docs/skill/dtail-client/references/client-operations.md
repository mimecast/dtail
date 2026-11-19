# Client operations

## Select servers and sources

- `--servers host1,host2:port` connects to several dservers; default SSH port is `2222`. `--servers servers.txt` reads one `HOST` or `HOST:PORT` per line. Blank lines and lines beginning with `#` are ignored. Whitespace around entries is trimmed. An empty server list is an error.
- `--discovery COMMA` treats `--servers` as a comma list; `--discovery FILE` treats it as a list file. These built-ins take no `:options` suffix. Builds may add discovery modules; check the deployed binary. A `/regex/` server filter applies to a custom discovery module's results, not to a normal comma or file list.
- With no `--servers` and no discovery module, all four data clients run locally without SSH or dserver. `--servers serverless` explicitly chooses this mode. An omitted `--files` in serverless mode reads standard input (`-`); for example `printf 'a\n' | dgrep --regex a`. A whitespace-only `--servers` value is an error, not serverless mode.
- `--files` accepts comma-separated files, paths, or globs; positional file arguments are joined into the same list. Blanks around each entry are stripped. Paths with commas or leading/trailing blanks cannot be addressed. Quote paths containing interior spaces; remote reading of these paths, and paths starting `base64%`, requires `encoded-read-path-v1` on dserver.
- `journal:unit.service` targets a systemd unit. `dcat`, `dgrep`, and `dmap` read a snapshot; `dtail` follows new entries. Remote servers must be Linux hosts with `journalctl` on `PATH` and advertise `journal-v1`; the server's `readfiles` permissions must allow the full `journal:` target.
- File readers recognize `.gz`, `.gzip`, and `.zst` by suffix. A build made without zstd support cannot read `.zst`. The dserver's file permissions and OS access still govern remote reads. A glob can be limited by `Server.MaxGlobTargets`.

## Choose a command

| Command | What it does | Specific flags |
| --- | --- | --- |
| `dcat` | Emit all lines of each selected file or snapshot. | Common flags only. |
| `dgrep` | Search existing input using Go regex syntax. | `--regex` (default `.`), `--grep` alias, `--invert`, `--before N`, `--after N`, `--max N` matching lines. |
| `dtail` | Follow new lines, reconnecting as needed. | Same filtering and context flags as `dgrep`; `--query`, `--timeout N`, `--shutdownAfter N` seconds (default 86400); `--colorTable`, `--wideColorTable`. `--checkHealth` is deprecated; use `dtailhealth`. |
| `dmap` | Run a MapReduce query over existing input. | `--query`, `--timeout N`. Query syntax and outfile are in [MapReduce](mapreduce.md). |
| `dtailhealth` | Check one dserver endpoint. | `--server HOST[:PORT]`; `--logger`, `--logLevel`, `--hostname-override`, `--no-auth-key`, `--pprof`, `--version`. No `--cfg`, `--auth-key-path`, or shared data-client flags. With no server it tests serverless mode and reports WARNING. |

`--grep` takes precedence over `--regex` when its value is nonempty. `dgrep --regex ''` is invalid; the default `.` matches nonempty lines, whereas `.*` matches every line including empty ones. With `--invert`, `.` selects only empty lines and `.*` selects none. `dtail --query` ignores `--regex`/`--grep` and `--invert`: the query's `from` table chooses the input lines. Query-driven `dtail` uses the same MapReduce language as `dmap`.

`--timeout N` is the maximum number of seconds a server collects data before disconnection; it is a CLI flag on `dmap` and `dtail`. For bounded `dtail` work, `--shutdownAfter N` limits the whole client run. `--max N` is a count of matching lines per read, not a global fleet-wide result limit.

## Shared data-client flags

These flags apply to `dcat`, `dgrep`, `dtail`, and `dmap`:

| Flags | Use |
| --- | --- |
| `--servers`, `--discovery`, `--port`, `--user` | Select hosts, discovery, default remote port, and SSH user. An explicit `HOST:PORT` overrides the default port. |
| `--files`, `--cfg` | Select input and JSON configuration file; `--cfg none` skips config loading. |
| `--auth-key-path`, `--no-auth-key`, `--agentKeyIndex`, `--known-hosts-path`, `--trustAllHosts` | Choose key authentication, disable fast reconnect, select SSH agent key (`-1` tries all), use a known-hosts file, or automatically trust unknown host keys. Use `--trustAllHosts` only when the user accepts that host-key trust choice. Deprecated `--key` aliases `--auth-key-path`. |
| `--plain`, `--quiet`, `--noColor` | Plain payload output, less diagnostic output, or disabled terminal colors. `--plain` also enables quiet and no-color behavior. |
| `--logger`, `--logDir`, `--logLevel`, `--log-payload` | Select output sink, log directory/level, and whether payload is also written to the daily client log file. See [results and limits](results-and-limits.md). |
| `--interactive-query`, `--control-tty` | Enable live `:reload` control and select its TTY device (default `/dev/tty`). |
| `--cpc`, `--hostname-override` | Set concurrent connection attempts per CPU and the hostname displayed in logs/output. |
| `--version`, `--pprof`, `--cpuprofile`, `--memprofile`, `--profile`, `--profiledir` | Show version or collect diagnostics and profiles. `--profile` enables both CPU and memory profiling; profile directory defaults to `profiles`. |

## Config and authentication

Without `--cfg`, a data client reads `~/.config/dtail/dtail.conf` if present, otherwise `~/.dtail.conf`; these files are not merged. The JSON file can contain `Common` and `Client` sections. `Common.SSHPort`, `Common.SSHConnectTimeoutMs`, `Common.Logger`, `Common.LogDir`, `Common.LogLevel`, `Common.HostnameOverride`, `Client.KnownHostsPath`, `Client.AuthKeyPath`, `Client.AuthKeyDisable`, `Client.LogPayload`, and `Client.TermColorsEnable` are client-relevant. For the schema and color palette, use `examples/dtail.schema.json` and `examples/dtail.json.example`. `dtailhealth` reads the user's default config but ignores `Common.Logger` and `Common.LogDir`.

SSH uses the selected local username (or `--user`) and the agent or private key. The private-key path precedence is `--auth-key-path` > `DTAIL_AUTH_KEY_PATH` > legacy `DTAIL_SSH_PRIVATE_KEYFILE_PATH` > `Client.AuthKeyPath` (default `~/.ssh/id_rsa`). An encrypted key can use `DTAIL_KEY_PASSPHRASE`; there is no interactive passphrase prompt. `DTAIL_HOSTNAME_OVERRIDE` is an environment fallback for the hostname override.

Fast reconnect is enabled by default: after ordinary SSH authentication, the client registers the public counterpart of its private key with dserver, which keeps it in memory for later connections. A restart, expiry, or mismatch falls back to normal SSH auth. Disable it with `--no-auth-key` or `Client.AuthKeyDisable`. `dtailhealth` supports only `--no-auth-key`, not `--auth-key-path`.

Unrecognized remote host keys prompt for trust and are stored in `known_hosts`; specify `--known-hosts-path` for an alternative file. `--trustAllHosts` skips the prompt and accepts keys that fail the known-hosts check, including a changed key, then updates the file. Use it only when that trust choice is intended.
