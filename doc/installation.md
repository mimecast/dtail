DTail Installation Guide
========================

The following installation guide has been tested successfully on CentOS 7. You may need to adjust accordingly depending on the distribution you use.

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
uid=1001(dserver) 1001=670(dserver) groups=1001(dserver)
```

3. Create the required file system structure and set the correct permissions:

```console
% sudo mkdir -p /etc/dserver /var/run/dserver
% sudo chown -R dserver:dserver /var/run/dserver
```

4. Install the ``dtail.json`` config to ``/etc/dserver/dtail.json``. An example can be found [here](../examples/dtail.json.example).

```console
% sudo mkdir /etc/dserver
% curl https://raw.githubusercontent.com/mimecast/dtail/master/examples/dtail.json.example |
    sudo tee /etc/dserver/dtail.json
```

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

# Prune old dserver log files

Log files live under ``/var/run/dserver/log`` (see ``LogDir`` in ``dtail.json``). To remove ``*.log`` files **older than seven days**, install [prune_dserver_logs.sh](../examples/prune_dserver_logs.sh.example) and a systemd timer (runs daily with a randomized delay):

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

