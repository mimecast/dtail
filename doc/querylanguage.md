DTail Query Language
====================

The query language allows you to run mapreduce queries on log files. This page intends to be a reference to the language.

## Prerequisites

For this to work, DTail needs to understand your log format. DTail already understands its own log format. You can have a look at all examples of the [examples](./examples.md) page using `-query` (these would be all examples of the `dmap` command, and some examples using the `dtail` command).

DTail also ships with a generic log format, which only allows you to run very basic queries. Check out the [log format](./logformats.md) documentation for this. To implement your own log format, please also check out the log format documentation.

## The complete language

```shell
QUERY :=
    select SELECT1[,SELECT2...]
    from TABLE
    [where COND1[,COND2...]]
    [group by GROUPFIELD1[,GROUPFIELD2...]]
    [order|rorder by ORDERFIELD]
    [interval SECONDS]
    [limit NUM]
    [outfile "FILENAME.csv"]
    [logformat LOGFORMAT]
SELECT := FIELD|AGGREGATION(FIELD)
TABLE := The mapreduce table name, e.g. WRITE in MAPREDUCE:WRITE
AGGREGATION := count|sum|min|max|avg|last|len
COND := ARG1 OPERATOR ARG2
ARG := This is either
    a string: "foo bar"
    a float number: 3.14
    a bareword e.g.: responsecode
    a field or a $variable
OPERATOR := This is one of ...
    Floating point operators:
        == != < <= > >=
    String operators:
        eq ne contains lacks (lacks is the opposite of contains, e.g. "not contains")
GROUPFIELD := bareword|$variable       
ORDERFIELD := This must be a AGGREGATION(FIELD) or FIELD which was specified in
              select clause already.
LOGFORMAT := The name of the log format implementation. It's 'default' by default.
```

Note, that the available fields and variables vary from the log format used. There is also a subtle difference between a field and a variable. Check out the [log format](./logformats.md) documentation for more information.
