# ADR-0018: How values merge

**Status:** accepted

**Decision:** The values a user gives are applied onto the package defaults in this order: values
reused from the previous revision (`--reuse-values`), `-f` files in order, then `--set` and
`--set-string` expressions in the order they were given (one list, `api.ValuesInput.Sets`).

- Maps merge key by key; `null` deletes a key.
- A list replaces the list it overrides as a whole, as in Helm: `-f` with `hosts: [a]` leaves no
  other host. A `--set` that indexes a list edits the list it overrides instead: `hosts[1]=x` keeps
  `hosts[0]` of the defaults or the files, and fills missing elements with `null`.
- `--set` types integers, `true`, `false` and `null`; decimals and numbers with a leading zero stay
  strings. `--set-string` never types.

**Reason:** Applying every `--set` before every `--set-string` ignored the order on the command
line, and `a[1]=x` replaced the default list with `[null, x]`, silently losing `a[0]`. Merging
lists element by element (strategic merge) would need a key per list and still surprise; a whole
list from a file replacing the default one is what Helm users expect.

**Consequences:** `packages.Options` holds the values as `ValueSources`; the CLI keeps `--set` and
`--set-string` in one list to preserve their order.
