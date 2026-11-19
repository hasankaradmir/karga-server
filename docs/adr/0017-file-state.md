# ADR-0017: State in YAML files

**Status:** accepted; supersedes ADR-0004.

**Decision:** karga-server keeps its state in YAML files under `$KARGA_HOME/state`, one directory
per release:

```text
state/version.yaml                         format version (1)
state/next-operation.yaml                  the next operation id
state/releases/<release>/release.yaml      the release
state/releases/<release>/revisions/<n>.yaml
state/releases/<release>/resources/<kind>_<name>.yaml
state/operations/<release>.yaml            its operations, one YAML document each
```

- Every file is replaced atomically: written beside it, synced, renamed, and the directory synced.
- A transaction (`StateStore.InTx`) keeps its writes in memory and applies them at the end, the
  release file last. Until `release.yaml` is written the release still points at what it had, so a
  crash in between leaves at most files nothing refers to.
- One process writes (one karga-server per `$KARGA_HOME`); the release lock (ADR-0015) orders the
  writes of a release, and one store-wide mutex orders the files several releases share.
- A release keeps its newest 200 operations; operations outlive the release, like an audit log.
- Nothing on disk is JSON: the state, the gateway settings (`gateway-settings/*.yaml`), what a release exposes
  (`proxies/<release>/expose.yaml`), Envoy's configuration (`envoy.yaml`, `lds.yaml`, `cds.yaml`)
  and the manifest inside a volume backup are YAML. JSON stays on the wire between the CLI and the
  server, and as the canonical input of digests computed in memory.
- A newer format version is refused. The SQLite database of earlier builds (`karga.db`) is refused
  too and not converted: no release used it.

**Reason:** The state is small (tens of releases, at most `--history-max` revisions each) and karga
runs one server per host (ADR-0012), so a database brings little: the files are readable and
diffable, a backup is a copy of `$KARGA_HOME` with the server stopped (Config and Secret files are
already files there), there are no schema migrations, and the server loses the SQLite driver.

**Consequences:** Atomicity across files is by write order, not by a database transaction.
Listing operations reads every release's file; with 200 per release this stays small. Changing
the layout needs a format version and code that reads the older one.
