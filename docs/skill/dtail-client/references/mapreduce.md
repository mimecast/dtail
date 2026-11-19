# MapReduce queries

Use `dmap` for past data and `dtail --query` for new lines. Quote the entire query with single shell quotes when it contains `$` fields. For example:

```sh
dmap --servers app01,app02 --files '/var/log/dserver/*.log' \
  --query 'from STATS select $hostname,max($goroutines),avg($cgocalls) group by $hostname order by max($goroutines)'
dtail --servers app01 --files '/var/log/app/*.log' \
  --query 'from REQUESTS select service,count(*),avg(latency) group by service interval 10'
dmap --files data.csv --query 'select name,age where age > 40 logformat csv outfile result.csv'
```

## Build a query

`select` is required; other clauses may appear in any order:

```text
from TABLE
select FIELD,AGGREGATION(FIELD),...
where FIELD OP VALUE [and FIELD OP VALUE ...]
group by FIELD,...
order by SELECTED_FIELD | rorder by SELECTED_FIELD
set $VARIABLE = VALUE
interval NUMBER
limit NUMBER
outfile [append] PATH
logformat NAME
```

- `from STATS` selects `|MAPREDUCE:STATS|` lines and the default parser. `from *` selects all MapReduce tables. `from .` or no `from` selects every line; without an explicit `logformat`, this uses the limited `generic` parser.
- `where` conditions are ANDed. Numeric operators: `==`, `!=`, `<`, `<=`, `>`, `>=` (also `=<`, `=>`). String operators: `eq`, `ne`, `contains`, `ncontains`/`lacks`, `hasprefix`, `nhasprefix`, `hassuffix`, `nhassuffix`.
- Aggregations: `count`, `sum`, `min`, `max`, `avg`, `last`, `len`, `percentage` (share of total), `percentile` (rank across groups). `set` can derive a `$variable` from a field or use `md5sum` or `maskdigits`. Without `group by`, the first selected field is the implicit group. Only one copy of any selected expression is allowed; the order field must be selected.
- Results aggregate every 5 seconds by default; `interval N` changes that. `limit N` limits result rows. `outfile PATH` writes a CSV result; `outfile append PATH` appends (writing the header for a missing or empty destination). `dtail --query` without an outfile shows interval results; with an outfile it accumulates results across intervals.
- A dynamic `key=value` field is a **bareword**, e.g. `service` or `latency`. `$hostname`, `$line`, `$server`, `$timezone`, `$timeoffset`, and `$empty` are built-in variables; `$time`, `$date`, `$hour`, `$minute`, `$second`, `$severity`, `$goroutines`, etc. require the default parser. A bareword prefixed with `$` silently resolves empty, with a client warning. Escape a field named like a query keyword or aggregation with backticks.
- `logformat default` understands DTail's own structured lines; `generic` has common variables only; `generickv` reads `field=value|...`; `csv` reads a header on the first line of each file. `mimecast`, `mimecastgeneric`, `custom1`, and `custom2` require an appropriate build or registered parser. A CSV record must fit on one physical line; embedded newlines are unsupported.

Check that the chosen parser exposes the fields in the actual source, and inspect sample lines before trusting an empty or single-group result. Numeric aggregations use only lines with a numeric value for that field. For complete syntax and sparse-field/older-server behavior, read `doc/querylanguage.md` and `doc/logformats.md` at the repository root.
