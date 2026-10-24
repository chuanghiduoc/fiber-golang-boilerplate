# Kubernetes deployment

Production-oriented Kubernetes manifests for the Fiber SaaS boilerplate. They
assume a managed PostgreSQL and Redis, an S3-compatible bucket, and an
ingress-nginx controller. No stateful database is bundled in the app
Deployment.

These files are templates. Replace every `OWNER/REPO`, `example.com`,
`REPLACE_*`, bucket, and CIDR placeholder before applying them. Do not commit a
rendered Secret.

## Contents

| File | Purpose |
| --- | --- |
| `base/` | Application manifests: Deployment, Service, HPA, PDB, NetworkPolicy, Ingress |
| `base/kustomization.yaml` | Reusable app base + image transformer; omits the Job and Secret |
| `prereqs/` | Namespace + ConfigMap kustomization, applied before the migration Job |
| `migration/job.yaml` | One-shot `/app/migrate` Job; applied separately, before the app |
| `overlays/staging/` | Staging namespace, HPA, ConfigMap, Ingress, resources, image tag |
| `overlays/production/` | Production namespace, HPA, ConfigMap, Ingress, resources, image tag |
| `overlays/components/` | Environment ConfigMap patches shared by the app and prereqs builds |
| `kustomization.yaml` | Convenience wrapper for `kubectl apply -k deploy/k8s` |
| `secret.example.yaml` | Template for the real Secret; excluded from every kustomization |

## Prerequisites

- Kubernetes 1.29+ and `kubectl` (with built-in kustomize).
- `kustomize` v5.8.1+ for `kustomize edit set image` in the manual flow;
  `kubectl kustomize` can render but has no `edit` subcommand.
- A managed PostgreSQL 18 instance reachable over TLS. `DB_SSLMODE=require`
  (or `verify-full`) is mandatory because `APP_ENV=production`.
- A managed Redis instance. Redis is required for correct session invalidation
  when more than one API replica is running.
- An S3-compatible bucket and credentials. S3 is the recommended storage
  backend; local disk requires a shared ReadWriteMany volume (see below).
- An SMTP relay that supports STARTTLS (the app uses STARTTLS, normally port
  587; implicit TLS on port 465 is not supported).
- ingress-nginx installed in the `ingress-nginx` namespace, or edit the
  Ingress class/NetworkPolicy selectors to match your controller.
- metrics-server for the HPA.
- cert-manager, or a pre-created TLS Secret for the Ingress host.
- Access to the GHCR package. If it is private, create an image pull Secret.
- For the optional GitHub Actions CD path: two GitHub Environments named
  `staging` and `production`, each with protection rules and a `KUBE_CONFIG`
  secret. See [Continuous deployment](#continuous-deployment-github-actions).

## Environments and overlays

The repository ships two overlays:

| Overlay | Namespace | HPA | Notes |
| --- | --- | --- | --- |
| `overlays/staging` | `fiber-boilerplate-staging` | 2–4 replicas | Smaller CPU/memory requests, staging hosts and bucket |
| `overlays/production` | `fiber-boilerplate-production` | 3–10 replicas | Immutable image tag required, production hosts and bucket |

Both reference the reusable base (`../../base`), keep the same resource names,
and isolate environments by namespace. `overlays/components/<env>-config`
contains the environment ConfigMap patch; the app overlay and its `prereqs`
sub-overlay both include it, so the migration Job and API read the same
non-secret configuration. `overlays/<env>/migration` is a separate
kustomization for the one-shot Job: the app overlay must never include it.

The image tag in every overlay is the literal placeholder
`REPLACE_WITH_IMAGE_TAG`. The deploy workflow replaces it in a temporary copy;
for a manual deployment, set it in the app overlay **and** the migration
overlay (see step 3). `base/` still uses `ghcr.io/OWNER/REPO:latest` for the
legacy `kubectl apply -k deploy/k8s` path; do not use that path for production.

Render an overlay without a cluster:

```bash
kubectl kustomize deploy/k8s/overlays/staging > /tmp/staging.yaml
kubectl kustomize deploy/k8s/overlays/production > /tmp/production.yaml
kubectl kustomize deploy/k8s/overlays/staging/prereqs > /tmp/staging-prereqs.yaml
kubectl kustomize deploy/k8s/overlays/staging/migration > /tmp/staging-migration.yaml
```

## 1. Configure the placeholders

At minimum, replace:

- The real image repository in `images[].newName` in
  `base/kustomization.yaml`, both app overlays, and both migration overlays.
  Keep `images[].name` exactly `ghcr.io/OWNER/REPO`: it is the stable source
  key used by the deploy workflow. Set `newTag` for manual deploys. The base
  Deployment and raw Job image literals are placeholders rewritten by these
  transformers.
- `overlays/staging/ingress-patch.yaml` and
  `overlays/production/ingress-patch.yaml`: API host and TLS Secret.
- `overlays/components/staging-config/configmap-patch.yaml` and
  `overlays/components/production-config/configmap-patch.yaml`:
  `APP_FRONTEND_URL`, `CORS_ALLOW_ORIGINS`, `DB_HOST`, `SMTP_HOST`,
  `EMAIL_FROM_ADDRESS`, and `STORAGE_S3_BUCKET`. Keep `Idempotency-Key`,
  `X-Tenant-Slug`, `Accept-Language`, and `Accept-Version` in
  `CORS_ALLOW_HEADERS`, and keep `Idempotent-Replayed`, `Content-Language`, and
  the version/deprecation headers in `CORS_EXPOSE_HEADERS` so browser clients
  can read replay, locale, and version metadata.
- `prereqs/configmap.yaml`: shared defaults such as `APP_TRUSTED_PROXIES`,
  `TENANT_REQUIRE_HEADER=true` (required in production so header-less writes
  never fall back to tenant 1), and the S3 endpoint. Keep environment-specific
  values in the overlay components. Every API client must send
  `X-Tenant-Slug`; the frontend forwards the `?tenant=<slug>` from
  reset/verification links to the API.
- `base/networkpolicy.yaml`: the DB/Redis CIDRs and SMTP ports/ranges if your
  managed services are not in `10.0.0.0/8` or use different ports.
- The two overlay `namespace:` values and their Namespace rename patches, plus
  the matching `prereqs`/`migration` overlays, if you rename an environment.
- `secret.example.yaml` (copied outside the repo): every `REPLACE_*` value.

Find the remaining placeholders with:

```bash
grep -RInE 'OWNER/REPO|example\.com|REPLACE_|10\.0\.0\.0/8' deploy/k8s
```

Use an immutable release tag (for example `1.4.2`) in production, not
`:latest`. The app overlay's `images:` transformer updates the Deployment and
the migration overlay's transformer updates the Job; the deploy workflow sets
both from one input. Production uses `imagePullPolicy: IfNotPresent` because the
tag is immutable; staging uses `Always` so a mutable staging branch tag is
refreshed. The legacy raw base path still uses `:latest` and `Always`; do not use
it for production.

Do not rename `images[].name` to your repository. Kustomize uses it only as the
source key; the actual image is `newName:newTag`. If you must rename the key,
change `SOURCE_IMAGE` in `.github/workflows/deploy.yml` and every base/overlay
kustomization together. Changing only some of them makes the workflow fail its
stable-key or rendered-image check.

`APP_TRUSTED_PROXIES` must contain only the cluster pod/service CIDR that
contains ingress-nginx. Setting it to `0.0.0.0/0` lets clients spoof
`X-Forwarded-For` and bypass per-IP rate limits.

`APP_PORT` is part of the probe, Service, container port, and NetworkPolicy
contract. If you change it from 8080, update `containerPort`, the `http` named
port used by the probes, the Service port, the Ingress backend, and the
NetworkPolicy ingress port `8080`.

## 2. Create the namespace and Secret

```bash
export ENV=staging
export NS=fiber-boilerplate-staging

# Applies the environment Namespace and ConfigMap (with staging values).
kubectl apply -k deploy/k8s/overlays/$ENV/prereqs

cp deploy/k8s/secret.example.yaml /tmp/fiber-secret.yaml
# Edit metadata.namespace to $NS and replace every value, then:
kubectl apply -f /tmp/fiber-secret.yaml
```

For production use `ENV=production` and
`NS=fiber-boilerplate-production`. The `prereqs` kustomization contains only
the Namespace and ConfigMap; the Secret is not part of any kustomization.

Never commit the rendered Secret. For GitOps, use External Secrets, Sealed
Secrets, SOPS, or your cloud secret manager; `secret.example.yaml` is
deliberately not part of `base/kustomization.yaml` or any overlay.

`JWT_SECRET`, `WEBHOOK_SECRET_ENCRYPTION_KEY`, `METRICS_AUTH_TOKEN`, and
`ADMIN_PASSWORD` use short `REPLACE_ME` placeholders on purpose. The API's
production validation rejects them, so an unedited template fails closed
instead of starting with a known secret.

The template leaves `ADMIN_EMAIL`/`ADMIN_PASSWORD` commented out, so no admin
user is bootstrapped. To create one on API startup, uncomment both values (and
optionally `ADMIN_NAME`); the password must be at least 12 characters. Setting
only one of them makes the API fail production config validation.

If the GHCR package is private, create a pull Secret and uncomment
`imagePullSecrets` in `base/deployment.yaml` (and add it to
`migration/job.yaml` if that image is also private):

```bash
kubectl -n "$NS" create secret docker-registry ghcr-pull \
  --docker-server=ghcr.io \
  --docker-username=<github-user> \
  --docker-password=<PAT-with-read:packages>
```

## 3. Run the migration Job before the rollout

Only `cmd/migrate` is allowed to apply DDL. The API and seed commands never
migrate, so API replicas do not race on schema changes.

The app overlay intentionally **excludes** the migration Job, so
`kubectl apply -k deploy/k8s/overlays/$ENV` cannot create the Job before the
Deployment. `kubectl apply -k` also has no cross-resource ordering guarantee.
The only safe flow is to apply the namespace/configmap/secret, apply and await
the Job, and only then apply the app overlay:

```bash
export ENV=staging
export NS=fiber-boilerplate-staging
export IMAGE_REPO=ghcr.io/OWNER/REPO
export IMAGE_TAG=1.4.2

# Pin the same immutable tag in the app and migration overlays. The CD workflow
# does this in a temporary copy; these commands edit tracked files, so commit
# the pin intentionally or revert it after the deploy.
(cd deploy/k8s/overlays/$ENV && kustomize edit set image "$IMAGE_REPO=$IMAGE_REPO:$IMAGE_TAG")
(cd deploy/k8s/overlays/$ENV/migration && kustomize edit set image "$IMAGE_REPO=$IMAGE_REPO:$IMAGE_TAG")

if kubectl -n "$NS" get job fiber-boilerplate-migrate > /dev/null 2>&1; then
  existing="$(kubectl -n "$NS" get job fiber-boilerplate-migrate \
    -o jsonpath='{range .status.conditions[*]}{.type}={.status}{"\n"}{end}' 2>/dev/null || true)"
  if ! printf '%s' "$existing" | grep -qxE 'Complete=True|Failed=True'; then
    echo "Refusing to delete a migration Job that is not complete or failed." >&2
    exit 1
  fi
  kubectl -n "$NS" delete job fiber-boilerplate-migrate --wait=true
fi
kubectl apply -k deploy/k8s/overlays/$ENV/migration
kubectl -n "$NS" wait \
  --for=condition=complete \
  --timeout=15m \
  job/fiber-boilerplate-migrate
kubectl -n "$NS" logs job/fiber-boilerplate-migrate
```

Do not add the Job back to `base/kustomization.yaml` or an app overlay; that
reintroduces the ordering ambiguity this split is meant to remove. The
migration overlay and the app overlay must use the same image tag; the CD
workflow sets both from one input and refuses mismatched renders.

On failure:

```bash
kubectl -n "$NS" describe job fiber-boilerplate-migrate
```

The Job has `restartPolicy: Never`, `backoffLimit: 3`, and
`activeDeadlineSeconds: 900`. Jobs are immutable: delete the old Job before
applying a changed template. Only delete it after confirming it is complete or
failed and no migration is running; deleting a running Job can interrupt DDL.
The deploy workflow refuses to delete a Job that is not `Complete` or `Failed`.
`ttlSecondsAfterFinished: 86400` removes a completed Job after 24 hours.

## 4. Deploy the application

Once the migration Job has completed:

```bash
kubectl apply -k deploy/k8s/overlays/$ENV
kubectl -n "$NS" rollout status deployment/fiber-boilerplate --timeout=10m
```

This applies the environment Namespace, ConfigMap, Deployment, Service, HPA,
PDB, NetworkPolicy, and Ingress. The migration Job and Secret are not part of
the app overlay and are not re-applied here. If the Job was garbage-collected
by its TTL after you ran it, rerun the Job apply/wait in step 3 before this
step.

The Deployment intentionally has no `spec.replicas`; the HPA owns the replica
count (`minReplicas: 2` staging, `3` production). Do not add `replicas` back to
the Deployment, or the HPA and Deployment controllers will fight over it.

The Deployment uses `maxUnavailable: 0` and `maxSurge: 1`; a new pod must pass
`/readyz` before an old pod is removed. The in-process job workers use a
PostgreSQL queue with `SKIP LOCKED`, so running multiple replicas is safe.

## 5. Verify

```bash
kubectl -n "$NS" get deploy,rs,pods,svc,hpa,pdb
kubectl -n "$NS" rollout status deployment/fiber-boilerplate --timeout=5m

kubectl -n "$NS" port-forward svc/fiber-boilerplate 8080:8080
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
```

Through the Ingress (replace the host with the overlay's host):

```bash
curl -fsS https://api.staging.example.com/healthz
curl -fsS https://api.staging.example.com/readyz
```

`/healthz` is a process liveness check. `/readyz` also checks dependencies
such as PostgreSQL and Redis; only PostgreSQL being down returns `503`, while a
Redis outage or a saturated DB pool reports `status:"degraded"` with `200`
(see `docs/operations.md`). The startup probe deliberately uses `/healthz`
(not `/readyz`), but it only covers slow-but-reachable startup: the API exits
during startup if PostgreSQL, storage (including the S3 bucket check), or cache
initialization fails. The startup probe cannot mask that dependency outage; the
container exits and restarts. Once startup succeeds, `/readyz` gates traffic. If
`kubectl get hpa` shows `<unknown>` targets, install/fix metrics-server.

## 6. Rollback

```bash
kubectl -n "$NS" rollout history deployment/fiber-boilerplate
kubectl -n "$NS" rollout undo deployment/fiber-boilerplate
kubectl -n "$NS" rollout status deployment/fiber-boilerplate --timeout=10m
```

Database migrations are forward-only in this setup. A previous image may not
be compatible with a newer schema, so prefer backward-compatible migrations and
test rollback before production. `cmd/migrate` has no automatic down step in
the Kubernetes Job.

## Continuous deployment (GitHub Actions)

`.github/workflows/deploy.yml` implements a manual, environment-gated deploy
path. It does **not** contact a cluster unless you configure the GitHub
Environments and secrets below; there is no cluster bundled with this
repository and no automatic deploy out of the box.

### Triggers and environments

- `workflow_dispatch` with `environment` (`staging` or `production`),
  `image_tag`, optional `image_preflight`, and a boolean `dry_run`.
- A `workflow_run` trigger fires when the Release workflow completes, but the
  job only runs for a **successful** Release run that was triggered by a `v*`
  tag. Failed or cancelled releases never deploy, and the Release run's tests
  have already passed. The production image tag is derived from
  `github.event.workflow_run.head_branch` (`v1.4.2` -> `1.4.2`). GitHub only
  runs `workflow_run` workflows whose file exists on the default branch, so
  merge this workflow before relying on tag-driven production.
- `image_preflight` (manual dispatch only) logs in to GHCR with the workflow
  token and waits up to five minutes for the exact image to exist. It requires
  `packages: read`, which the workflow grants. Tag-driven deploys rely on the
  successful Release run and skip the preflight.
- `dry_run: true` renders the app, prereqs, and migration manifests and uploads
  them as an artifact. It does not read `KUBE_CONFIG` or call `kubectl` against
  a cluster, so it works with no environment secret (the GitHub Environment
  still gates the job).
- Concurrency is serialized per environment with `cancel-in-progress: false`,
  so a migration or rollout is never cancelled by a second deploy.

### Required GitHub configuration

Create two GitHub Environments named `staging` and `production`. Add protection
rules (required reviewers, wait timer, deployment branches/tags) as appropriate
for your organization.

In each environment:

| Name | Required | Purpose |
| --- | --- | --- |
| `KUBE_CONFIG` (secret) | yes for real deploys | Kubeconfig for that environment's cluster. Raw YAML or base64; base64 is recommended. |
| `GHCR_IMAGE` (variable, or secret) | no | Bare lowercase image repository, for example `ghcr.io/acme/fiber`. It must equal the Release workflow image `ghcr.io/<owner>/<repo>`; otherwise the workflow fails. No scheme, tag, digest, port, or colon. Defaults to `ghcr.io/${{ github.repository }}`. |

The workflow does not create the application Secret. Before the first deploy,
create `fiber-boilerplate-secrets` in the target namespace out of band (or via
External Secrets / Sealed Secrets / SOPS). The workflow fails early with a
clear error if it is missing. The image is pulled by the cluster, so give the
cluster pull credentials separately if the package is private. `packages: read`
is granted only for the optional GHCR preflight; it is not used to pull into
the cluster.

### Deploy sequence and image promotion

1. Resolve `staging`/`production`, the namespace, and the image repository/tag.
   Staging accepts branch tags (and always pulls); production rejects `latest`,
   branch names, and placeholder tags and requires a full semver release
   (`1.4.2`) or `sha-<40 hex>`.
2. Validate the stable `ghcr.io/OWNER/REPO` image source key, render a temporary
   copy of the overlay, and inject the same `newName`/`newTag` into both the app
   and migration kustomizations.
3. Apply the overlay `prereqs` (Namespace + environment ConfigMap) and verify
   the Secret exists.
4. Inspect any existing migration Job and refuse to delete one that is not
   `Complete` or `Failed`; delete only a finished Job. Then apply the migration
   overlay and poll for up to 15 minutes. A `Failed=True` condition fails the
   step immediately, and the app is not applied if the Job fails. The workflow
   job timeout is 60 minutes.
5. Apply the app overlay and wait for `rollout status`.
6. Smoke-check `/healthz` and `/readyz` through a temporary service
   port-forward. No ingress DNS or public URL is required.
7. On failure, print the rollout history/undo commands and pod status; the
   workflow does not auto-rollback because migrations are forward-only.

Promote a release by deploying the same full semver image tag to staging, then
to production. The workflow never rebuilds; it only promotes an image already
published by `.github/workflows/release.yml`. To deploy from a workstation
instead, use the manual flow in steps 1–4 with `kustomize edit set image` in the
overlay and migration kustomization.

### Rollback

```bash
kubectl -n "$NS" rollout history deployment/fiber-boilerplate
kubectl -n "$NS" rollout undo deployment/fiber-boilerplate
kubectl -n "$NS" rollout status deployment/fiber-boilerplate --timeout=10m
```

The migration Job is not rolled back. If the failed release applied an
incompatible migration, restore the database from a verified backup/PITR
snapshot or ship a forward fix instead of rolling the image back blindly. See
[Operations](../../docs/operations.md#6-migrations-and-legacy-upgrade).

## Operational notes

### External PostgreSQL and Redis

The manifests do not deploy PostgreSQL or Redis. Use managed services with
TLS, automated backups/point-in-time recovery, and private networking.

- `DB_HOST`, `DB_PORT`, `DB_DATABASE`, `DB_USERNAME`, `DB_SCHEMA`, and
  `DB_SSLMODE` are in the ConfigMap; `DB_PASSWORD` is in the Secret.
- `DB_MAX_CONNS` is per pod. Production uses 10 with HPA `maxReplicas: 10`,
  so keep the database connection limit above 100. Staging uses 5 with max 4.
- `CACHE_DRIVER=redis` and `REDIS_URL` are required for multi-replica session
  invalidation. Use `rediss://` when the provider supports TLS.
- Update the Database/Redis CIDRs and ports in `base/networkpolicy.yaml` if they
  are not private `10.0.0.0/8` endpoints. A migration Job blocked by the
  NetworkPolicy will fail to connect.

### S3 versus local uploads

The template uses `STORAGE_DRIVER=s3` and `readOnlyRootFilesystem: true`. This
is the recommended multi-replica setup. Create the bucket before rollout: each
replica checks bucket existence at startup (and tries to create a missing
bucket, which can race across replicas). An unreachable S3 endpoint or bad
credentials prevents startup; after startup, object reads/writes/deletes fail
on use and are not covered by `/readyz`.

The Deployment mounts a small bounded `emptyDir` at `/tmp` as scratch space so
the image root can stay read-only even if a dependency needs temp files. It is
not upload storage and is safe to lose on restart.

If you must use `STORAGE_DRIVER=local`:

- Mount a writable PVC at `/app/uploads` in the Deployment (the commented
  example in `base/deployment.yaml`).
- Use a ReadWriteMany volume (NFS/EFS/CephFS) so every replica sees the same
  files; `emptyDir` is node-local, is lost on restart, and is only suitable for
  a single replica.
- Ensure the volume is writable by UID 100 / GID 101 (`fsGroup: 101` is set)
  or adjust `runAsUser`/`runAsGroup` to match the image user and volume
  permissions. The image's `appuser` is UID 100 / GID 101, so 100:101 ownership
  matches `/app/uploads` in the image.
- A local upload volume is not a substitute for object storage durability or
  backups.

### JWT_SECRET rotation and the job queue

`JWT_SECRET` is also used to derive the AES key that encrypts queued email job
payloads. Rotating it has two effects:

1. All existing access tokens fail validation. Refresh tokens are opaque
   database rows and remain valid unless revoked separately; revoke them (or
   bump the token version) if a full re-login is required.
2. Encrypted jobs already in the `jobs` table can no longer be decrypted. A
   rolling update where old and new replicas use different secrets can also
   produce jobs that the other replicas cannot open.

Before rotating, drain the queue (stop new email-producing traffic and wait
until no pending/processing jobs remain), then stop all API replicas (for
example, delete or pause the HPA and scale the Deployment to 0), update the
Secret, and scale back up. A normal rolling update briefly mixes old and new
secrets, so a new replica's queued job can be claimed by an old replica that
cannot decrypt it. If draining is not possible, expect failed jobs and inspect
the dead-letter/failed rows after the rotation.

### WEBHOOK_SECRET_ENCRYPTION_KEY rotation

`WEBHOOK_SECRET_ENCRYPTION_KEY` seals outbound endpoint and inbound source
signing secrets with AES-256-GCM. It is required in production and must be at
least 32 characters. Rotating it does **not** re-encrypt existing rows: every
stored webhook secret becomes unreadable.

1. Announce a receiver maintenance window.
2. Update the Secret and restart all replicas with the new key.
3. Rotate every endpoint and inbound source secret through the API; deliver the
   new plaintext values to the receivers.
4. In-flight outbound deliveries sealed with the old key become `dead`; replay
   them after rotation. Inbound senders receive `401` until they use the new
   secret.

Unlike `JWT_SECRET`, this key does not sign access tokens and does not affect
the email job queue. Keep it distinct from `JWT_SECRET` so the two rotations
cannot interfere.

### Configuration changes

Environment variables are injected with `envFrom`, so editing the ConfigMap or
Secret does not update running pods. Restart the Deployment explicitly:

```bash
kubectl -n "$NS" rollout restart deployment/fiber-boilerplate
kubectl -n "$NS" rollout status deployment/fiber-boilerplate
```

Changing the Deployment pod template (including the image tag) already triggers
a rollout.

### Ingress, TLS, and network policy

- `base/ingress.yaml` assumes ingress-nginx. Replace the host and TLS Secret in
  each `overlays/<env>/ingress-patch.yaml`, or enable the commented cert-manager
  annotation in the base.
- `proxy-body-size` is 16 MiB, above `APP_BODY_LIMIT` (12 MiB) and
  `STORAGE_MAX_FILE_SIZE` (10 MiB).
- The NetworkPolicy allows ingress only from the `ingress-nginx` namespace. If
  your controller runs elsewhere, change the namespace label. A Prometheus rule
  for the `monitoring` namespace is included as a comment.
- **Kubelet probes are CNI-dependent.** Many CNIs allow node-originated
  readiness/liveness probes to the pod regardless of NetworkPolicy; some enforce
  policy and will drop them. `base/networkpolicy.yaml` includes a commented
  ingress rule for the node/node-pod CIDR: uncomment and set it if your CNI
  blocks probes. Do not widen it to `0.0.0.0/0`.
- Egress to HTTPS and SMTP is intentionally broad (`0.0.0.0/0`); tighten it
  with VPC endpoints/provider ranges. DB and Redis egress are limited to
  `10.0.0.0/8` by default.

### HPA and PDB

The HPA requires metrics-server. Staging scales on CPU at 70% utilization from
2 to 4 replicas; production scales from 3 to 10. The HPA owns the Deployment's
replica count; the Deployment intentionally has no `spec.replicas`. The PDB
keeps at least one API pod available during voluntary disruptions such as node
drains. Every environment runs at least two replicas, so drains proceed one pod
at a time without deadlocking.

### Release workflow and CI

`.github/workflows/release.yml` runs on `v*` tags and manual dispatch. It:

- runs `go vet ./...`, `go build ./...`, and `go test -race ./...` before
  publishing;
- builds `linux/amd64` and `linux/arm64` images with Buildx/QEMU;
- pushes to `ghcr.io/${{ github.repository }}` using `GITHUB_TOKEN`;
- emits semver tags (`1.4.2`, `1.4`, `1`) and `latest` for **stable** semver
  tags; pre-releases keep only their full version tag. Every run also gets a
  full-SHA tag, and manual branch dispatches get the branch name;
- publishes provenance and an SBOM, with GitHub Actions cache;
- pins actions to full commit SHAs (version comments alongside) verified on
  2026-10-24.

The CI `Manifests` job runs `actionlint`, renders the base and both environment
overlays (including their `prereqs` and `migration` kustomizations) with
kustomize, checks that no app overlay contains the Secret or migration Job, and
runs kubeconform strict against Kubernetes 1.29, including
`migration/job.yaml` and `secret.example.yaml`.

`.github/workflows/deploy.yml` is the optional CD path. It is manual for staging
and for manual production runs, and auto-deploys production only after a
successful Release workflow run triggered by a `v*` tag. It is gated by GitHub
Environments; see
[Continuous deployment](#continuous-deployment-github-actions).

Example release:

```bash
git tag v1.4.2
git push origin v1.4.2
```

To build and push the same image manually (for example from a workstation):

```bash
echo "<PAT-with-write:packages>" | docker login ghcr.io -u <github-user> --password-stdin
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --target prod \
  --tag ghcr.io/OWNER/REPO:1.4.2 \
  --push .
```

Pin the release by setting `images[].newTag` to `1.4.2` in
`deploy/k8s/overlays/<env>/kustomization.yaml` and
`deploy/k8s/overlays/<env>/migration/kustomization.yaml`; the transformers
update the Deployment and Job. The deploy workflow does this in a temporary
copy. Use the exact version tag in production; the major/minor and `latest`
tags are mutable. The legacy `deploy/k8s/base/kustomization.yaml` still uses
`:latest` for the raw `kubectl apply -k deploy/k8s` path; do not use that path
for production.

## Validation

```bash
# Parse every YAML file (Python 3 with PyYAML is only an example).
python3 - <<'PY'
import glob, yaml
for path in sorted(glob.glob("deploy/k8s/**/*.yaml", recursive=True)) + [
    ".github/workflows/ci.yml",
    ".github/workflows/deploy.yml",
    ".github/workflows/release.yml",
]:
    with open(path, encoding="utf-8") as f:
        list(yaml.safe_load_all(f))
    print("ok", path)
PY

# Render the base and both overlays (no cluster required).
kubectl kustomize deploy/k8s
kubectl kustomize deploy/k8s/overlays/staging
kubectl kustomize deploy/k8s/overlays/production
kubectl kustomize deploy/k8s/overlays/staging/prereqs
kubectl kustomize deploy/k8s/overlays/staging/migration

# Lint both workflows (actionlint v1.7.12 is what CI uses).
actionlint

# With a reachable cluster, perform a client-side dry run. Without a cluster
# this still needs API discovery and will fail to resolve built-in kinds.
kubectl apply --dry-run=client -k deploy/k8s/overlays/staging
```
