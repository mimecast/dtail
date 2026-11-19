DTail Installation Guide
========================

This guide targets maintained Linux distributions. Its service examples use
`systemd` and its package examples use `dnf` (RHEL, Rocky Linux and Fedora);
adapt them for your platform. Install at least the Go version declared in
[`go.mod`](../go.mod) (currently Go 1.26.6) before building.

# Compile it

Please check the [Quick Starting Guide](quickstart.md) for instructions on compiling DTail. It is recommended to automate the build process via your build pipeline (e.g. produce a deployable (.rpm, .deb, ...) via Jenkins). You don't have to use ``go install...`` to compile and install the binaries. You can also clone the repository and use ``make`` instead.

## Linux ACL support

This is optional, but it gives you better security. On Linux, you have the option to compile `dserver` with File System Access Control List support. For that, you need:

### 1. Install the `libacl` development library. On RHEL, CentOS and Fedora, it would be

```console
% sudo dnf install libacl-devel -y
```

### 2. Enable ACL via a Go build flag

Set the `DTAIL_USE_ACL` environment variable before invoking the make command.

```console
% export DTAIL_USE_ACL=yes
```

Alternatively, you could add `-tags linuxacl` to the Go compiler.

## Build without zstd (optional)

For targets where CGO-based zstd is unavailable (for example cross-compiling `dserver` for another architecture), build with the `nozstd` tag. Compressed `.zst` log files will not be supported in that binary.

```console
% export DTAIL_NO_ZSTD=yes
```

This sets `-tags nozstd` via the Makefile. Plain `go build` users can pass `-tags nozstd` directly.

## Proprietary features (optional)

Builds can enable the proprietary features with `DTAIL_USE_PROPRIETARY=yes make build`. Leave it off for the regular open-source build.

# Install it

It is recommended to automate all the installation process outlined here. You could use a configuration management system such as Puppet, Chef or Ansible. However, that relies heavily on how your infrastructure is managed and is out of scope of this documentation.

1. The ``dserver`` binary has to be installed on all machines (server boxes) involved. A good location for the binary would be ``/usr/local/bin/dserver`` with permissions set as follows:

```console
% sudo chown root:root /usr/local/bin/dserver
% sudo chmod 0755 /usr/local/bin/dserver
```

2. Create the ``dserver`` run user and group. The user could look like this:

```console
% sudo adduser dserver
% id dserver
uid=1001(dserver) gid=1001(dserver) groups=1001(dserver)
```

3. Create the required file system structure and set the correct permissions:

```console
% sudo mkdir -p /etc/dserver /var/run/dserver
% sudo chown -R dserver:dserver /var/run/dserver
```

4. Install the ``dtail.json`` config to ``/etc/dserver/dtail.json``. An example can be found [here](../examples/dtail.json.example).

```console
% curl https://raw.githubusercontent.com/mimecast/dtail/master/examples/dtail.json.example |
    sudo tee /etc/dserver/dtail.json
```

Config resolution note: ``dserver`` reads ``/etc/dserver/dtail.json`` only because the example ``systemd`` unit passes ``-cfg /etc/dserver/dtail.json``. Without ``-cfg``, every DTail binary searches for a config at ``~/.config/dtail/dtail.conf`` first and, if that does not exist, at ``~/.dtail.conf`` (the first existing file wins; they are not merged).

### Upgrade note: ``Common.Logger`` and ``Common.LogDir`` now take effect

Older releases silently ignored ``Common.Logger`` (and the client commands' ``Common.LogDir``) from the config file, because every command's ``--logger`` and ``--logDir`` flag default always replaced it. These config values now apply. The precedence is: explicit ``--logger``/``--logDir`` flag > ``Common.Logger``/``Common.LogDir`` from the config file > the per-command default (``file`` and ``log`` for dserver, ``fout`` and ``~/log`` for the client commands). dtailhealth now ignores both config values: it always uses the ``none`` logger unless given ``--logger``, and always the ``log`` directory. It previously honoured ``Common.LogDir`` when run with ``--logger file`` or ``--logger fout``; such log files now go to ``./log``.

Older copies of the example config contained ``"Logger": "Fout"``. If your installed ``/etc/dserver/dtail.json`` still has that line, dserver now tees its log to stdout as well as to the log file, so under systemd every line also lands in the journal. To keep the previous behaviour (log file only), set ``"Logger": "File"`` or remove the ``Logger`` key before restarting dserver after the upgrade:

```console
% grep -n '"Logger"' /etc/dserver/dtail.json
```

### Upgrade note: ``Client.TermColors`` names now take effect

Older releases copied every ``Client.TermColors`` value verbatim into the output, so a colour name such as ``"Red"`` or ``"Dim"`` was printed as literal text instead of a colour; only raw escape sequences such as ``"\u001b[31m"`` worked. Colour and attribute names (matched case-insensitively) are now converted to terminal escape codes, so a configured palette now changes the client's colours.

Existing config files keep loading, dserver included (it reads the same file but never renders colours):

* The prefixed spellings used by the example configs of v4.0.0 through v4.3.4 (``samples/dtail.json.sample``, later ``examples/dtail.json.example``) (``"FgBlack"``, ``"BgCyan"``, ``"AttrDim"``, ``"AttrNone"`` ...) are still accepted and mean the colour or attribute they name. They are deprecated, and ``examples/dtail.schema.json`` reports them; drop the ``Fg``/``Bg``/``Attr`` prefix to silence the schema.
* Raw SGR escape sequences (``ESC[...m``, including several concatenated ones and ``:``-separated parameters) are used unchanged, as before, and ``""`` still means no escape code. Other raw strings that older releases passed through verbatim (for example ``"\u001b[K"`` or an escape code followed by text) are now treated like any other invalid value, see below.
* Any other value (for example a typo such as ``"Bleu"``) no longer ends up in the output. dtail uses that field's default colour instead and prints a warning naming the config file and the value on stderr, for example ``WARN: config file /etc/dserver/dtail.json: ignoring invalid background color "Bleu", using the default instead: ...``. A bad colour value is never a fatal config error.

### SSH listen address (``SSHBindAddress``)

The example config sets ``Server.SSHBindAddress`` to ``0.0.0.0``, so dserver listens on **every** local IPv4 address, including your LAN (e.g. ``192.168.1.x`` on eth0) and any other interface (loopback, WireGuard, etc.). Clients reach it as ``<that-host-LAN-IP>:2222``; you do **not** need to change this for normal LAN access.

To listen **only** on a specific address—for example only the home LAN and not on a VPN—set ``SSHBindAddress`` in ``/etc/dserver/dtail.json`` to **that machine’s** address (each host needs its own value), e.g. ``192.168.1.125`` on ``pi0``, ``192.168.1.126`` on ``pi1``. Alternatively, override from the command line (after ``-cfg``): ``dserver -cfg /etc/dserver/dtail.json -bindAddress 192.168.1.125``. Then reload or restart dserver.

5. It is recommended to configure DTail server as a service to ``systemd``. An example unit file for ``systemd`` can be found [here](../examples/dserver.service.example).

```console
% curl https://raw.githubusercontent.com/mimecast/dtail/master/examples/dserver.service.example |
    sudo tee /etc/systemd/system/dserver.service
% sudo systemctl daemon-reload
```

The unit is intended to stay **disabled** until you opt in. Start DTail server manually when needed:

```console
% sudo systemctl start dserver
```

To start it automatically at boot, run once: `sudo systemctl enable dserver`.

# Configure it

The example config above only sets a few keys. The most important ``Server`` settings (built-in defaults, see ``examples/dtail.schema.json``):

| Key | Default | Purpose |
| --- | --- | --- |
| ``SSHBindAddress`` | ``0.0.0.0`` | Address the SSH server binds to |
| ``MaxConnections`` | ``10`` | Max concurrent user connections (SSH handshakes included) |
| ``IdleSessionTimeoutS`` | ``900`` | Rolling inactivity timeout of authenticated sessions, in seconds |
| ``MaxConcurrentCats`` | ``2`` | Max concurrent cat/grep/MapReduce reads; also bounds grouped scheduled jobs |
| ``MaxConcurrentTails`` | ``50`` | Max concurrent tails |
| ``SharedReadsDisable`` | ``false`` | Turn off shared reads (same-file follow reads and grouped job reads) |
| ``MaxLineLength`` | ``1048576`` | Max line length in bytes before a line is split |
| ``HostKeyPath`` | ``./cache/ssh_host_key`` | Private SSH host key (generated on first start) |
| ``HostKeyBits`` | ``4096`` | RSA host key size in bits |
| ``AuthKeyEnabled`` | ``true`` | In-memory auth-key registration and fast reconnect |
| ``AuthKeyTTLSeconds`` | ``86400`` | Auth-key cache entry TTL in seconds |
| ``AuthKeyMaxPerUser`` | ``5`` | Max cached auth keys per user |
| ``OutputBufferMaxBytes`` | ``2097152`` | Per-session payload backing memory cap; producers apply backpressure |
| ``OutputFlushTimeoutMs`` | ``2000`` | Max wait for the session reader to drain output |
| ``OutputReadRetryIntervalMs`` | ``1`` | Stale-generation fallback poll interval |
| ``OutputEOFAckTimeoutMs`` | ``2000`` | Max wait for the output EOF acknowledgement |
| ``MaxGlobTargets`` | ``1000`` | Max files one read command's glob may dispatch |
| ``Permissions.Default`` | ``["^/.*"]`` | Read permissions for users without a ``Users`` entry |

A few pointers beyond the table:

* **Auth-key fast reconnect**: ``AuthKeyEnabled``/``AuthKeyTTLSeconds``/``AuthKeyMaxPerUser`` tune the in-memory auth-key cache, see [auth-key-fast-reconnect.md](auth-key-fast-reconnect.md).
* **Session/output tuning**: ``IdleSessionTimeoutS`` plus the ``Output*`` keys above bound idle sessions and slow clients; ``SharedReadsDisable`` trades the shared-read optimization for per-session readers; ``MaxConnections``/``MaxConcurrentCats``/``MaxConcurrentTails`` size the connection and read slots.
* **Journal support**: dserver advertises the ``journal-v1`` capability on Linux when ``journalctl`` is on ``PATH``; clients can then read ``journal:unit.service`` targets (permission rules must match the full ``journal:...`` target).
* **Scheduled and continuous MapReduce jobs**: the ``Schedule`` and ``Continuous`` config arrays run MapReduce queries periodically (``TimeRange`` hours) or continuously and write CSV outfiles; see the config schema for the job fields.

# Start it

To start the DTail server via ``systemd`` run:

```console
% sudo systemctl start dserver
% sudo systemctl status dserver
● dserver.service - DTail server
   Loaded: loaded (/etc/systemd/system/dserver.service; disabled; vendor preset: disabled)
   Active: active (running) since Fri 2019-12-06 13:21:24 GMT; 2s ago
   Main PID: 12296 (dserver)
   Memory: 1.5M
   CGroup: /dserver.slice/dserver.service
     └─12296 /usr/local/bin/dserver -cfg /etc/dserver/dtail.json

    Dec 06 13:21:24 serv-001.lan.example.org systemd[1]: Started DTail server.
    Dec 06 13:21:24 serv-001.lan.example.org dserver[12296]: SERVER|serv-001|INFO|Launching server|server|DTail 1.0.0
    Dec 06 13:21:24 serv-001.lan.example.org dserver[12296]: SERVER|serv-001|INFO|Creating server|DTail 1.0.0
    Dec 06 13:21:24 serv-001.lan.example.org dserver[12296]: SERVER|serv-001|INFO|Reading private server RSA host key from file|cache/ssh_host_key
    Dec 06 13:21:24 serv-001.lan.example.org dserver[12296]: SERVER|serv-001|INFO|Starting server
    Dec 06 13:21:24 serv-001.lan.example.org dserver[12296]: SERVER|serv-001|INFO|Binding server|1.2.3.4:2222
```

### Firewall (firewalld on RHEL, Rocky Linux, Fedora, …)

The DTail server listens on TCP port ``2222`` (see ``SSHPort`` in ``dtail.json``). **ICMP (ping) may work while TCP to 2222 is blocked**, because host firewalls often allow ping but not arbitrary ports.

If ``firewalld`` is active, allow the DTail port permanently and reload:

```console
% sudo firewall-cmd --permanent --add-port=2222/tcp
% sudo firewall-cmd --reload
% sudo firewall-cmd --list-ports
```

Clients may report ``dial tcp …: connect: no route to host`` when the firewall rejects the connection with an ICMP unreachable—opening ``2222/tcp`` fixes that. For other firewalls (nftables, ufw, …), add an equivalent allow rule for ``2222/tcp``. A small helper script is [firewalld-dserver-port.sh.example](../examples/firewalld-dserver-port.sh.example).

# Register SSH public keys in DTail server

The DTail server now runs as a ``systemd`` service under system user ``dserver``. However, the system user ``dserver`` has no permissions to read the SSH public keys from ``/home/USER/.ssh/authorized_keys``. Therefore, no user would be able to establish an SSH session to DTail server. As an alternative path DTail server also checks for public SSH key files in ``/var/run/dserver/cache/USER.authorized_keys``.

It is recommended to execute [update_key_cache.sh](../examples/update_key_cache.sh.example) periodically to update the key cache. In case you manage your public SSH keys via Puppet you could subscribe the script to corresponding module. Or alternatively just configure a cron job or a systemd timer to run every once in a while, e.g. every 30 minutes:

```console
% curl https://raw.githubusercontent.com/mimecast/dtail/master/examples/update_key_cache.sh.example |
    sudo tee /var/run/dserver/update_key_cache.sh
% sudo chmod 755 /var/run/dserver/update_key_cache.sh
% curl https://raw.githubusercontent.com/mimecast/dtail/master/examples/dserver-update-keycache.service.example |
    sudo tee /etc/systemd/system/dserver-update-keycache.service
% curl https://raw.githubusercontent.com/mimecast/dtail/master/examples/dserver-update-keycache.timer.example |
    sudo tee /etc/systemd/system/dserver-update-keycache.timer
% sudo systemctl daemon-reload
% sudo systemctl start dserver-update-keycache.service
% sudo systemctl enable dserver-update-keycache.timer
% sudo systemctl start dserver-update-keycache.timer
```

**Note on persistent script locations:** ``/var/run`` (usually ``/run``) is a ``tmpfs``: the installed scripts vanish on reboot while the systemd timers keep firing (and failing). Installing the scripts into a persistent location such as ``/usr/local/bin`` is recommended instead — the example units reference ``/var/run/dserver/`` out of the box, so if you move the scripts, also update the ``ExecStart=`` paths in the service units accordingly. The dserver ``WorkingDirectory=/var/run/dserver`` with the relative ``CacheDir`` ``cache`` and ``LogDir`` ``log`` is fine to keep: those are regenerable caches and rotating logs.

# Prune old dserver log files

Log files live under ``/var/run/dserver/log`` (see ``LogDir`` in ``dtail.json``). To remove ``*.log`` files **older than seven days**, install [prune_dserver_logs.sh](../examples/prune_dserver_logs.sh.example) and a systemd timer (runs daily with a randomized delay). As with the key-cache script above, prefer a persistent location such as ``/usr/local/bin`` over ``/var/run/dserver`` — see the note there:

```console
% curl https://raw.githubusercontent.com/mimecast/dtail/master/examples/prune_dserver_logs.sh.example |
    sudo tee /var/run/dserver/prune_dserver_logs.sh
% sudo chmod 755 /var/run/dserver/prune_dserver_logs.sh
% curl https://raw.githubusercontent.com/mimecast/dtail/master/examples/dserver-prune-logs.service.example |
    sudo tee /etc/systemd/system/dserver-prune-logs.service
% curl https://raw.githubusercontent.com/mimecast/dtail/master/examples/dserver-prune-logs.timer.example |
    sudo tee /etc/systemd/system/dserver-prune-logs.timer
% sudo systemctl daemon-reload
% sudo systemctl enable --now dserver-prune-logs.timer
```

The script uses ``find /var/run/dserver/log -type f -name '*.log' -mtime +7 -delete``.

# Run DTail client

Now you should be able to use DTail client like outlined in the [Quick Starting Guide](quickstart.md). Also, have a look at the [Examples](examples.md).

# Monitor it

To verify that DTail server is up and running and functioning as expected, you should configure the Nagios check [check_dserver.sh](../examples/check_dserver.sh.example) in your monitoring system. The check has to be executed locally on the server (e.g. via NRPE). How to configure the monitoring system in detail is out of scope of this guide.

```console
% ./check_dserver.sh
OK: DTail SSH Server seems fine
```
