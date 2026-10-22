#!/usr/bin/env bash

# Real PostgreSQL round trips. Requires Docker and a locally built backup image.
set -euo pipefail
image="${BACKUP_IMAGE:-postgres-backup:test}"

# Exercise host-owned bind mounts without requiring sudo/chown. Production
# uses the image's UID 10001; ensure that default remains non-root too.
test "$(docker run --rm --entrypoint id "$image" -u)" = 10001
if [[ "$(id -u)" = 0 ]]; then
  echo 'Run this integration suite as a non-root Docker user' >&2
  exit 1
fi

run_id="backup-it-$(date +%s)-$$"
network="$run_id"
scratch_root="${RUNNER_TEMP:-${TMPDIR:-/tmp/opencode}}"
mkdir -p "$scratch_root"
scratch="$(mktemp -d "$scratch_root/postgres-backup-it.XXXXXX")"
containers=()
cleanup() {
  for container in "${containers[@]}"; do docker rm -fv "$container" >/dev/null 2>&1 || true; done
  docker network rm "$network" >/dev/null 2>&1 || true
  # Preserve evidence for debugging; never delete an arbitrary mount path.
  echo "Integration artifacts: $scratch"
}
trap cleanup EXIT

docker network create "$network" >/dev/null
password="$(cat /proc/sys/kernel/random/uuid)"
printf '%s' "$password" > "$scratch/password"
chmod 600 "$scratch/password"
mkdir "$scratch/backups"
printf '%s\n' integration-disk > "$scratch/backups/.backup-disk-id"

run_backup() {
  docker run --rm --network "$network" --user "$(id -u):$(id -g)" \
    --read-only --tmpfs /tmp:rw,mode=1777 \
    -v "$scratch/config.yaml:/config.yaml:ro" \
    -v "$scratch/password:/credentials/password:ro" \
    -v "${1:-$scratch/backups}:/backups" \
    "$image" backup --config /config.yaml
}

for version in 14 16 17; do
  server="$run_id-pg$version"
  containers+=("$server")

  docker run -d --name "$server" --network "$network" \
    -e POSTGRES_PASSWORD="$password" -e POSTGRES_DB=app \
    "postgres:$version" >/dev/null
  
  ready=false
  for attempt in {1..60}; do
    if docker exec "$server" pg_isready -h 127.0.0.1 -U postgres -d app >/dev/null 2>&1; then
      ready=true
      break
    fi
    sleep 1
  done
  
  if [[ "$ready" != true ]]; then docker logs "$server"; exit 1; fi
  docker exec "$server" psql -U postgres -d app -v ON_ERROR_STOP=1 -c \
    "CREATE ROLE app_reader; CREATE TABLE sample (id bigserial PRIMARY KEY, value text NOT NULL); INSERT INTO sample(value) VALUES ('recover me'); GRANT SELECT ON sample TO app_reader;" >/dev/null
  
  cat > "$scratch/config.yaml" <<EOF
storage:
  root: /backups
  expectedDiskID: integration-disk
  keepSuccessful: 2
targets:
  - name: pg$version
    host: $server
    port: 5432
    majorVersion: $version
    username: postgres
    databases: [app]
    passwordFile: /credentials/password
    sslMode: disable
    timeout: 2m
EOF

  run_backup
  
  archive="$(find "$scratch/backups/pg$version" -name '*.dump' -type f -print -quit)"
  test -n "$archive"
  restore="$run_id-restore$version"
  containers+=("$restore")
  
  # A different bootstrap username avoids an expected duplicate postgres role
  # when restoring globals, allowing ON_ERROR_STOP to catch every SQL error.
  docker run -d --name "$restore" --network "$network" \
    -e POSTGRES_PASSWORD="$password" -e POSTGRES_USER=recovery_admin \
    -e POSTGRES_DB=postgres "postgres:$version" >/dev/null
  
  ready=false
  for attempt in {1..60}; do
    if docker exec "$restore" pg_isready -h 127.0.0.1 -U recovery_admin -d postgres >/dev/null 2>&1; then
      ready=true
      break
    fi
    sleep 1
  done
  
  if [[ "$ready" != true ]]; then docker logs "$restore"; exit 1; fi
  globals="$(find "$scratch/backups/pg$version" -name globals.sql -type f -print -quit)"
  test -n "$globals"
  docker exec -i "$restore" psql -U recovery_admin -d postgres -v ON_ERROR_STOP=1 < "$globals"
  
  # Restore into a separate clean cluster, including the database definition.
  docker exec -i "$restore" pg_restore -U recovery_admin --exit-on-error --create -d postgres < "$archive"
  actual="$(docker exec "$restore" psql -U recovery_admin -d app -Atc 'SELECT value FROM sample WHERE id = 1')"
  test "$actual" = 'recover me'
  next_id="$(docker exec "$restore" psql -U recovery_admin -d app -Atc "INSERT INTO sample(value) VALUES ('sequence works') RETURNING id" | head -n 1)"
  test "$next_id" = 2
  grant="$(docker exec "$restore" psql -U recovery_admin -d app -Atc "SELECT has_table_privilege('app_reader', 'sample', 'SELECT')")"
  test "$grant" = t
  echo "PostgreSQL $version: backup, restore, data, sequence and grant checks passed"
done

# Keep only two complete sets, but only after newer sets have succeeded.
run_backup
run_backup

count="$(find "$scratch/backups/pg17" -mindepth 1 -maxdepth 1 -type d ! -name '.partial-*' | wc -l)"
test "$count" -eq 2
echo 'Successful-set retention check passed'

# Kubernetes Forbid does not prevent a manually created Job. Demonstrate that
# the host and the container cooperate on the same advisory filesystem lock.
if command -v flock >/dev/null; then
  (
    flock -n 9
    if run_backup; then
      echo 'ERROR: overlapping backup bypassed the lock' >&2
      exit 1
    fi
  ) 9> "$scratch/backups/.backup.lock"
  echo 'Overlapping backup check passed'
fi

# Fail-closed check: an ordinary empty directory must not become a backup disk.
mkdir "$scratch/missing-disk"
if run_backup "$scratch/missing-disk"; then
  echo 'ERROR: backup accepted a missing disk marker' >&2
  exit 1
fi

test -z "$(find "$scratch/missing-disk" -mindepth 1 -print -quit)"
echo 'Missing disk check passed without writing files'
