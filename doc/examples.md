Examples
========

This page demonstrates the primary usage of DTail. Please also see `--help` for more available options.

## Table of contents

* How to use `dtail` to follow logs
* How to use `dtail` to aggregate logs
* How to use `dcat`
* How to use `dgrep`
* How to use `dmap`
* How to use the DTail serverless mode

## How to use `dtail`

### Following logs

The following example demonstrates how to follow logs of multiple servers at once. The server list is provided as a flat text file. The example filters all records containing the string `INFO`. Any other Go compatible regular expression can also be used instead of `INFO`.

```shell
% dtail --servers serverlist.txt --grep INFO --files "/var/log/dserver/*.log"
```

Hint: you can also provide a comma separated server list, e.g.: `servers server1.example.org,server2.example.org:PORT,...`

![dtail](dtail.gif "Tail example")

Hint: You can also use the shorthand version (omitting the `--files`)

```shell
% dtail --servers serverlist.txt --grep INFO "/var/log/dserver/*.log"
```

### Aggregating logs

To run ad-hoc map-reduce aggregations on newly written log lines you must add a query. The following example follows all remote log lines and prints out every few seconds the result to standard output.

Hint: To run a map-reduce query across log lines written in the past, please use the `dmap` command instead.

```shell
% dtail --servers serverlist.txt \
    --files '/var/log/dserver/*.log' \
    --query 'from STATS select sum($goroutines),sum($cgocalls),last($time),max(lifetimeConnections)'
```

Beware: For map-reduce queries to work, you have to ensure that DTail supports your log format. Check out the [query language](./querylanguage.md) and [log formats](./logformats.md) for more information.

![dtail-map](dtail-map.gif "Tail mapreduce example")

Hint: You can also use the shorthand version:

```shell
% dtail --servers serverlist.txt \
    --files '/var/log/dserver/*.log' \
    'from STATS select sum($goroutines),sum($cgocalls),last($time),max(lifetimeConnections)'
```
Here is another example:

```shell
% dtail --servers serverlist.txt \
    --files '/var/log/dserver/*.log' \
    --query 'from STATS select $hostname,max($goroutines),max($cgocalls),$loadavg,lifetimeConnections group by $hostname order by max($cgocalls)'
```

![dtail-map](dtail-map2.gif "Tail mapreduce example 2")

You can also continuously append the results to a CSV file by adding `outfile append filename.csv` to the query:

```shell
% dtail --servers serverlist.txt \
    --files '/var/log/dserver/*.log' \
    --query 'from STATS select ... outfile append result.csv'
```

## How to use `dcat`

The following example demonstrates how to cat files (display the full content of the files) of multiple servers at once.

As you can see in this example, a DTail client also creates a local log file of all received data in `~/log`. You can also use the `noColor` and `-plain` flags (this all also work with other DTail commands than `dcat`).

```shell
% dcat --servers serverlist.txt --files /etc/hostname
```

![dcat](dcat.gif "Cat example")

Hint: You can also use the shorthand version:

```shell
% dcat --servers serverlist.txt /etc/hostname
```

## How to use `dgrep`

The following example demonstrates how to grep files (display only the lines which match a given regular expression) of multiple servers at once. In this example, we look after some entries in `/etc/passwd`  This time, we don't provide the server list via an file but rather via a comma separated list directly on the command line. We also explore the `-before`, `-after` and `-max` flags (see animation).

```shell
% dgrep --servers server1.example.org:2223 \
    --files /etc/passwd \
    --regex nologin
```

Generally, `dgrep` is also a very useful way to search historic application logs for certain content.

![dgrep](dgrep.gif "Grep example")

Hint: `-regex` is an alias for `-grep`.

## How to use `dmap`

To run a map-reduce aggregation over logs written in the past, the `dmap` command can be used. The following example aggregates all map-reduce fields `dmap` will print interim results every few seconds. You can also write the result to an CSV file by adding `outfile result.csv` to the query.

```shell
% dmap --servers serverlist.txt \
    --files '/var/log/dserver/*.log'
    --query 'from STATS select $hostname,max($goroutines),max($cgocalls),$loadavg,lifetimeConnections group by $hostname order by max($cgocalls)'
```

Remember: For that to work, you have to make sure that DTail supports your log format. You can either use the ones already defined in `internal/mapr/logformat` or add an extension to support a custom log format. The example here works out of the box though, as DTail understands its own log format already. 

![dmap](dmap.gif "DMap example")

## How to use the DTail serverless mode

Until now, all examples so far required to have remote server(s) to connect to. That makes sense, as after all DTail is a *distributed* tool. However, there are circumstances where you don't really need to connect to a server remotely. For example, you already have a login shell open to the server an all what you want is to run some queries directly on local log files.

The serverless mode does not require any `dserver` up and running and therefore there is no networking/SSH involved. 

All commands shown so far also work in a serverless mode. All what needs to be done is to omit a server list. The DTail client then starts in serverless mode.

### Serverless map-reduce query

The following `dmap` example is the same as the previously shown one, but the difference is that it operates on a local log file directly:

```shell
% dmap --files /var/log/dserver/dserver.log
    --query 'from STATS select $hostname,max($goroutines),max($cgocalls),$loadavg,lifetimeConnections group by $hostname order by max($cgocalls)'
```

As a shorthand version the following command can be used:

```shell
% dmap 'from STATS select $hostname,max($goroutines),max($cgocalls),$loadavg,lifetimeConnections group by $hostname order by max($cgocalls)' /var/log/dsever/dserver.log
```

You can also use a file input pipe as follows:

```shell
% cat /var/log/dserver/dserver.log | \
    dmap 'from STATS select $hostname,max($goroutines),max($cgocalls),$loadavg,lifetimeConnections group by $hostname order by max($cgocalls)'
```

### Aggregating CSV files

In essence, this works exactly like aggregating logs. All files operated on must be valid CSV files and the first line of the CSV must be the header. E.g.:

```shell
% cat example.csv
name,lastname,age,profession
Michael,Jordan,40,Basketball player
Michael,Jackson,100,Singer
Albert,Einstein,200,Physician
% dmap --query 'select lastname,name where age > 40 logformat csv outfile result.csv' example.csv
% cat result.csv
lastname,name
Jackson,Michael
Einstein,Albert
```

DMap can also be used to query and aggregate CSV files from remote servers.

### Other serverless commands

The serverless mode works transparently with all other DTail commands. Here are some examples:

```shell
dtail /var/log/dserver/dserver.log
```

```shell
dtail --logLevel trace /var/log/dserver/dserver.log
```

```shell
dcat /etc/passwd
```

```shell
dcat --plain /etc/passwd > /etc/test
# Should show no differences.
diff /etc/test /etc/passwd 
```

```shell
dgrep --regex ERROR --files /var/log/dserver/dsever.log
```

```shell
dgrep --before 10 --after 10 --max 10 --grep ERROR /var/log/dserver/dsever.log
```
