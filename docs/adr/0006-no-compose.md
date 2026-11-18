# ADR-0006: No Docker Compose as the package format

**Status:** accepted

**Decision:** karga defines its own small resource model (Workload, Service, Volume, Network, Config,
Secret) instead of using Compose files.

**Reason:** Runtime independence and a model karga can diff, plan and roll back precisely.
