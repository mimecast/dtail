Quick Starting Guide
====================

This is the quick starting guide. For a more sustainable setup, involving how to create a background service via ``systemd``, recommendations about automation via Jenkins and/or Puppet and health monitoring via Nagios please also follow the [Installation Guide](installation.md).

This guide assumes that you know how to generate and configure a public/private SSH key pair for secure authorization and shell access. That is out of scope of this guide. For more information please have a look at the OpenSSH documentation of your distribution.

This guide also assumes that you know how to install and use a Go compiler and GNU make.

# Compile it

To produce all DTail binaries run ``make``:

```console
workstation01 ~/git/dtail % make
go build -o dtail ./cmd/dtail/main.go
go build -o dcat ./cmd/dcat/main.go
go build -o dgrep ./cmd/dgrep/main.go
go build -o dmap ./cmd/dmap/main.go
go build -o dserver ./cmd/dserver/main.go
```

It produces the following executables:

* ``dserver``: The DTail server
* ``dtail``: Client for tailing/following log files remotely (distributed tail)
* ``dcat``: Client for displaying whole files remotely (distributed cat)
* ``dgrep``: Client for searching whole files files remotely using a regex (distributed grep)
* ``dmap``: Client for executing distributed mapreduce queries (may will consume a lot of RAM and CPU)

# Start DTail server

Copy the ``dserver`` binary to the remote server machines of your choice (e.g. ``serv-001.lan.example.org`` and ``serv-002.lan.example.org``) and start it on each of the servers as follows:

```console
serv-001 ~ % ./dserver
SERVER|serv-001|INFO|Launching server|server|DTail 1.0.0
SERVER|serv-001|INFO|Creating server|DTail 1.0.0
SERVER|serv-001|INFO|Generating private server RSA host key
SERVER|serv-001|INFO|Starting server
SERVER|serv-001|INFO|Binding server|0.0.0.0:2222
```

``dserver`` is now listening on TCP port 2222 and waiting for incoming connections. All SSH keys listed in ``~/.ssh/authorized_keys`` are now respected by the DTail server for authorization.

# Setup DTail client

## Setup SSH

Make sure that your public SSH key is listed in ``~/.ssh/authorized_keys`` on all server machines involved. The private SSH key counterpart should preferably stay on your Laptop or workstation in ``~/.ssh/id_rsa`` or ``~/.ssh/id_dsa``.

DTail utilises the SSH Agent for SSH authentication. This is to avoid entering the passphrase of the private SSH key over and over again when a new SSH session is initiated from the DTail client to a new DTail server. For this the private SSH key has to be registered at the SSH Agent:

```console
workstation01 ~ % ssh-add ~/.ssh/id_rsa
Enter passphrase for ~/.ssh/id_rsa: **********
Identity added: ~/.ssh/id_rsa (~/.ssh/id_rsa)
```

The DTail client communicates with the SSH Agent through ``~/.ssh/ssh_auth_socket`` whenever a new session to a DTail server is established.

To test whether SSH is setup correctly you should be able to SSH into the servers with the OpenSSH client and your private SSH key through the SSH Agent without entering the private keys passphrase. The following assumes to have an OpenSSH server running on ``serv-001.lan.example.org`` and an OpenSSH client installed on your laptop or workstation. Please notice that DTail does not require to have an OpenSSH infrastructure set up but DTail uses by default the same public/private key file paths as OpenSSH. OpenSSH can be of a great help to verify that the SSH keys are configured correctly:

```console
workstation01 ~/git/dtail % ssh serv-001.lan.example.org
serv-001 ~ %
serv-001 ~ % exit
workstation01 ~/git/dtail %
```

## Run DTail client

Now it is time to connect to the DTail servers through the DTail client:

```console
workstation01 ~/git/dtail % ./bin/dtail --servers serv-001.lan.example.org,server-002.lan.example.org --files "/var/log/service/*.log"
CLIENT|workstation01|INFO|Launching client|tail|DTail 1.0.0
CLIENT|workstation01|INFO|Initiating base client
CLIENT|workstation01|INFO|Added SSH Agent to list of auth methods
CLIENT|workstation01|INFO|Deduped server list|1|1
CLIENT|workstation01|WARN|Encountered unknown host|{serv-002.lan.example.org:2222 0xc000146450 0xc00014a2f0 [serv-002.lan.example.org]:2222 ssh-rsa AAAA....
CLIENT|workstation01|WARN|Encountered unknown host|{serv-001.lan.example.org:2222 0xc0001ff450 0xc00ee4a2f0 [serv-001.lan.example.org]:2222 ssh-rsa AAAA....
Encountered 2 unknown hosts: 'serv-002.lan.example.org:2222 serv-001.lan.example.org:2222'
Do you want to trust these hosts?? (y=yes,a=all,n=no,d=details): y
CLIENT|workstation01|INFO|Added hosts to known hosts file|~/.ssh/known_hosts
CLIENT|workstation01|INFO|stats|connected=1/1(100%)|new=1|rate=0.20/s|throttle=0|cpus/goroutines=8/17
CLIENT|workstation01|INFO|stats|connected=1/1(100%)|new=0|rate=0.00/s|throttle=0|cpus/goroutines=8/17
CLIENT|workstation01|INFO|stats|connected=1/1(100%)|new=0|rate=0.00/s|throttle=0|cpus/goroutines=8/17
CLIENT|workstation01|INFO|stats|connected=1/1(100%)|new=0|rate=0.00/s|throttle=0|cpus/goroutines=8/17
.
.
.
```

Have a look [here](examples.md) for more usage examples.
