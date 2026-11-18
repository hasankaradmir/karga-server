# ADR-0013: three repositories: karga-cli, karga-server, karga-sdk

**Status:** accepted; ADR-0014 replaces what karga-sdk holds and the compatibility rules.

**Decision:** karga lives in three public repositories:

- **karga-server** (`github.com/hasankaradmir/karga-server`): the server, its adapters (Docker,
  file store, Envoy), the systemd unit and install script, the integration and e2e tests, and the
  design documents (`docs/`).
- **karga-cli** (`github.com/hasankaradmir/karga-cli`): the CLI, its install script, the user
  documentation (README, USAGE).
- **karga-sdk** (`github.com/hasankaradmir/karga-sdk`): the contract between them: the package
  format (resource model, load, lint, render, create) and the API (types, the `Backend` interface,
  the unix socket client and the HTTP handler), plus the example packages (ADR-0014).

The CLI depends only on karga-sdk; the server depends on karga-sdk and never on the CLI; karga-sdk
depends on neither (`depguard` enforces each).

**Reason:** the CLI and the server have different users, platforms (the CLI also runs on macOS to
write packages) and release cadences. One repository made the CLI's tests start a server and let
it import server internals; the split makes the boundary the API.

**Constraints:** A change to the API is made in karga-sdk first, then required by the server and
the CLI (Go pseudo-versions until karga-sdk is tagged). The server's e2e tests build the CLI from a
checkout of its repository (`KARGA_CLI_SRC`, default `../karga-cli`) or take a binary (`KARGA_CLI`).
The history of each repository is the history of the original one for its files.
