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
| `namespace.yaml` | `fiber-boilerplate` namespace with `restricted` Pod Security labels |
| `configmap.yaml` | Non-secret production defaults and hostname/bucket placeholders |
| `secret.example.yaml` | Template for the real Secret; excluded from kustomize |
| `migration-job.yaml` | One-shot `/app/migrate` Job; applied separately, before the app |
| `deployment.yaml` | RollingUpdate Deployment (HPA owns replicas), probes, security context |
| `service.yaml` | ClusterIP on port 8080 |
| `hpa.yaml` | CPU-based HPA, min 2 / max 10 |
| `pdb.yaml` | `minAvailable: 1` |
| `networkpolicy.yaml` | Ingress from ingress-nginx; restricted egress; CNI probe caveat |
| `ingress.yaml` | nginx Ingress with TLS host placeholder |
| `kustomization.yaml` | App resources + image transformer; omits Job and `secret.example.yaml` |

## Prerequisites

- Kubernetes 1.29+ and `kubectl` (with built-in kustomize).
- A managed PostgreSQL 17 instance reachable over TLS. `DB_SSLMODE=require`
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

## 1. Configure the placeholders

At minimum, replace:

- `deployment.yaml`, `kustomization.yaml`, and `migration-job.yaml`:
  `ghcr.io/OWNER/REPO:latest`. The app kustomization's `images:` transformer
  rewrites the Deployment; the raw Job uses its literal line.
- `ingress.yaml`: `api.example.com` and `fiber-boilerplate-tls`.
- `configmap.yaml`: `APP_FRONTEND_URL`, `CORS_ALLOW_ORIGINS`, `DB_HOST`,
  `SMTP_HOST`, `EMAIL_FROM_ADDRESS`, `STORAGE_S3_BUCKET`, and the
  `APP_TRUSTED_PROXIES`/S3 endpoint values for your platform.
- `secret.example.yaml` (copied outside the repo): every `REPLACE_*` value.
- `networkpolicy.yaml`: the DB/Redis CIDRs and SMTP ports/ranges if your
  managed services are not in `10.0.0.0/8` or use different ports.

Find the remaining placeholders with:

```bash
grep -RInE 'OWNER/REPO|example\.com|REPLACE_|10\.0\.0\.0/8' deploy/k8s
```

Use an immutable release tag (for example `1.4.2`) in production, not
`:latest`. The app kustomization's `images:` transformer is the single source
for the Deployment image; the raw migration Job is outside that kustomization
by design, so keep its literal image line in sync when pinning a release. Both
manifests use `imagePullPolicy: Always` for the mutable `:latest` placeholder;
switch to `IfNotPresent` after pinning an immutable tag.

`APP_TRUSTED_PROXIES` must contain only the cluster pod/service CIDR that
contains ingress-nginx. Setting it to `0.0.0.0/0` lets clients spoof
`X-Forwarded-For` and bypass per-IP rate limits.

`APP_PORT` is part of the probe, Service, container port, and NetworkPolicy
contract. If you change it from 8080, update `containerPort`, the `http` named
port used by the probes, the Service port, the Ingress backend, and the
NetworkPolicy ingress port `8080`.

## 2. Create the namespace and Secret

```bash
kubectl apply -f deploy/k8s/namespace.yaml

cp deploy/k8s/secret.example.yaml /tmp/fiber-secret.yaml
# Edit /tmp/fiber-secret.yaml with real values, then:
kubectl apply -f /tmp/fiber-secret.yaml

kubectl apply -f deploy/k8s/configmap.yaml
```

Never commit the rendered Secret. For GitOps, use External Secrets, Sealed
Secrets, SOPS, or your cloud secret manager; `secret.example.yaml` is
deliberately not part of `kustomization.yaml`.

`JWT_SECRET`, `METRICS_AUTH_TOKEN`, and `ADMIN_PASSWORD` use short
`REPLACE_ME` placeholders on purpose. The API's production validation rejects
them, so an unedited template fails closed instead of starting with a known
secret.

The template leaves `ADMIN_EMAIL`/`ADMIN_PASSWORD` commented out, so no admin
user is bootstrapped. To create one on API startup, uncomment both values (and
optionally `ADMIN_NAME`); the password must be at least 12 characters. Setting
only one of them makes the API fail production config validation.

If the GHCR package is private, create a pull Secret and uncomment
`imagePullSecrets` in `deployment.yaml` (and add it to the migration Job if
that image is also private):

```bash
kubectl -n fiber-boilerplate create secret docker-registry ghcr-pull \
  --docker-server=ghcr.io \
  --docker-username=<github-user> \
  --docker-password=<PAT-with-read:packages>
```

## 3. Run the migration Job before the rollout

Only `cmd/migrate` is allowed to apply DDL. The API and seed commands never
migrate, so API replicas do not race on schema changes.

The app kustomization intentionally **excludes** `migration-job.yaml`, so
`kubectl apply -k deploy/k8s` cannot create the Job before the Deployment.
`kubectl apply -k` also has no cross-resource ordering guarantee. The only safe
flow is to apply the namespace/configmap/secret, apply and await the Job, and
only then apply the app kustomization:

```bash
kubectl apply -f deploy/k8s/namespace.yaml
kubectl apply -f deploy/k8s/configmap.yaml
# Create and apply the real Secret first (step 2).

kubectl -n fiber-boilerplate delete job fiber-boilerplate-migrate --ignore-not-found
kubectl apply -f deploy/k8s/migration-job.yaml
kubectl -n fiber-boilerplate wait \
  --for=condition=complete \
  --timeout=15m \
  job/fiber-boilerplate-migrate
kubectl -n fiber-boilerplate logs job/fiber-boilerplate-migrate
```

Do not add the Job back to `kustomization.yaml`; that reintroduces the ordering
ambiguity this split is meant to remove. The explicit `kubectl apply -f` path
uses the Job's literal image tag, so keep it in sync with the app's `images:`
transformer.

On failure:

```bash
kubectl -n fiber-boilerplate describe job fiber-boilerplate-migrate
```

The Job has `restartPolicy: Never`, `backoffLimit: 3`, and
`activeDeadlineSeconds: 900`. Jobs are immutable: delete the old Job before
applying a changed template. Only delete it after confirming it is complete and
no migration is running; deleting a running Job can interrupt DDL.
`ttlSecondsAfterFinished: 86400` removes a completed Job after 24 hours.

## 4. Deploy the application

Once the migration Job has completed:

```bash
kubectl apply -k deploy/k8s
kubectl -n fiber-boilerplate rollout status deployment/fiber-boilerplate --timeout=10m
```

This applies the Namespace, ConfigMap, Deployment, Service, HPA, PDB,
NetworkPolicy, and Ingress. The migration Job and Secret are not part of this
kustomization and are not re-applied here. If the Job was garbage-collected by
its TTL after you ran it, rerun the Job apply/wait in step 3 before this step.

The Deployment intentionally has no `spec.replicas`; the HPA's
`minReplicas: 2` owns the replica count. Do not add `replicas` back to the
Deployment, or the HPA and Deployment controllers will fight over it.

The Deployment uses `maxUnavailable: 0` and `maxSurge: 1`; a new pod must pass
`/readyz` before an old pod is removed. The in-process job workers use a
PostgreSQL queue with `SKIP LOCKED`, so running 2+ replicas is safe.

## 5. Verify

```bash
kubectl -n fiber-boilerplate get deploy,rs,pods,svc,hpa,pdb
kubectl -n fiber-boilerplate rollout status deployment/fiber-boilerplate --timeout=5m

kubectl -n fiber-boilerplate port-forward svc/fiber-boilerplate 8080:8080
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
```

Through the Ingress (replace the host):

```bash
curl -fsS https://api.example.com/healthz
curl -fsS https://api.example.com/readyz
```

`/healthz` is a process liveness check. `/readyz` also checks dependencies
such as PostgreSQL and Redis. The startup probe deliberately uses `/healthz`
(not `/readyz`) so a dependency outage during boot does not restartloop the pod;
once startup succeeds, `/readyz` gates traffic. If `kubectl get hpa` shows
`<unknown>` targets, install/fix metrics-server.

## 6. Rollback

```bash
kubectl -n fiber-boilerplate rollout history deployment/fiber-boilerplate
kubectl -n fiber-boilerplate rollout undo deployment/fiber-boilerplate
kubectl -n fiber-boilerplate rollout status deployment/fiber-boilerplate --timeout=10m
```

Database migrations are forward-only in this setup. A previous image may not
be compatible with a newer schema, so prefer backward-compatible migrations and
test rollback before production. `cmd/migrate` has no automatic down step in
the Kubernetes Job.

## Operational notes

### External PostgreSQL and Redis

The manifests do not deploy PostgreSQL or Redis. Use managed services with
TLS, automated backups/point-in-time recovery, and private networking.

- `DB_HOST`, `DB_PORT`, `DB_DATABASE`, `DB_USERNAME`, `DB_SCHEMA`, and
  `DB_SSLMODE` are in the ConfigMap; `DB_PASSWORD` is in the Secret.
- `DB_MAX_CONNS` is per pod. With HPA `maxReplicas: 10`, keep
  `DB_MAX_CONNS * 10` below the database connection limit. The template uses
  10, for at most 100 connections.
- `CACHE_DRIVER=redis` and `REDIS_URL` are required for multi-replica session
  invalidation. Use `rediss://` when the provider supports TLS.
- Update the Database/Redis CIDRs and ports in `networkpolicy.yaml` if they are
  not private `10.0.0.0/8` endpoints. A migration Job blocked by the
  NetworkPolicy will fail to connect.

### S3 versus local uploads

The template uses `STORAGE_DRIVER=s3` and `readOnlyRootFilesystem: true`. This
is the recommended multi-replica setup. Create the bucket before rollout: each
replica checks bucket existence at startup (and tries to create a missing
bucket, which can race across replicas).

The Deployment mounts a small bounded `emptyDir` at `/tmp` as scratch space so
the image root can stay read-only even if a dependency needs temp files. It is
not upload storage and is safe to lose on restart.

If you must use `STORAGE_DRIVER=local`:

- Mount a writable PVC at `/app/uploads` in the Deployment (the commented
  example in `deployment.yaml`).
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

1. All existing access and refresh tokens become invalid; users must log in
   again.
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

### Configuration changes

Environment variables are injected with `envFrom`, so editing the ConfigMap or
Secret does not update running pods. Restart the Deployment explicitly:

```bash
kubectl -n fiber-boilerplate rollout restart deployment/fiber-boilerplate
kubectl -n fiber-boilerplate rollout status deployment/fiber-boilerplate
```

Changing the Deployment pod template (including the image tag) already triggers
a rollout.

### Ingress, TLS, and network policy

- `ingress.yaml` assumes ingress-nginx. Replace `api.example.com`, provide the
  `fiber-boilerplate-tls` Secret, or enable the commented cert-manager
  annotation.
- `proxy-body-size` is 16 MiB, above `APP_BODY_LIMIT` (12 MiB) and
  `STORAGE_MAX_FILE_SIZE` (10 MiB).
- The NetworkPolicy allows ingress only from the `ingress-nginx` namespace. If
  your controller runs elsewhere, change the namespace label. A Prometheus rule
  for the `monitoring` namespace is included as a comment.
- **Kubelet probes are CNI-dependent.** Many CNIs allow node-originated
  readiness/liveness probes to the pod regardless of NetworkPolicy; some enforce
  policy and will drop them. `networkpolicy.yaml` includes a commented ingress
  rule for the node/node-pod CIDR: uncomment and set it if your CNI blocks
  probes. Do not widen it to `0.0.0.0/0`.
- Egress to HTTPS and SMTP is intentionally broad (`0.0.0.0/0`); tighten it
  with VPC endpoints/provider ranges. DB and Redis egress are limited to
  `10.0.0.0/8` by default.

### HPA and PDB

The HPA requires metrics-server. It scales on CPU at 70% utilization from a
minimum of 2 to a maximum of 10 replicas, and it owns the Deployment's replica
count; the Deployment intentionally has no `spec.replicas`. The PDB keeps at
least one API pod available during voluntary disruptions such as node drains.
With exactly 2 replicas, node drains must proceed one pod at a time.

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

The CI `Manifests` job runs `actionlint`, renders `deploy/k8s` with kustomize,
and runs kubeconform strict against Kubernetes 1.29, including the raw
migration Job and `secret.example.yaml`.

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
`deploy/k8s/kustomization.yaml`; the transformer updates the Deployment. The
migration Job is applied outside kustomize, so update its literal image tag to
the same immutable version. Use the exact version tag in production; the
major/minor and `latest` tags are mutable.

## Validation

```bash
# Parse every YAML file.
python3 - <<'PY'
import glob, yaml
for path in glob.glob("deploy/k8s/*.yaml") + [".github/workflows/release.yml"]:
    with open(path, encoding="utf-8") as f:
        list(yaml.safe_load_all(f))
    print("ok", path)
PY

# Render the kustomization (no cluster required).
kubectl kustomize deploy/k8s

# With a reachable cluster, perform a client-side dry run. Without a cluster
# this still needs API discovery and will fail to resolve built-in kinds.
kubectl apply --dry-run=client -k deploy/k8s
```
