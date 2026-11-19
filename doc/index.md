DTail Documentation
===================

## Installation and Usage

* To get a taste, please have a look at the [Usage Examples](./examples.md).
* Follow the [Quick Starting Guide](./quickstart.md) if you are looking into DTail your first time.
* For a more sustainable setup, please follow the [Installation Guide](./installation.md).
* To configure `dserver`, see the [Server Configuration Guide](./server-configuration.md) — read permissions, connection limits, journal reads, shared reads and scheduled jobs.

## Advanced topics

* The [DTail Query Language](./querylanguage.md) is the starting point to dig deeper into DTail's own SQL-like mapreduce language for extraction/aggregation stats from log files.
* The [Interactive Query Reload](./interactive-query.md) documentation describes `:reload`, `:show`, `:help`, `:quit`, the reload flags per mode, capability fallback on mixed-version servers, and session reuse semantics.
* [Log Formats](./logformats.md) explains how to create your own custom log format for use with mapreduce queries.
* The [Auth-Key Fast-Reconnect](./auth-key-fast-reconnect.md) documentation explains how clients register a temporary public key with `dserver` to skip the full SSH authentication on reconnects.
* The [serverless mode](./examples.md#how-to-use-the-dtail-serverless-mode) (see the Usage Examples) works without any `dserver` and without SSH networking.
* Check out the [Testing Guide](./testing.md) for unit and integration testing.
