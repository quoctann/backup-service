Keep the original backup permissions restricted, and make a private, user-owned copy for development.
Also, 0700 means only UID 10001 can access that directory; membership in group 10001 alone would not help.

### 1. Copy one completed backup set

Run on the Debian host as your normal user. First pause scheduled backups and wait for active Jobs to finish, so retention cannot remove files during copying.

Replace the source directory below with a real completed set—not .partial-*:

```bash
mkdir -p "$HOME/dev-backups"
chmod 700 "$HOME/dev-backups"

sudo cp -a \
  /mnt/storage/backup/postgres-a/backup-REPLACE_WITH_ACTUAL_SET \
  "$HOME/dev-backups/"
```

Give your user ownership of only the copied set:

```bash
sudo chown -R "$(id -u):$(id -g)" \
  "$HOME/dev-backups/backup-REPLACE_WITH_ACTUAL_SET"
```

The originals remain owned by 10001:10001. Resume the CronJob after copying.

If development happens on another machine, transfer this private copy using SSH/SCP. Follow the checksum verification instructions in docs/restore.md before restoring.

### 2. Restore into a separate local development database

Use the matching PostgreSQL major version and install required extensions.

For typical development, create a new database owned by your development account:

```bash
createdb -h localhost -U devuser app_dev
```

Then restore the archive selected from manifest.json:

```bash
pg_restore \
  -h localhost \
  -U devuser \
  --dbname=app_dev \
  --no-owner \
  --no-acl \
  --exit-on-error \
  "$HOME/dev-backups/backup-REPLACE_WITH_ACTUAL_SET/db-000.dump"
```

Here:

- --no-owner avoids requiring production owners.
- --no-acl skips production grants.
- Omitting --create restores into app_dev, not the original database name.

Usually skip globals.sql for development—do not blindly import production superusers or roles. Some applications still require selected roles or extensions; recreate those deliberately.

Keep development isolated: disable email, payments, webhooks, and background jobs, and sanitize sensitive data before using it locally.