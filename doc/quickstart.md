Quick Starting Guide
====================

This is the quick starting guide. For a more sustainable setup involving creating a background service via ``systemd``, recommendations about automation via Jenkins and Puppet and health monitoring via Nagios, please follow the [Installation Guide](installation.md).

This guide assumes that you know how to generate and configure a public/private SSH key pair for secure authorization and shell access. For more information, please have a look at the OpenSSH documentation of your distribution.

# Install it

To compile and install all DTail binaries directly from GitHub run:

```console
% for cmd in dcat dgrep dmap dtail dserver dtailhealth; do
    go install github.com/mimecast/dtail/cmd/$cmd@latest;
  done
```

It produces the following executables in ``$GOPATH/bin``:

* ``dcat``: Client for displaying whole files remotely (distributed cat)
* ``dgrep``: Client for searching whole files remotely using a regex (distributed grep)
* ``dmap``: Client for executing distributed MapReduce queries (may consume a lot of RAM and CPU)
* ``dtail``: Client for tailing/following log files remotely (distributed tail)
* ``dtailhealth``: Client for dserver health checks
* ``dserver``: The DTail server

Alternatively, you can clone the repository and run ``make build``, which compiles all of the binaries above plus ``dtail-tools`` (a helper binary with benchmarking, profiling and PGO tooling) into the repository root.

# Start DTail server

Copy the ``dserver`` binary to the remote server machines of your choice (e.g. ``serv-001.lan.example.org`` and ``serv-002.lan.example.org``) and start it on each of the servers as follows:

```console
❯ ./dserver --logger Stdout --logLevel debug --bindAddress $(hostname) --port 2222
DTail 4.3.2-ng Protocol 4.1 Have a lot of fun!
INFO|20250923-222429|Starting server|DTail 4.3.2-ng Protocol 4.1 Have a lot of fun!
INFO|20250923-222429|Reading private server RSA host key from file|./cache/ssh_host_key
INFO|20250923-222429|Starting server
INFO|20250923-222429|Binding server|X.Y.Z.W:2222
DEBUG|20250923-222429|Starting listener loop
INFO|20250923-222429|Starting continuous job runner after 2s
INFO|20250923-222429|Starting scheduled job runner after 2s
```

``dserver`` is now listening on TCP port 2222 and waiting for incoming connections. All SSH keys listed in ``~/.ssh/authorized_keys`` are now respected by the DTail server for authorization. On first start, the server generates an RSA host key into ``./cache/ssh_host_key`` (configurable via the ``HostKeyPath`` config setting).

# Setup DTail client

## Setup SSH

Ensure that your public SSH key is listed in ``~/.ssh/authorized_keys`` on all server machines involved. The private SSH key counterpart should preferably stay on your Laptop or workstation in one of the default key locations ``~/.ssh/id_rsa``, ``~/.ssh/id_dsa``, ``~/.ssh/id_ecdsa`` or ``~/.ssh/id_ed25519`` — the DTail client tries all of them in that order.

DTail relies on SSH for secure authentication and communication. You can either use an SSH Agent or a private SSH key file directly.

### SSH Agent

The clients (all client binaries such as ``dtail``, ``dgrep`` and so on...) communicate with an auth backend via the SSH auth socket. The SSH auth socket is configured via the environment variable ``SSH_AUTH_SOCK`` which usually points to ``~/.ssh/ssh_auth_socket`` or similar (depending on your configuration, it may also point to other auth backends such as GPG Agent, in which case ``SSH_AUTH_SOCK`` would point to ``~/.gnupg/S.gpg-agent.ssh`` or similar).

Usually you would use the SSH Auth Agent. For this the private SSH key has to be registered at the SSH Agent:

```console
% ssh-add ~/.ssh/id_rsa
Enter passphrase for ~/.ssh/id_rsa: **********
Identity added: ~/.ssh/id_rsa (~/.ssh/id_rsa)
```

To test whether SSH is set up correctly, you should be able to SSH into the servers with the OpenSSH client and your private SSH key through the SSH Agent without entering the private key's passphrase. The following assumes to have an OpenSSH server running on ``serv-001.lan.example.org`` and an OpenSSH client installed on your laptop or workstation. Please notice that DTail does not require to have an OpenSSH infrastructure set up, but DTail uses by default the same public/private key file paths as OpenSSH. OpenSSH can be of great help to verify that the SSH keys are configured correctly:

```console
workstation01 ~ % ssh serv-001.lan.example.org
serv-001 ~ %
serv-001 ~ % exit
workstation01 ~ %
```

Please consult the OpenSSH documentation of your distribution if the test above does not work for you.

### SSH Private Key file

As an alternative to using an SSH Agent, an SSH private key file can be used directly. Just add the argument ``--auth-key-path ~/.ssh/id_rsa`` (pointing to your private key) to the DTail client. The same path can also be configured via the ``DTAIL_AUTH_KEY_PATH`` environment variable or the ``Client.AuthKeyPath`` config setting. Password-protected keys are supported via the ``DTAIL_KEY_PASSPHRASE`` environment variable (there is no interactive passphrase prompt, so env-var is the only way).

## Run DTail client

Now it is time to connect to the DTail servers through the DTail client:

```console
% dtail --servers serv-001.lan.example.org,serv-002.lan.example.org --files "/var/log/service/*.log"
Encountered 2 unknown hosts: 'serv-001.lan.example.org:2222,serv-002.lan.example.org:2222'
Do you want to trust these hosts?? (y=yes,a=all,n=no,d=details): y
CLIENT|workstation01|INFO|Added hosts to known hosts file|/home/user/.ssh/known_hosts
REMOTE|serv-001|100|1|service.log|2025-09-23T22:25:01|INFO|Service started
REMOTE|serv-002|100|1|service.log|2025-09-23T22:25:01|INFO|Service started
CLIENT|workstation01|INFO|STATS:STATS|connected=2|servers=2|connected%=100|new=2|throttle=0|goroutines=37|cgocalls=7|cpu=8
.
.
```

Without the ``--cfg`` flag, all client commands look for a JSON config file at ``~/.config/dtail/dtail.conf`` first and, if that does not exist, at ``~/.dtail.conf``. The first file that exists wins (they are not merged).

# What else can it do?

* **Serverless mode**: All clients also work without any ``dserver`` — just omit the server list (or pass ``--servers serverless``) to read local files directly. See [examples.md](examples.md).
* **Journal reads**: On Linux, a ``journal:unit.service`` file target follows or reads the systemd journal of that unit (requires the server to advertise the ``journal-v1`` capability, i.e. ``journalctl`` on ``PATH``).
* **Auth-key fast reconnect**: Enabled by default; the client registers a key with ``dserver`` on first connect so repeated connections skip the normal SSH auth round-trips. See [auth-key-fast-reconnect.md](auth-key-fast-reconnect.md).
* **Interactive query control**: ``--interactive-query`` keeps the run open for ``:reload <flags>``, ``:show``, ``:help`` and ``:quit`` control commands. See [interactive-query.md](interactive-query.md).

Have a look [here](examples.md) for more usage examples.