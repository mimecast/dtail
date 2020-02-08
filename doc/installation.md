DTail Installation Guide
========================

The following installation guide has been tested successfully on CentOS 7. You may need to adjust accordingly depending on the distribution you use.

# Compile it

Please check the [Quick Starting Guide](quickstart.md) for instructions how to compile DTail. It is recommended to automate the build process via your build pipeline (e.g. produce a deployable RPM via Jenkins). You don't have to use ``go get...`` to compile and install the binaries. You can also clone the repository and use ``make`` instead.

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
uid=670(dserver) gid=670(dserver) groups=670(dserver)
```

3. Create the required file system structure and set the correct permissions:

```console
% sudo mkdir -p /etc/dserver /var/run/dserver
% sudo chown -R dserver:dserver /var/run/dserver
```

4. Install the ``dtail.json`` config to ``/etc/dserver/dtail.json``. An example can be found [here](../samples/dtail.json.sample).

```console
% sudo mkdir /etc/dserver
% curl https://raw.githubusercontent.com/mimecast/dtail/master/samples/dtail.json.sample |
    sudo tee /etc/dserver/dtail.json >/dev/null
```

5. It is recommended to configure DTail server as a service to ``systemd``. An example unit file for ``systemd`` can be found [here](../samples/dserver.service.sample).

```console
% curl https://raw.githubusercontent.com/mimecast/dtail/master/samples/dserver.service.sample |
    sudo tee /etc/systemd/system/dserver.service >/dev/null
% sudo systemctl daemon-reload
% sudo systemctl enable dserver
```

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

# Register SSH public keys in DTail server

The DTail server now runs as a ``systemd`` service under system user ``dserver``. The system user ``dserver`` however has no permissions to read the SSH public keys from ``/home/USER/.ssh/authorized_keys``. Therefore, no user would be able to establish a SSH session to DTail server. As an alternative path DTail server also checks for public SSH key files in ``/var/run/dserver/cache/USER.authorized_keys``.

It is recommended to execute [update_key_cache.sh](../samples/update_key_cache.sh.sample) periodically to update the key cache. In case you manage your public SSH keys via Puppet you could subscribe the script to corresponding module. Or alternatively just configure a cron job or a systemd timer to run every once in a while.

```console
% curl https://raw.githubusercontent.com/mimecast/dtail/master/samples/update_key_cache.sh.sample |
    sudo tee /var/run/dserver/update_key_cache.sh >/dev/null
% sudo chmod 755 /var/run/dserver/update_key_cache.sh
% curl https://raw.githubusercontent.com/mimecast/dtail/master/samples/dserver-update-keycache.service.sample |
    sudo tee /etc/systemd/system/dserver-update-keycache.service >/dev/null
% curl https://raw.githubusercontent.com/mimecast/dtail/master/samples/dserver-update-keycache.timer.sample |
    sudo tee /etc/systemd/system/dserver-update-keycache.timer >/dev/null
% sudo systemctl daemon-reload
```

# Run DTail client

Now you should be able to use DTail client like outlined in the [Quick Starting Guide](quickstart.md). Also have a look at the [Examples](examples.md).

# Monitor it

To verify that DTail server is up and running and functioning as expected  you should configure the Nagios check [check_dserver.sh](../samples/check_dserver.sh.sample) in your monitoring system. The check has to be executed locally on the server (e.g. via NRPE). How to configure the monitoring system in detail is out of scope of this guide.

```console
% ./check_dserver.sh
OK: DTail SSH Server seems fine
```

