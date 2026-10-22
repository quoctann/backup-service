# Operations: step-by-step runbook

This guide explains **what to do, where to do it, and why**. It does not format a disk, modify PostgreSQL Deployments, or change their PVCs.

## Understand the paths and safety checks first

The example setup uses these paths:

```text
Debian host                               Backup container
/mnt/backup-hdd/                           (HDD mountpoint)
└── postgres/                   ────────> /backups/
    ├── .backup-disk-id
    ├── .backup.lock                       (created by the application)
    └── postgres-a/                        (created after a target connects)
        └── backup-<timestamp>-<random>/
            ├── db-000.dump
            ├── globals.sql
            └── manifest.json
```

`/backups` is the container's view of the host directory; it is **not** a second copy of the data. `postgres-a` is a configured target name, not necessarily a database name. The manifest maps numbered dump files to database names.

### Why an unmounted HDD can fill the host's root filesystem

Linux keeps the mountpoint directory even when no disk is mounted there. Without protection, a program writing to `/mnt/backup-hdd/postgres` could therefore write onto the host's root filesystem instead of the HDD.

This setup uses several layers:

| Setting or change | Purpose | Limitation |
| --- | --- | --- |
| Mount by filesystem UUID | Select the same filesystem after reboot, even if `/dev/sdb` becomes `/dev/sdc` | The administrator must choose the correct UUID |
| `hostPath.type: Directory` | Refuse a nonexistent backup directory instead of creating one | An existing directory does not prove the HDD is mounted |
| `.backup-disk-id` on the HDD | Application checks its contents before creating even the lock file | A matching marker mistakenly created on the root filesystem defeats this check |
| UID/GID `10001` and restrictive permissions | Allow the nonroot backup container to write only to its prepared directory | Permissions are not proof of disk identity |
| Node label `backup-storage=usb` | Schedule the Pod on the node carrying the HDD | The label does not monitor whether the disk is connected |

**The application does not verify the host filesystem UUID at runtime.** It checks the marker. The host UUID checks below ensure that you create that marker on the actual HDD, not on the underlying root filesystem. Never create or copy a marker into an unmounted directory to make a failing backup work.

## Prepare the host disk

This is **one-time setup**, before deploying the CronJob. After a reboot, use the checks in steps 4 and 6 to confirm the setup still works; do not recreate the marker.

### Step 1 — Identify the existing HDD filesystem

**Run on:** the Debian host with the USB HDD physically attached.

```sh
lsblk -f
sudo blkid
```

Find the HDD partition using its size, filesystem, label, and device information. Record its filesystem **UUID** and filesystem type. Do not assume that every `/dev/sdb1` is your backup disk.

**Check before continuing:** you know which existing filesystem should hold backups and have inspected any existing data. This guide assumes **ext4**; do not copy the ext4 configuration for an exFAT, NTFS, or other filesystem. No formatting is required or performed here.

In the following examples:

- Replace `REPLACE_WITH_REAL_FILESYSTEM_UUID` with that filesystem's UUID.
- Keep `/mnt/backup-hdd` as the host mountpoint unless you intentionally update all related paths.
- `homelab-backups-01` is an application marker value you choose. **It is not the filesystem UUID.**

### Step 2 — Create the mountpoint, not the backup directory

**Run on:** the Debian host.

```sh
sudo install -d -m 0755 /mnt/backup-hdd
```

**Purpose:** give Linux a directory at which to attach the HDD filesystem. Creating this empty mountpoint on the root filesystem is normal.

**Do not create `/mnt/backup-hdd/postgres` or its marker yet.** Those belong on the mounted HDD, after verification.

### Step 3 — Configure mounting after reboot

**Run on:** the Debian host.

Open the mount configuration:

```sh
sudoedit /etc/fstab
```

If this filesystem already has an entry, inspect and adjust that entry rather than adding a conflicting duplicate. For the existing ext4 filesystem, use:

```fstab
UUID=REPLACE_WITH_REAL_FILESYSTEM_UUID /mnt/backup-hdd ext4 defaults,nofail,nodev,nosuid,noexec 0 2
```

What the important options mean:

- `UUID=...`: identify the filesystem independently of its changing device name.
- `/mnt/backup-hdd`: make the HDD contents available at this host path.
- `nofail`: allow Debian to boot if the USB HDD is missing. **This option does not prevent writes to the underlying directory.** The marker check supplies that protection in the normal missing-disk case.
- `nodev,nosuid,noexec`: treat this as data storage, not a place to run privileged programs.
- `0 2`: disable legacy dump scheduling and allow a non-root-filesystem boot check.

Save the file, then reload systemd's generated mount configuration:

```sh
sudo systemctl daemon-reload
```

### Step 4 — Mount and verify the actual filesystem

**Run on:** the Debian host.

If it is not already mounted at the intended path:

```sh
sudo mount /mnt/backup-hdd
```

Then inspect it:

```sh
mountpoint /mnt/backup-hdd
findmnt --mountpoint /mnt/backup-hdd -o TARGET,SOURCE,UUID,FSTYPE,OPTIONS
```

**Expected result:** `mountpoint` reports that the path is a mountpoint; `findmnt` reports your recorded UUID, the intended filesystem type, and a writable mount (`rw`). A device name alone is not enough.

**Stop if:** the path is not a mountpoint, the UUID is different, or the filesystem is read-only. Do not create the marker while investigating. If the HDD is mounted somewhere else, resolve the intended mount location first; do not unmount a filesystem that other workloads are using.

### Step 5 — Create the private backup directory and marker

**Run on:** the Debian host, only after step 4 succeeds.

The following guarded script checks the UUID again before writing. Replace the UUID inside the script. It creates only the `postgres` directory and marker, not a filesystem or partition.

**Before running:** if `/mnt/backup-hdd/postgres` already contains unrelated data, stop and select a dedicated directory instead. The script sets ownership and permissions on that directory, so do not reuse it blindly.

```sh
sudo sh -eu <<'SH'
expected_uuid='REPLACE_WITH_REAL_FILESYSTEM_UUID'

mountpoint -q /mnt/backup-hdd
actual_uuid=$(findmnt -n --mountpoint /mnt/backup-hdd -o UUID)
if test "$actual_uuid" != "$expected_uuid"; then
  echo 'Wrong filesystem or unreplaced UUID placeholder; nothing initialized.' >&2
  exit 1
fi

test ! -L /mnt/backup-hdd/postgres
marker=/mnt/backup-hdd/postgres/.backup-disk-id
if test -e "$marker" || test -L "$marker"; then
  echo 'Marker already exists; inspect it rather than recreating it.' >&2
  exit 1
fi

install -d -o 10001 -g 10001 -m 0700 /mnt/backup-hdd/postgres
umask 077
set -C
printf '%s\n' 'homelab-backups-01' > "$marker"
chown 10001:10001 "$marker"
chmod 0600 "$marker"
SH
```

Why these changes are needed:

- `10001:10001` matches the container's numeric user and group. A host user named `backupsvc` is not required.
- Directory mode `0700` gives only its owner access; marker mode `0600` gives only its owner read/write access.
- `umask 077` prevents newly created files from being readable by other users.
- `set -C` prevents overwriting an existing marker through ordinary shell redirection.
- The application never creates the marker itself: otherwise it could accidentally authorize an unmounted directory.

`fsGroup` in the Pod is not a substitute for preparing hostPath ownership. Do not use `chmod 777` or recursively change ownership of the entire HDD. If `postgres` already contains unrelated data, stop and select a dedicated directory instead.

### Step 6 — Verify ownership and marker contents

**Run on:** the Debian host.

```sh
sudo stat -c '%u:%g %a %n' \
  /mnt/backup-hdd/postgres \
  /mnt/backup-hdd/postgres/.backup-disk-id
sudo cat /mnt/backup-hdd/postgres/.backup-disk-id
```

**Expected result:**

```text
10001:10001 700 /mnt/backup-hdd/postgres
10001:10001 600 /mnt/backup-hdd/postgres/.backup-disk-id
homelab-backups-01
```

The marker is an identifier, not a password. Backup contents, however, are sensitive.

### Step 7 — Match the application and Kubernetes configuration

**Edit in:** your workload GitOps repository, following [deployment.md](deployment.md).

These values must agree:

| Location | Example value | Meaning |
| --- | --- | --- |
| CronJob `volumes[].hostPath.path` | `/mnt/backup-hdd/postgres` | Directory on the Debian host |
| Container `volumeMounts[].mountPath` | `/backups` | The same directory as seen inside the Pod |
| Config `storage.root` | `/backups` | Where the Go application writes |
| Config `storage.expectedDiskID` | `homelab-backups-01` | Exact expected marker contents |
| Pod `runAsUser` / `runAsGroup` | `10001` / `10001` | Matches host directory ownership |

Also configure your PostgreSQL targets, image, and password Secret mounts. The example files are not ready for your real databases until edited. Credentials belong in the `backup` namespace, not automatically in the database namespace.

### Step 8 — Label the HDD-equipped node and deploy

**Run on:** a machine with `kubectl` access to k3s.

```sh
kubectl get nodes
kubectl label node YOUR_NODE backup-storage=usb
kubectl get nodes -l backup-storage=usb
```

Replace `YOUR_NODE` with the correct node name. If it already has that label, inspect it; do not overwrite a conflicting label without understanding it.

**Purpose:** the CronJob's node selector places the Pod on the host where the HDD path exists. The label does not mount or health-check the disk.

Complete [deployment.md](deployment.md) to provision the namespace and Secrets, publish/configure the image, and sync the workload through Argo CD. Then return here for a manual backup.

**After a host reboot:** repeat the mount/UUID checks from step 4 and ownership/marker checks from step 6. A successful reboot alone does not prove the disk mounted, because `nofail` deliberately permits boot without it.

## Validate and trigger a backup

### Step 1 — Check the deployed workload

**Run on:** a machine with `kubectl` access. The examples assume namespace `backup`.

```sh
kubectl -n backup get cronjob backup-service
kubectl -n backup get jobs
```

Confirm the CronJob has your configured image/configuration and no backup is currently running. The example schedule is **02:00 UTC daily**, not the Debian host's local timezone.

### Step 2 — Start one manual Job

```sh
JOB="backup-manual-$(date -u +%Y%m%d%H%M%S)"
kubectl -n backup create job "$JOB" --from=cronjob/backup-service
```

**Purpose:** create a one-off Job using the deployed CronJob template, without changing the daily schedule. Keep using the same shell so the `JOB` variable remains available.

`concurrencyPolicy: Forbid` prevents overlap between Jobs scheduled by the CronJob controller. It does not cover manually created Jobs. The application additionally uses the shared `.backup.lock`; a competing run fails rather than writes concurrently. Never delete the lock file to resolve contention.

### Step 3 — Watch logs and determine success

```sh
kubectl -n backup logs -f "job/$JOB" --all-containers=true --pod-running-timeout=120s
kubectl -n backup wait --for=condition=complete "job/$JOB" --timeout=10860s
kubectl -n backup get job "$JOB"
```

**Expected result:** a `backup complete` log for each configured target, followed by a completed Job. If Pod startup takes longer than the log command's timeout, inspect events and retry logs when the Pod is running.

If the Job does not complete:

```sh
kubectl -n backup describe job "$JOB"
kubectl -n backup get pods -l "job-name=$JOB"
kubectl -n backup logs "job/$JOB" --all-containers=true
```

`wait` for completion can time out even when the Job has already failed. Inspect status rather than assuming it is still making progress. The example uses `backoffLimit: 0`, so failure does not automatically retry. Some targets may have completed even if another failed; a failed overall Job does not erase their completed sets.

### Step 4 — Inspect the files and prove recovery

**Run on:** the Debian host.

```sh
sudo find /mnt/backup-hdd/postgres -maxdepth 3 -type f -name manifest.json -print
```

Use the manifest to identify the database files in a completed `backup-...` set. Do not restore from a `.partial-...` directory. Follow [restore.md](restore.md) to verify checksums and restore into an isolated PostgreSQL instance.

**Completion criterion:** a finished Job and an archive listing are useful checks, but a successful isolated restore is the proof that recovery works.

### Optional: what the CLI commands do

These commands run **inside an environment with the binary and the stated config path**, not automatically on your Debian host:

```sh
backup-service validate --config /etc/backup-service/config.yaml
backup-service backup --config /etc/backup-service/config.yaml
```

- `validate` checks configuration syntax and values only. It does not verify the disk, credentials, database permissions, or connectivity.
- `backup` performs the disk checks and actual backups. Kubernetes normally invokes this for you.

## Routine monitoring and troubleshooting

### What to monitor

The application writes JSON logs to stdout; configure your existing collector to send these Pod logs to LGTM. Alerts are **not installed by these examples**.

- **Per-target freshness:** alert if a target has no successful backup within your chosen window, for example 26 hours for daily backups. A missed/suspended schedule may produce no failed Job. Do not rely only on overall CronJob success or file modification times.
- **Job failures and duration:** investigate failed Jobs and unexpected runtime. Preserve logs before Kubernetes history/TTL cleanup.
- **Disk health:** monitor free bytes, free inodes, mount identity, and USB I/O errors. On the host, inspect `df -h`, `df -i`, `findmnt`, and `journalctl -k`.

### What to do when a run fails

| Symptom | Check | Do not do this |
| --- | --- | --- |
| Pod Pending / not starting | `kubectl -n backup describe pod POD_NAME`: node label, image pull, Secret, admission events | Weaken cluster-wide security policy to bypass one error |
| Backup storage error / absent marker | Repeat host steps 4 and 6; confirm UUID and marker | Create a replacement marker before proving the HDD is mounted |
| Permission denied | Host UID/GID `10001`, directory traversal, Secret mount permissions | Use `chmod 777` or run the container as root |
| Lock unavailable | Check other active manual/scheduled Jobs; wait for the existing writer | Delete `.backup.lock`; locks belong to the inode, not just the filename |
| Authentication or utility failure | DNS/network, password Secret, grants/RLS, configured server major and client availability | Print passwords into logs |
| Low space / corrupt or partial sets | Inspect usage and manifests; investigate with no backup running | Delete healthy history blindly to make a run pass |

Utility stderr is deliberately not forwarded to centralized logs because it can contain SQL or sensitive server details. For detailed diagnosis, run the matching `psql`/`pg_dump` in a trusted session with the same connection settings and a protected `PGPASSFILE`; inspect stderr there. Do not paste sensitive diagnostics publicly.

### How retention behaves

The example `keepSuccessful: 14` means **14 successful sets per target**, not 14 calendar days. Retention runs only after a new set is published successfully. It checks manifests, sizes, and SHA-256 hashes before counting sets, so same-size corruption cannot displace healthy history.

Rehashing adds HDD reads: allow enough target timeout for dumping, verification, and retention. Corrupt, unknown, and incomplete directories are not automatically pruned. Normally failed staging directories are removed; forced termination can leave partial directories for investigation. Monitor disk usage independently.

## Safely pause for HDD maintenance

**Do not unplug or unmount the disk while a backup or restore reader is active.**

1. Set `spec.suspend: true` on the CronJob in your workload GitOps repository and sync Argo CD. This prevents new scheduled Jobs, but does not stop existing Jobs or manual runs.
2. Confirm that no backup Job or restore Pod is using the disk. Tell other operators not to start manual Jobs.
3. Perform host-side maintenance only after readers/writers have stopped. Follow normal Debian safe-unmount procedures; do not force unmount a busy filesystem.
4. Reconnect/mount the HDD and repeat the UUID and marker checks from host steps 4 and 6. Do not recreate the marker.
5. Remove suspension in GitOps, sync, and run one manual backup. A missed scheduled run may start promptly when resumed, depending on its starting deadline; the filesystem lock still protects against overlap.

For an urgent pause you can patch the live CronJob, but Argo CD may revert it unless Git also reflects the change:

```sh
kubectl -n backup patch cronjob backup-service --type=merge -p '{"spec":{"suspend":true}}'
```

Do not test missing-disk handling by unplugging a mounted, active HDD. The automated integration test uses an empty directory with no marker, without touching a real disk.

## Keep a second copy and practise restores

The HDD shares risks with the host: theft, power events, controller failure, accidental deletion, and ransomware. Maintain an encrypted independent/off-site copy and test retrieving it. This application does not create that second copy for you.

Copy only completed sets, including globals and manifests. Coordinate copying with retention so a set cannot be deleted halfway through a copy; copying from a live directory without coordination is not a consistent snapshot. Verify destination checksums.

The application **does not encrypt archives**. Permissions do not protect a stolen disk or prevent access by privileged processes. If you use host-managed disk encryption, keep its unlock key in your separately protected recovery plan.

Run [restore drills](restore.md) after first deployment, after version changes, and regularly thereafter. Record the recovery point, restore duration, and verification results. Use [disaster-recovery.md](disaster-recovery.md) if the original k3s cluster is unavailable.
