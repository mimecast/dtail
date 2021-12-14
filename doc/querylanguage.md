DTail Query Language
====================

The query language allows you to run mapreduce queries on log files. This page intends to be a reference to the language.

## Prerequisites

For this to work, DTail needs to understand your log format. DTail already understands its own log format. You can have a look at all examples of the [examples](./examples.md) page using `-query` (these would be all examples of the `dmap` command, and some examples using the `dtail` command).

DTail also ships with a generic log format, which only allows you to run very basic queries. Check out the [log format](./logformats.md) documentation for this. To implement your own log format, please also check out the log format documentation.

## The language

These are the fundamental types of the query language:

```shell
NUMBER := A whole number (e.g. 42)
FLOAT := A float number, e.g. 3.14
STRING := A quoted string, e.g. "foo"
FIELD := BAREWORD|VARIABLE
BAREWORD := A bare string without quotes, e.g. foo. This usually contains a value
            extracted from a log line.
VARIABLE := Like a bareword, but with a $ prefix, e.g. $foo. This usually contains
            a special value set by DTail itself (not necessary from the log line).
```

This is the overall structure of a query:

```shell
QUERY := from TABLE
         select SELECT1[,SELECT2...]
         [where CONDITION1[,CONDITION2...]]
         [group by FIELD1[,FIELD2...]]
         [order|rorder by ORDERFIELD]
         [set SET1,[,SET2...]]
         [interval NUMBER]
         [limit NUMBER]
         [outfile STRING]
         [logformat LOGFORMAT]
```

Whereas....

```shell
TABLE := The mapreduce table name, e.g. STATS in MAPREDUCE:STATS
SELECT := FIELD|AGGREGATION(FIELD)
CONDITION := ARG1 OPERATOR ARG2
ARG := FIELD|FLOAT|STRING
OPERATOR := FLOATOPERATOR|STRINGOPERATOR
FLOATOPERATOR := One of: == != < <= > >=
STRINGOPERATOR := eq|ne|contains|ncontains|lacks|hasprefix|nhasprefix|hassuffix|nhassuffix
ORDERFIELD := FIELD|AGGREGATION(FIELD)
SET := VARIABLE = FLOAT|STRING|FIELD|FUNCTION(FIELD)
LOGFORMAT := default|generic|generickv|...
AGGREGATION := count|sum|min|max|avg|last|len
FUNCTION := md5sum|maskdigits
```

*Notes:*

* `lacks` is an alias for `ncontains` (not contains)
* `rorder` stands for reverse order and is the inverse of `order`
* Available fields (variables and barewords) vary from the log format used. Check out the [log format](./logformats.md) documentation for more information.
