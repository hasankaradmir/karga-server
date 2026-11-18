# ADR-0015: concurrency and crash consistency

**Status:** accepted

**Decision:**

- **Release lock.** An operation holds its release from reading its state (planning) to recording
  its end, inside the server process. A user's command waits for it (bounded by the request's
  context: the client's, `api.ClientContext`, though the operation itself runs on one its going away
  does not cancel); the reconciler tries it and skips a busy release. Entries of idle releases are
  freed.
- **Gateway lock.** Changes to the host gateway (what releases expose, its settings, the pre-flight
  check of host ports and routes) run under one host-wide lock, and the check runs again under it
  just before an exposure is written, so two releases cannot claim the same port or route.
- **Operations.** Every mutating command records an operation, with its options (delete volumes,
  strategy, timeout, keep history), before it changes anything, and its steps as it goes. An
  operation whose process is gone blocks the release until it is resumed or aborted (`--force`).
- **Resume.** When karga-server starts, it takes the lock of every release it left an operation
  running on before it answers requests, then resumes those operations in the background, with their
  recorded options (a command on such a release waits for its resume; the others are served at
  once; a runtime that does not answer yet is waited for): install, upgrade and rollback deploy their recorded
  revision again (the plan skips what is done), uninstall runs again. A resume never deletes more
  than the interrupted command would have.
- **State writes.** A transaction (`StateStore.InTx`) covers the writes of one step of one release
  and is applied file by file, the release file last (ADR-0017). Runtime changes are never inside a
  transaction: the state records what was applied after it was applied, and a repair or a resume
  converges from whatever the runtime holds.
- **Config and Secret files** are written under the revision's directory in `$KARGA_HOME` before
  the containers that mount them start, and updated in place: the bind mount is the runtime's
  contract, so running containers keep seeing them.

**Reason:** one server owns the state and the runtime (ADR-0012), so locks in its memory are
enough; recording options makes a resume do what the user asked, not what is safest to guess.

**Consequences:** two servers on one `$KARGA_HOME` are refused (lock file). A crash between a
runtime change and its record leaves a runtime ahead of the state; the next plan or the reconciler
sees the difference and repairs it.
