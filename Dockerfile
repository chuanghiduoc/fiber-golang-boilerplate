# Base images are pinned by digest (tag kept for readability); Dependabot
# (docker ecosystem) proposes digest bumps.
# ---- Build stage ----
FROM golang:1.27.3-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS build

# No git: modules come from GOPROXY and -buildvcs=false skips VCS stamping.
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .

# docs/ is committed and checked for drift in CI; do not regenerate it here.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -trimpath \
    -buildvcs=false \
    -o server ./cmd/api && \
    CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -trimpath \
    -buildvcs=false \
    -o migrate ./cmd/migrate

# ---- Production stage ----
FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a AS prod

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S appgroup && \
    adduser -S appuser -G appgroup

WORKDIR /app

COPY --from=build /app/server ./server
COPY --from=build /app/migrate ./migrate
COPY --from=build /app/migrations ./migrations
COPY --from=build /app/docs ./docs

RUN mkdir -p /app/uploads && chown appuser:appgroup /app/uploads

USER appuser

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD wget -qO- "http://127.0.0.1:${APP_PORT:-8080}/readyz" >/dev/null || exit 1

CMD ["./server"]
