# syntax=docker/dockerfile:1

# ---- Stage 1: build the React dashboard ----------------------------------
# Runs on the build host's native arch (output is arch-independent).
FROM --platform=$BUILDPLATFORM node:22-alpine AS frontend
WORKDIR /app/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
# Build the production bundle with Vite directly (esbuild). We intentionally skip
# the `tsc -b` typecheck that `npm run build` runs first — typechecking is a CI
# concern, not a prerequisite for emitting the shippable bundle.
# Vite's outDir is ../backend/web/dist, so the bundle lands in /app/backend/web/dist.
RUN npx --no-install vite build

# ---- Stage 2: compile the Go binary --------------------------------------
# Cross-compiles natively (CGO disabled, pure-Go sqlite) — no QEMU needed.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS backend
WORKDIR /app/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
# Overlay the freshly built dashboard so //go:embed picks up the real assets.
COPY --from=frontend /app/backend/web/dist ./web/dist
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/songguo ./cmd/songguo

# ---- Stage 3: minimal runtime --------------------------------------------
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata wget \
    && adduser -D -H -u 10001 songguo \
    && mkdir -p /data && chown songguo:songguo /data
COPY --from=backend /out/songguo /usr/local/bin/songguo

USER songguo
WORKDIR /data
VOLUME ["/data"]

ENV SONGGUO_LISTEN=:8080 \
    SONGGUO_DB=/data/songguo.db
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["songguo"]
