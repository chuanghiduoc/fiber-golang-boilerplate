# ---- Build stage ----
FROM golang:1.27.3-alpine AS build

RUN apk add --no-cache git

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
FROM alpine:3.24 AS prod

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
