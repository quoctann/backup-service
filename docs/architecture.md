# Architecture and Go learning notes

The pipeline is deliberately small:

```text
Kubernetes CronJob → Go CLI → version-matched PostgreSQL subprocesses
                         ↘ verified mounted disk → completed backup sets
```

Configuration describes targets, not Kubernetes discovery. The process needs outbound database/DNS access and filesystem permissions, but no Kubernetes client, token, RBAC, ingress, or HTTP server. Kubernetes supplies scheduling, bounded runtime, resource isolation, and Job history. The Go process owns validating inputs, running tools, detecting failures, and making completed sets distinguishable from partial ones.

The following invariants explain the implementation and provide useful research topics:

- **Typed configuration at the boundary:** decode a small schema, reject invalid target names/paths/versions/timeouts, and validate before filesystem writes. Supported client paths are explicit `/usr/lib/postgresql/N/bin`; configuration must not become an arbitrary command path.
- **`os/exec`, not a shell:** pass arguments separately to `exec.CommandContext`. SQL names and filenames should never be concatenated into shell scripts. Keywords: argument injection, environment minimization, subprocess stderr redaction.
- **Timeouts and cancellation:** derive a context for each target, propagate process failures, and account for the whole Job deadline. Research Unix signals, process groups, and child cleanup: cancelling the Go process and terminating all descendants are related but distinct problems.
- **Password handling:** consume a mounted password file without logging it. PostgreSQL password-file syntax has escaping and permission requirements; do not assume an arbitrary password can be interpolated into a connection string or `.pgpass` unchanged. Do not include role passwords in globals exports.
- **Filesystem identity versus mount identity:** a marker protects against many operator mistakes, but is not proof that the intended block device is mounted. The host must verify UUID/mountpoint. HostPath is not a portable storage abstraction; it pins scheduling and failure recovery to that node/disk.
- **Transactional publication:** write into a staging area on the destination filesystem, verify subprocess exit statuses and checksums, then publish completion only after all required artifacts are durable. Research `fsync`, directory sync, atomic rename, and same-filesystem constraints. Atomic visibility is not the same as power-loss durability.
- **Locking:** advisory disk locks can serialize cooperating writers across manual and scheduled runs. Research `flock`, lock lifetime, file descriptors, and why stale filename-based locks are not equivalent. CronJob `Forbid` alone does not cover manual Jobs.
- **Retention after success:** prune only eligible completed sets for the same target, with path boundaries and symlink handling reviewed. Never sacrifice the last good backup to make room for an unverified run. Failed artifacts need an explicit, separate cleanup policy.
- **Errors and observability:** distinguish validation, connection, dump, storage, and cleanup failures. Return a failing process status if required work fails; log target/run identifiers without secrets. Per-target freshness is more useful than merely seeing a successful CronJob.

Useful tests include schema boundaries, version-to-client selection, redaction, cancellation, missing/wrong disk markers, disk-full/write failures, incomplete publication, checksum failures, retention preserving successful history, and restoration against disposable PostgreSQL servers. Filesystem fakes alone cannot prove real disk durability or a real restore. Integration/restore tests complement unit tests; never run destructive tests against production.
