# Deployment and GitOps

## Prerequisites

- Kubernetes supports `CronJob.spec.timeZone` (stable in 1.27); schedule is daily **02:00 UTC**, not host local time.
- Complete [disk setup](operations.md#prepare-the-host-disk) first. This workload needs a trusted node and a namespace allowed to use `hostPath`; Pod Security `restricted` rejects hostPath even with the remaining hardening settings. Do not weaken policy cluster-wide.
- Build/publish the runtime image separately. Its ENTRYPOINT is `/usr/local/bin/backup-service`, with UID/GID **10001** (user `backupsvc`), a shell for the optional restore Pod, and matching clients at `/usr/lib/postgresql/14/bin`, `/usr/lib/postgresql/16/bin`, and `/usr/lib/postgresql/17/bin`. Pushes to `main` publish `ghcr.io/<repo>:<full-commit-sha>` for amd64/arm64. Automated restore drills are currently disabled; verify restores on your deployed architecture. PostgreSQL 14 reaches end of life in November 2026: schedule an upgrade rather than treating the bundled client as continuing security support.
- Provide a PostgreSQL backup role with CONNECT and sufficient read access to all selected objects (including the intended handling of row-level security). Test the actual role; a successful TCP connection is insufficient. Globals export may require additional catalog privileges. Do not grant superuser simply to avoid understanding these permissions.
- Allow network access from namespace `backup` to the target Services and cluster DNS. Cross-namespace database connections use Service DNS, not Kubernetes RBAC. Add environment-specific NetworkPolicies if needed; there is no ingress, Service, service-account token, or RoleBinding here.

## Configure and install

### Publish the image

After committing and reviewing the implementation, push to `main` to run verification and publish to GHCR:

```sh
git push origin main
# Full SHA of the commit just pushed (when HEAD is main):
git rev-parse HEAD
```

The image tag is `ghcr.io/YOUR-OWNER/YOUR-REPOSITORY:<full-commit-sha>`: exactly 40 lowercase hexadecimal characters, without a `sha-` prefix. Only this tag is published; there is no `latest` or semver image alias. **Semver releases are a future idea, not the current implementation; pushing a `v*` tag does not publish or deploy.** Enable GitHub Actions and package publication for the repository. The publish workflow runs Go tests and a local image build before publishing; real restore drills are currently commented out in both workflows. Run them manually on your deployed architecture; multi-architecture builds are not restore validation. Make the package publicly readable, or provision a registry pull Secret and reference it with `imagePullSecrets` in the Pod template. Do not put registry tokens in Git.

### CI → GitOps handoff

After a successful publish, `.github/workflows/publish.yaml` calls `workflow_dispatch` on `quoctann/pisces-infra`, targeting `deploy-backup-service.yml` on `main` with `inputs.image_tag` equal to the published full commit SHA.

- Confirm that the receiver's filename is exactly `deploy-backup-service.yml` (the API uses the filename, not the workflow display name); adjust the dispatch URL if different.
- Set the repository secret `INFRA_DISPATCH_PAT` to a token authorized for the target repository. A fine-grained PAT needs **Actions: write** there. This service's `GITHUB_TOKEN` cannot dispatch across repositories.
- The receiver updates `newTag` for the backup-service image in Kustomize and pushes the change. Configure Argo CD auto-sync, or sync manually. A successful dispatch means GitHub accepted the request, not that the receiver or cluster deployment succeeded.
- Builds from separate pushes can finish out of order. Receiver concurrency alone does not guarantee commit order; before committing an update, the receiver should reject a SHA that is no longer the source repository's current `main` to avoid deploying an older build last.
- The included workload is a **CronJob**: after sync, the updated image applies to newly created Jobs, not Jobs already running.

The SHA tag identifies the source commit but is not a registry-enforced immutable reference. Digest pinning is an optional stronger deployment design; it requires changing the handoff and receiver to update `digest` instead of only `newTag`.

### Configure the workload

1. Copy `examples/kustomize/` into your **workload GitOps repository**, for example `workloads/backup-service/`.
2. Edit `base/config.yaml`: disk identity, targets, supported major versions, databases, and password paths. `storage.keepSuccessful: 14` counts successful sets per target. `sslMode: prefer` is the example default and permits an unencrypted fallback; use certificate-validated TLS where required.
3. Change the overlay image to your published image with `newTag: <full-commit-sha>`. The receiver must update the Kustomization used by your Argo CD Application. Do not leave a `digest` field alongside `newTag` for this SHA-tag handoff: the digest would take precedence. Labels, disk paths, credentials, image architecture, and destination ownership must match the host.
4. Create namespace `backup` and provision its Secret **before** the first scheduled run:

   ```sh
   kubectl apply -f examples/kustomize/overlays/homelab/namespace.yaml
   # Existing file outside Git, containing only the database password:
   kubectl -n backup create secret generic postgres-a-backup \
     --from-file=password=/secure/local/postgres-a-password
   ```

   The Secret must be in `backup`, not the database namespace: Kubernetes cannot mount a Secret across namespaces. The mounted file becomes `/run/credentials/postgres-a/password`. Mode `0440` and supplemental GID 10001 permit nonroot reads. Do not copy real credentials into the example YAML or shell arguments. A Secret is not encryption by itself: restrict Secret access and configure encryption at rest.

   Alternatively, use an existing External Secrets Operator installation: create an `ExternalSecret` in `backup`, reference your configured `SecretStore`/`ClusterSecretStore`, set `spec.target.name: postgres-a-backup`, and map the remote password to `spec.data[].secretKey: password`. Store the remote reference, not the secret value, in Git. This project does not install an operator or invent a provider-specific store configuration. Wait for the resulting Secret to exist before enabling the CronJob.
5. Render and inspect, then apply:

   ```sh
   kubectl kustomize examples/kustomize/overlays/homelab
   kubectl apply --dry-run=server -k examples/kustomize/overlays/homelab
   kubectl apply -k examples/kustomize/overlays/homelab
   ```

   With Argo CD, let Argo apply instead of doing the final manual apply. `configMapGenerator` hashes config content and rewrites the CronJob's ConfigMap reference automatically. Change the config in the generator's directory, not a generated ConfigMap. Keep `examples/config.yaml` synchronized if maintaining both examples.

For more targets, add distinct password Secrets and mounts, and reference their file paths in config. Increase the total Job deadline if necessary: the example allows three hours total while one target has a 45-minute timeout. Temporary storage and memory limits must be sized for your actual workload. Large dumps belong on the HDD, not `/tmp`.

If omitted, configuration defaults are port `5432`, target timeout `1h`, TLS mode `verify-full`, and retention `7` successful sets. The examples explicitly choose `prefer`, `45m`, and `14` sets instead. `verify-full` requires an appropriate trusted CA and matching server hostname; the application forwards explicit libpq certificate-file environment settings (`PGSSLROOTCERT`, `PGSSLCERT`, `PGSSLKEY`, `PGSSLCRL`, `PGSSLCRLDIR`) if configured. Mount certificate/key files read-only with suitable permissions.

## App-of-apps (conceptual wiring)

Keep application source/image builds in this repository. Keep rendered workload intent in a separate workload repository. Your existing infrastructure/root app owns a child `Application` pointing at that workload repo, for example:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: backup-service
  namespace: argocd
spec:
  project: homelab # Must already allow this repo, destination, and Namespace resource.
  source:
    repoURL: https://github.com/YOUR-ORG/homelab-workloads.git
    targetRevision: main
    path: workloads/backup-service/overlays/homelab
  destination:
    server: https://kubernetes.default.svc
    namespace: backup
  syncPolicy:
    syncOptions:
      - CreateNamespace=true
```

This is not an included resource. Replace repo/project/path and commit it where your root app discovers child Applications. Choose automation/pruning explicitly; initial manual sync is safer while checking the disk and credentials. A missing disk is a host problem, not something GitOps should fix by creating directories or markers. Namespace/Secret controllers and the workload should have intentional ordering; app-of-apps alone does not guarantee readiness across child apps.

See [operations](operations.md) to trigger and observe the first run. Do not assume scheduling success means restore readiness.
