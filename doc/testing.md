DTail Testing Guide
===================

Currently, there are 3 different ways of how DTail can be tested.

1. Unit tests (automatic)
2. Integration tests (automatic)
3. Semi-manual tests with DTail server instances running in Docker.

## Unit tests

To run all the unit tests simply run the following make command at the top level source directory:

```shell
% make test
```

It will run unit tests for each source directory one after another and abort immediately when an error occurs.

## Integration tests

Other than the unit tests, which only test the internal code, the integration tests will run a set of DTail commands externally and thus simulating common end user use cases.

This means, that you will need to compile all DTail binaries prior to running these tests:

```shell
# Not mandatory, but pest practise to delete all previously compiled binaries
% make clean 
# Now compile all the binaries (dtail, dcat, dmap, dserver...)
% make build
```

The integration tests can be enabled setting the following environment variable:

```
% export DTAIL_INTEGRATION_TEST_RUN_MODE=yes
```

To run the integration test together with all the unit tests simply run `make test` in the top level source tree. In case you only want to run the integration tests without the normal unit tests, then just do:

```shell
% go clean -testcache
% go test -race -v ./integrationtests
```

## Semi-manual tests with DTail server instances running in Docker

### Requirements 

This assumes, that you have Docker up and running on your system. The following has been tested only on Fedora Linux. You might need to do some extra setup if you want to run this on Docker for Mac/Windows.

This also assumes, that you have compiled all the DTail binaries already (with `make` in the top level source directory.

### Building DTail server Docker image

To build the `dserver` Docker image run:

```
% make -C docker
make: Entering directory '/home/paul/git/dtail/docker'
cp ../integrationtests/mapr_testdata.log .
cp ../dserver .
docker build . -t dserver:develop
Sending build context to Docker daemon  13.84MB
Step 1/11 : FROM fedora:34
---> dce66322d647
Step 2/11 : RUN mkdir -p /etc/dserver /var/run/dserver/ /var/log/dserver
.
.
.
Successfully built b44e92f7c066
Successfully tagged dserver:develop
rm ./dserver
rm ./mapr_testdata.log
make: Leaving directory '/home/paul/git/dtail/docker'
```

### Starting a DTail server farm

To spin up 10 instances of `dserver` run:

```shell
% make -C docker spinup
make: Entering directory '/home/paul/git/dtail/docker'
./spinup.sh 10
Creating dserver-serv0
b34e05f33deb62df628dc00b4ea4e7f1da0be73fdba7895d182fff6dd5b48b2f
Creating dserver-serv1
bb303acfb47a620e3f58f7c0539102db1d286ed2104d091eefe52710c2bba86e
Creating dserver-serv2
.
.
.
```

Now, have a look at the containers:

```shell
% docker ps
CONTAINER ID   IMAGE             COMMAND                  CREATED         STATUS         PORTS                                       NAMES
3873f7e69620   dserver:develop   "/usr/local/bin/dser…"   3 minutes ago   Up 3 minutes   0.0.0.0:2232->2222/tcp, :::2232->2222/tcp   dserver-serv9
412f79d0f716   dserver:develop   "/usr/local/bin/dser…"   3 minutes ago   Up 3 minutes   0.0.0.0:2231->2222/tcp, :::2231->2222/tcp   dserver-serv8
1ff2a52ae614   dserver:develop   "/usr/local/bin/dser…"   3 minutes ago   Up 3 minutes   0.0.0.0:2230->2222/tcp, :::2230->2222/tcp   dserver-serv7
6d1c78eceedd   dserver:develop   "/usr/local/bin/dser…"   3 minutes ago   Up 3 minutes   0.0.0.0:2229->2222/tcp, :::2229->2222/tcp   dserver-serv6
073fe345235f   dserver:develop   "/usr/local/bin/dser…"   3 minutes ago   Up 3 minutes   0.0.0.0:2228->2222/tcp, :::2228->2222/tcp   dserver-serv5
63fd7f2393f1   dserver:develop   "/usr/local/bin/dser…"   3 minutes ago   Up 3 minutes   0.0.0.0:2227->2222/tcp, :::2227->2222/tcp   dserver-serv4
32c9f940312c   dserver:develop   "/usr/local/bin/dser…"   3 minutes ago   Up 3 minutes   0.0.0.0:2226->2222/tcp, :::2226->2222/tcp   dserver-serv3
91b137c2dd19   dserver:develop   "/usr/local/bin/dser…"   3 minutes ago   Up 3 minutes   0.0.0.0:2225->2222/tcp, :::2225->2222/tcp   dserver-serv2
bb303acfb47a   dserver:develop   "/usr/local/bin/dser…"   3 minutes ago   Up 3 minutes   0.0.0.0:2224->2222/tcp, :::2224->2222/tcp   dserver-serv1
b34e05f33deb   dserver:develop   "/usr/local/bin/dser…"   3 minutes ago   Up 3 minutes   0.0.0.0:2223->2222/tcp, :::2223->2222/tcp   dserver-serv0
```

### Connecting to all 10 DTail servers

Have a look at `docker/Makefile` for some pre-defined commands. But one example would be:

```shell
make -C docker dtail
```

This will launch `dtail` and follow the `dserver` log files of all 10 containers.

### Stopping all Docker containers again

Just run this:

```shell
make -C docker spindown
```
