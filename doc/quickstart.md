Quick Starting Guide
====================

This is the quick starting guide. For a more sustainable setup, involving how to create a background service via ``systemd``, recommendations about automation via Jenkins and/or Puppet and health monitoring via Nagios please also follow the [Installation Guide](installation.md).

This guide assumes that you know how to generate and configure a public/private SSH key pair for secure authorization and shell access. For more information please have a look at the OpenSSH documentation of your distribution.

# Install it

On Linux you need to install the libacl development library for file system ACL permission support in `dserver`. On CentOS and/or Fedora it would be

```console
% sudo yum install libacl-devel -y
```

To compile and install all DTail binaries directly from GitHub run:

```console
% for cmd in dcat dgrep dmap drun dtail dserver; do
    go get github.com/mimecast/dtail/cmd/$cmd;
  done
```

It produces the following executables in ``$GOPATH/bin``:

* ``dcat``: Client for displaying whole files remotely (distributed cat)
* ``dgrep``: Client for searching whole files files remotely using a regex (distributed grep)
* ``dmap``: Client for executing distributed mapreduce queries (may will consume a lot of RAM and CPU)
* ``drun``: Client for executing commands on remote servers.
* ``dtail``: Client for tailing/following log files remotely (distributed tail)
* ``dserver``: The DTail server

# Start DTail server

Copy the ``dserver`` binary to the remote server machines of your choice (e.g. ``serv-001.lan.example.org`` and ``serv-002.lan.example.org``) and start it on each of the servers as follows:

```console
% ./dserver
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

DTail relies on SSH for secure authentication and communication. The clients (all client binaries such as ``dtail``, ``dgrep`` and so on...) communicate with an auth backend via the SSH auth socket. The SSH auth socket is configured via the environment variable ``SSH_AUTH_SOCK`` which usually points to ``~/.ssh/ssh_auth_socket`` or similar (depending on your configuration it may also point to other auth backends such as GPG Agent, in which case ``SSH_AUTH_SOCK`` would point to ``~/.gnupg/S.gpg-agent.ssh`` or similar).

Usually you would use the SSH Auth Agent. For this the private SSH key has to be registered at the SSH Agent:

```console
% ssh-add ~/.ssh/id_rsa
Enter passphrase for ~/.ssh/id_rsa: **********
Identity added: ~/.ssh/id_rsa (~/.ssh/id_rsa)
```

To test whether SSH is setup correctly you should be able to SSH into the servers with the OpenSSH client and your private SSH key through the SSH Agent without entering the private keys passphrase. The following assumes to have an OpenSSH server running on ``serv-001.lan.example.org`` and an OpenSSH client installed on your laptop or workstation. Please notice that DTail does not require to have an OpenSSH infrastructure set up but DTail uses by default the same public/private key file paths as OpenSSH. OpenSSH can be of a great help to verify that the SSH keys are configured correctly:

```console
workstation01 ~ % ssh serv-001.lan.example.org
serv-001 ~ %
serv-001 ~ % exit
workstation01 ~ %
```

Please consult the OpenSSH documentation of your distribution if the test above does not work for you.

## Run DTail client

Now it is time to connect to the DTail servers through the DTail client:

```console
% dtail --servers serv-001.lan.example.org,server-002.lan.example.org --files "/var/log/service/*.log"
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
```

Have a look [here](examples.md) for more usage examples.
