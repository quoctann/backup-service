# Restore drill (never automatic)

**Restore only to a new, isolated PostgreSQL instance first.** Never point these commands at production or restore over a database in use. Logical dumps can contain executable SQL: trust the backup source, inspect globals, and isolate the restore account and network. A read-only HDD prevents accidental artifact changes, not dangerous SQL on the destination.

## 1. Select and verify a completed set

Identify the target, successful completion record, source server major version, database archive, globals SQL, and metadata. Do not select an incomplete run or guess based only on the newest directory name. Keep an untouched copy before recovery work.

The current schema is `manifest.json` with `schemaVersion: 1`, target, major version, timestamps, and a `files` array containing `name`, `size`, `sha256`, and (for database dumps) `database`. A completed set contains `globals.sql` and `db-000.dump`, `db-001.dump`, etc. Use the manifest's `database` mapping rather than assuming the first file is `app`. Directories named `.partial-*` are not restore candidates. Verify the exact naming against your running release.

On a trusted workstation/host with **jq and sha256sum** (not assumed installed in the runtime image), change into a selected completed set or its verified copy. This rejects unsupported schemas and unsafe filenames, then verifies every listed artifact:

```sh
cd /backups/SELECTED_COMPLETED_SET
set -eu
CHECKS=$(mktemp)
trap 'rm -f "$CHECKS"' EXIT
jq -er '
  if .schemaVersion != 1 or (.files | length) < 2 then
    error("unsupported or incomplete manifest")
  elif all(.files[];
    (.name | test("^(globals[.]sql|db-[0-9]{3,}[.]dump)$")) and
    (.sha256 | test("^[a-f0-9]{64}$"))) then
    .files[] | .sha256 + "  " + .name
  else error("unsafe checksum entry") end
' manifest.json > "$CHECKS"
sha256sum --check "$CHECKS"
```

For a different manifest version, stop and consult that release's verifier instead of silently skipping verification. Also inspect sizes, target, timestamps, and version metadata. The manifest itself is not covered by its own artifact hashes: retain a separately trusted copy or signed inventory for stronger assurance. Missing checksums mean integrity is unproven; investigate or select another set. Checksums detect corruption, not malicious replacement without a trusted reference.

## 2. Prepare an isolated destination and utility Pod

Provision a fresh disposable PostgreSQL instance with a separate PVC, no application traffic, and a known DNS name. Prefer the **same major version** as the source for an initial recovery drill; install required extension binaries/libraries and appropriate locales first. Cross-major upgrades need a separately tested migration plan. Use the source-matched client tools throughout (14, 16, or 17); do not rely on whatever `pg_restore` appears first on PATH.

Supply an isolated restore administrator's password file out of band:

```sh
kubectl -n backup create secret generic restore-drill-credentials \
  --from-file=password=/secure/local/isolated-restore-password
```

Edit `examples/restore/restore-pod.yaml` to use your pinned published image, then:

```sh
kubectl apply -f examples/restore/restore-pod.yaml
kubectl -n backup wait --for=condition=Ready pod/backup-restore --timeout=120s
kubectl -n backup exec -it backup-restore -- /bin/sh
```

This optional Pod mounts the HDD **read-only**, has no API token, and expires after four hours. Its credential is only for the isolated destination, not a production credential. Restrict network reachability where possible. Review the manifest before applying it; it is deliberately not in the backup Kustomization.

## 3. Review globals and preserve database metadata

In the Pod, substitute actual values. The hostname below is deliberately an isolated restore destination:

```sh
set -eu
PG_BIN=/usr/lib/postgresql/14/bin
export PGHOST=postgres-restore.restore-drill.svc.cluster.local
export PGPORT=5432 PGUSER=restore_admin PGDATABASE=postgres
export PGCONNECT_TIMEOUT=10
export PGSSLMODE=prefer
ARCHIVE=/backups/SELECTED_COMPLETED_SET/db-000.dump
GLOBALS=/backups/SELECTED_COMPLETED_SET/globals.sql
test -r "$ARCHIVE"
test -r "$GLOBALS"
"$PG_BIN/pg_restore" --version
"$PG_BIN/pg_restore" --list "$ARCHIVE" > /tmp/archive-list.txt
```

Authenticate without putting a password into command arguments/history. Use a process-local environment variable only for this short-lived utility session (it is still readable by sufficiently privileged processes):

```sh
PGPASSWORD=$(cat /run/restore-credentials/password)
export PGPASSWORD
"$PG_BIN/psql" -X --set=ON_ERROR_STOP=1 \
  --command='SELECT version(), current_database(), inet_server_addr();'
```

Review globals SQL before executing it. Expected globals should have been dumped with `pg_dumpall --globals-only --no-role-passwords`; check that no password hashes/literals slipped in. Do not post the file publicly. Review superuser/replication/bypass-RLS privileges, role memberships, tablespace paths, and existing role conflicts. Prepare a reviewed copy at `/tmp/globals-reviewed.sql` (for example with `kubectl cp` from a secured workstation); do not alter the HDD original. Resolve conflicts intentionally rather than ignoring SQL errors. Set needed application login passwords separately through your secret system, since no role passwords should be backed up.

Inspect archive metadata and its TOC. Confirm roles/owners are available and map unsupported tablespaces deliberately. The destination must not already contain the archived database. Restore with `--create` so archive-supported database creation metadata, ownership, encoding, locale, and database properties are preserved rather than replaced by an arbitrary `createdb` default. Some metadata and operating-system locale/extension prerequisites still require manual verification. If the archive lacks creation metadata, stop and obtain the documented source settings; do not guess.

```sh
test -s /tmp/globals-reviewed.sql
"$PG_BIN/psql" -X --set=ON_ERROR_STOP=1 \
  --file=/tmp/globals-reviewed.sql
"$PG_BIN/pg_restore" --exit-on-error --verbose --create \
  --dbname=postgres "$ARCHIVE"
unset PGPASSWORD
```

`postgres` here is the **maintenance connection database**; `--create` restores the database named in the archive. Do not add `--clean` or drop an existing production database to get past conflicts. Recreate the disposable instance instead. Do not use `--no-owner`/`--no-acl` silently: these change recovery semantics. Review archived SQL further with `pg_restore --file=/tmp/archive-review.sql "$ARCHIVE"` if needed, budgeting scratch space.

### Special case: the archived database is named `postgres`

A freshly initialized destination already contains `postgres`; changing the maintenance connection alone does not resolve that conflict. **Only on the verified disposable destination**, after reviewing its hostname, server identity and contents, deliberately remove that empty bootstrap database and restore through `template1`. Never run this against the source or any destination with valuable data. Keep the restore password available for these commands, then unset it again:

```sh
"$PG_BIN/psql" -X --dbname=template1 --set=ON_ERROR_STOP=1 \
  --command='SELECT version(), inet_server_addr();'
printf 'Confirm this is the EMPTY disposable destination by typing its hostname: '
read -r confirmed_host
test "$confirmed_host" = "$PGHOST"
"$PG_BIN/dropdb" --maintenance-db=template1 postgres
"$PG_BIN/pg_restore" --exit-on-error --verbose --create \
  --dbname=template1 "$ARCHIVE"
unset PGPASSWORD
```

Do this **instead of** the normal restore command above, not after it. Existing sessions can prevent the drop; investigate them rather than adding a force option. Backups of template databases need a separately reviewed procedure.

These commands assume a custom/directory/tar archive supported by `pg_restore`. A plain SQL dump instead requires a separately reviewed workflow using `psql -X --set=ON_ERROR_STOP=1`; it cannot be passed to `pg_restore`. Confirm actual format before starting.

## 4. Verify and clean up

Reconnect to the **restored database** with a controlled verification account. Check extensions and versions (`SELECT extname, extversion FROM pg_extension;`), database owner/encoding/collation, roles and grants, table counts or application-specific invariants, and representative queries. Run application smoke tests against the isolated database, with outbound email/webhooks/jobs disabled. Run `ANALYZE` if appropriate before performance checks. A zero exit code or `pg_restore --list` is not a full restore test.

Record the recovery point and elapsed recovery time. Only a separate, reviewed cutover plan should reconnect production clients. Do not automate a restore into production from this CronJob.

```sh
kubectl -n backup delete pod backup-restore
kubectl -n backup delete secret restore-drill-credentials
```

Delete the disposable database/PVC only after verifying it is not production and retaining drill results. Do not delete backup artifacts as part of restore cleanup.
