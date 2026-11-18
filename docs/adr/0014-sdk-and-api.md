# ADR-0014: what karga-sdk holds, and how the API changes

**Status:** accepted; replaces the package list and the compatibility part of ADR-0013.

**Decision:** karga-sdk holds only the contract between the CLI and the server:

- **The package format:** the resource model (`resource`), and loading, linting, rendering and
  creating packages (`packages`). The CLI renders offline (`lint`, `template`, `show`); the server
  renders what it deploys. Both must agree, so the code is shared.
- **The API** (`api`): the request and result types, the `Backend` interface, the unix socket
  client and the HTTP handler.

Code both happen to need (a logger, file helpers, plan display) is not a reason to put it in the
SDK: each repository keeps its own. The server has its own request, result, plan and state types;
only `internal/server/backend.go` knows the API and maps between them (`depguard` forbids
`karga-sdk/api` in `internal/application`, `internal/ports` and `internal/domain`).

**Versions:** the API version is `Major.Minor` (`api.Version`, the `Karga-Api` header).

- A client and a server work together when the major is the same, whatever their release
  versions. Another major is refused on every request with a message naming the side to upgrade;
  `/v1/version` answers any client.
- A minor adds fields or endpoints. A server refuses a request with a field or an endpoint it does
  not know as incompatible ("upgrade karga-server"), instead of ignoring it: a newer CLI never has
  an option silently dropped. A client ignores result fields it does not know.
- Renaming or removing a field, or changing its meaning, raises the major.

**Errors:** the server sends an error kind with every error: not found, conflict, invalid,
incompatible. The CLI maps them to exit codes: 1 other failure, 2 usage, 3 server unavailable or
incompatible, 4 not found, 5 conflict, 6 `status` unhealthy. Scripts may rely on the codes and on
the shapes of `-o json` and `-o yaml` (the API's views), which only grow within a major.

**Reason:** an SDK that collects whatever is shared couples the CLI and the server through
internals and makes every refactor an SDK release. Keeping it to the wire and the package format
makes its surface the compatibility promise, and lets the server change its own model freely.

**Consequences:** the server maps every request and result (`backend.go`); adding an API field
means an SDK change, then the mapping. A newer CLI against an older server fails loudly for new
options until the server is upgraded.
