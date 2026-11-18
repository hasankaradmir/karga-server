# ADR-0004: SQLite state

**Status:** superseded by ADR-0017 (state in YAML files).

**Decision:** Release state (releases, immutable revisions, applied resources, operations) lives in a
SQLite database in `$KARGA_HOME` (pure-Go driver, WAL, schema migrations with `user_version`).

**Reason:** Enough for one host and operationally simple: one file, no server, easy to back up
(`karga state backup`).
