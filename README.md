# backup-service

A small Go CLI for scheduled logical PostgreSQL backups to a mounted disk. Kubernetes schedules runs; PostgreSQL's version-matched tools perform the dumps. No API server access, HTTP service, or in-process scheduler is needed.

## Scope

- Explicit targets and file-based passwords; PostgreSQL 14, 16, and 17 clients.
- An expected disk marker guards the backup destination.
- Retention keeps successful backup sets, not a fixed number of days.
- Logical backups are not continuous WAL archiving or point-in-time recovery. A USB disk is not an independent off-site backup.

## Local use

Requires Go **1.27.2 or newer** (the module can download its required toolchain automatically).

```sh
go test ./...
go build -o bin/backup-service ./cmd/backup-service
docker build -t postgres-backup:test .
bash tests/integration/run.sh
./bin/backup-service validate --config examples/config.yaml
./bin/backup-service backup --config examples/config.yaml
```

The integration runner requires a **nonroot user with local Docker access**. It exercises PostgreSQL 14/16/17 backup and clean-cluster restore (globals, database creation, data, sequences, and grants), successful-set retention, missing-disk rejection, and overlapping filesystem locks; artifacts remain under `/tmp/opencode` or `RUNNER_TEMP`. Docker access is highly privileged: use a trusted development/CI host. The container uses the host UID for test bind mounts; Kubernetes uses UID/GID 10001.

Install matching PostgreSQL clients at `/usr/lib/postgresql/{14,16,17}/bin` before running backups locally. The example expects `/backups` to be a verified mounted backup directory, its `.backup-disk-id` to contain `homelab-backups-01`, and a password file at `/run/credentials/postgres-a/password`. Edit the config for your environment; validation is not proof that a backup is restorable.

**Security:** PostgreSQL 14 reaches end of life in November 2026. Plan an upgrade; bundling its client does not extend upstream security support.

- [Configuration example](examples/config.yaml)
- [Deployment and GitOps](docs/deployment.md)
- [Operations and disk setup](docs/operations.md)
- [Restore drill](docs/restore.md)
- [Disaster recovery](docs/disaster-recovery.md)
- [Architecture and Go learning notes](docs/architecture.md)

The Kustomize examples use a placeholder image. Pushes to `main` verify and publish `ghcr.io/<repo>:<full-commit-sha>` for amd64/arm64, then dispatch the same SHA to the GitOps repository. Semver releases are only a future idea: `v*` tags do not publish images. See [deployment](docs/deployment.md) for the dispatch setup. Automated restore drills are currently commented out in the workflows; run them manually on your actual deployment architecture before relying on backups.
