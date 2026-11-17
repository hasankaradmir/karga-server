# ADR-0013: three repositories: karga, karga-server, karga-sdk

**Status:** accepted

**Decision:** karga lives in three public repositories:

- **karga-server** (`github.com/hasankaradmir/karga-server`): the server, its adapters (Docker,
  SQLite, Envoy), the systemd unit and install script, the integration and e2e tests, and the
  design documents (`docs/`).
- **karga** (`github.com/hasankaradmir/karga`): the CLI, its install script, the user
  documentation (README, USAGE).
- **karga-sdk** (`github.com/hasankaradmir/karga-sdk`): what both use: the resource model, the
  package engine (load, lint, render, create), plans and diffs as the CLI shows them, and the API
  (types, the `Backend` interface, the unix socket client and the HTTP handler), plus the example
  packages.

The CLI depends only on karga-sdk; the server depends on karga-sdk and never on the CLI; karga-sdk
depends on neither (`depguard` enforces each). The server keeps its own types as aliases of the
SDK's, and its plans embed the SDK's plan change with the fields only the executor needs.

**Compatibility:** the API carries a major version (`api.Major`, the `Karga-Api` header). A client
and a server work together when it is the same, whatever their release versions; otherwise every
request is refused with a message naming the side to upgrade. `/v1/version` answers any client.
Changing a request or a result in an incompatible way raises the major.

**Reason:** the CLI and the server have different users, platforms (the CLI also runs on macOS to
write packages) and release cadences. One repository made the CLI's tests start a server and let
it import server internals; the split makes the boundary the API.

**Constraints:** A change to the API is made in karga-sdk first, then required by the server and
the CLI (Go pseudo-versions until karga-sdk is tagged). The server's e2e tests build the CLI from a
checkout of its repository (`KARGA_CLI_SRC`, default `../karga`) or take a binary (`KARGA_CLI`).
The history of each repository is the history of the original one for its files. A later cloud
service lives in a separate, private repository.
