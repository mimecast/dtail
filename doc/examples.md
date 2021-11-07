Examples
========

This page demonstrates the primary usage of DTail. Please also see ``dtail --help`` for more available options.

# How to use ``dtail``

## Tailing logs

The following example demonstrates how to follow logs of multiple servers at once. The server list is provided as a flat text file. The example filters all records containing the string ``INFO``. Any other Go compatible regular expression can be used instead of ``INFO``.

```shell
% dtail --servers serverlist.txt --grep INFO --files "/var/log/dserver/*.log"
```

Hint: you can also provide a comma separated server list, e.g.: `--servers server1.example.org,server2.example.org:PORT,...`.

![dtail](dtail.gif "Tail example")

Hint: You can also use the shorthand version (omitting the `--files`):

```shell
% dtail --servers serverlist.txt --grep INFO "/var/log/dserver/*.log"
```

## Aggregating logs

To run ad-hoc MapReduce aggregations on newly written log lines you must add a query. The following example follows all remote log lines and prints out every few seconds the top 10 servers with the most average free memory. To run a MapReduce query across log lines written in the past, please use the ``dmap`` command instead.

```shell
% dtail --servers serverlist.txt \
    --files '/var/log/dserver/*.log' \
    --query 'from STATS select sum($goroutines),sum($cgocalls),last($time),max(lifetimeConnections)'
```

For MapReduce queries to work, you have to ensure that DTail supports your log format. You can either use the ones already defined in ``internal/mapr/log format`` or add an extension to support a custom log format.

![dtail-map](dtail-map.gif "Tail mapreduce example")

Hint: You can also use the shorthand version:

```shell
% dtail --servers serverlist.txt \
    --files '/var/log/dserver/*.log' \
    'from STATS select sum($goroutines),sum($cgocalls),last($time),max(lifetimeConnections)'
```
Here is yet another example:

```shell
% dtail --servers serverlist.txt \
    --files '/var/log/dserver/*.log' \
    --query 'from STATS select $hostname,max($goroutines),max($cgocalls),$loadavg,lifetimeConnections group by $hostname order by max($cgocalls)'
```

![dtail-map](dtail-map2.gif "Tail mapreduce example 2")

# How to use ``dcat``

The following example demonstrates how to cat files (display the full content of the files) of multiple servers at once.

As you can see in this example, a DTail client also creates a local log file of all received data in `~/log`. You can also use the `-noColor` and `-plain` flags (they also work with other commands than `dcat`).

```shell
% dcat --servers serverlist.txt --files /etc/hostname
```

![dcat](dcat.gif "Cat example")

Hint: You can also use the shorthand version:

```shell
% dcat --servers serverlist.txt /etc/hostname
```

# How to use ``dgrep``

The following example demonstrates how to grep files (display only the lines which match a given regular expression) of multiple servers at once. In this example, we look after some entries in ``/etc/passwd``.  This time, we don't provide the server list via an file but rather via a comma separated list directly on the command line. We also explore the `-before`, `-after` and `-max` flags.

```shell
% dgrep --servers server1.example.org:2223 \
    --files /etc/passwd \
    --regex nologin
```

![dgrep](dgrep.gif "Grep example")

Hint: `-regex` is an alias for `-grep`.

# How to use ``dmap``

To run a MapReduce aggregation over logs written in the past, the ``dmap`` command has be used. Fhe following example aggregates all MapReduce fields ``dmap`` will print interim results every few seconds. You can also write the result to an CSV file by adding `outfile result.csv` to the query.

```shell
% dmap --servers serverlist.txt \
    --files '/var/log/dserver/*.log'
    --query 'from STATS select $hostname,max($goroutines),max($cgocalls),$loadavg,lifetimeConnections group by $hostname order by max($cgocalls)'
```

Remember: For that to work, you have to make sure that DTail supports your log format. You can either use the ones already defined in ``internal/mapr/logformat`` or add an extension to support a custom log format. Te example here works out of the box though, as DTail understands its own log format already. 

![dmap](dmap.gif "DMap example")
