# Base images are pinned by digest (tag kept for readability); Dependabot
# (docker ecosystem) proposes digest bumps. The runtime is distroless/static:
# no shell, no package manager, no libc, and the nonroot user (65532). Go is
# built CGO_ENABLED=0 with -tags timetzdata, so the image needs no tzdata
# package either.
# ---- Build stage ----
FROM golang:1.27.3-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS build

# VERSION is the release tag or commit SHA. It is embedded in cmd/api and
# logged at startup; the image tag itself is not trusted at runtime.
ARG VERSION=dev

# No git: modules come from GOPROXY and -buildvcs=false skips VCS stamping.
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .

# docs/ is committed and checked for drift in CI; do not regenerate it here.
# -trimpath removes local paths, -tags timetzdata embeds the IANA database in
# the binary (the distroless static image ships none), and -buildvcs=false
# avoids a git dependency in the build context.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -tags timetzdata \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -trimpath \
    -buildvcs=false \
    -o server ./cmd/api && \
    CGO_ENABLED=0 GOOS=linux go build \
    -tags timetzdata \
    -ldflags="-s -w" \
    -trimpath \
    -buildvcs=false \
    -o migrate ./cmd/migrate && \
    CGO_ENABLED=0 GOOS=linux go build \
    -tags timetzdata \
    -ldflags="-s -w" \
    -trimpath \
    -buildvcs=false \
    -o seed ./cmd/seed && \
    mkdir -p /out/uploads

# ---- Production stage ----
# distroless/static:nonroot provides ca-certificates, /etc/passwd, /tmp, and
# runs as UID/GID 65532. No HEALTHCHECK shell is available, so the binary's
# own `healthcheck` subcommand is the probe (see compose and the k8s probes).
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS prod

WORKDIR /app

COPY --from=build /app/server ./server
COPY --from=build /app/migrate ./migrate
COPY --from=build /app/seed ./seed
COPY --from=build /app/migrations ./migrations
COPY --from=build /app/docs ./docs
# STORAGE_DRIVER=local writes uploads here; the local path must be writable by
# the nonroot user. S3 (the production default) needs no writable volume.
COPY --from=build --chown=65532:65532 /out/uploads ./uploads

USER 65532:65532

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD ["/app/server", "healthcheck"]

CMD ["./server"]
