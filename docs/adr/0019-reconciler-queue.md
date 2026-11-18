# ADR-0019: One reconciler, a queue without loss, short repairs

**Status:** accepted

**Decision:** karga-server reconciles one release at a time. Releases to reconcile wait in a queue
that holds each one once, however often it is notified (events are debounced 2 s), and drops none:
the queue is a set the loop empties when it wakes. A repair waits at most a minute for what it
recreated; one that does not come up fails, and the per-resource backoff (10 s doubling to 5 min,
giving up after 5 failures in 10 minutes) tries it again later. Probes and restarts are quick and
need no limit. A release's repair records are forgotten when it is uninstalled.

**Reason:** a repair that waited the deployment timeout (5 minutes) for a container that never came
up held every other release's repairs that long, and the queue dropped work once 256 releases were
waiting. Running repairs of several releases at once would load the Docker daemon, the disk and the
CPU of a single host for little: changes to the host gateway are serialized anyway (ADR-0015), and
the host's resources matter more than how fast a second release is repaired.

**Consequences:** a release that keeps failing to come up is retried at growing intervals instead of
blocking the loop; `karga status` shows what was given up on.
