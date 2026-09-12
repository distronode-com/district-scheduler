# syntax=docker/dockerfile:1

# ── Frontend build stage ───────────────────────────────────────────────────────
# Digests are the multi-arch manifest-list (OCI image index) digests, not a
# per-architecture one — docker-publish.yml builds linux/amd64 and linux/arm64 from
# this file, and pinning a single-arch digest would break the other arch at pull time.
# Get them with `crane digest <image>`; `docker inspect` on a pulled image gives the
# single-arch digest instead.
FROM node:22-alpine@sha256:c610fcdfb1d5b4740dd70c284ed3cb16bb857e0f7166196e36a5501df7a3aa32 AS frontend-builder

# Pin pnpm to the version in package.json's `packageManager` field (not @latest)
# so CI builds are reproducible and match the committed lockfile.
RUN corepack enable && corepack prepare pnpm@10.32.1 --activate

WORKDIR /app

COPY frontend/package.json frontend/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile

COPY frontend/ .
RUN pnpm build

# ── Go build stage ─────────────────────────────────────────────────────────────
FROM golang:1.26.6-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS builder

RUN apk add --no-cache ca-certificates wget

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=frontend-builder /app/build ./frontend/build

# Build natively for whatever architecture this stage is running on
# (TARGETARCH is auto-populated by Docker buildx; falls back correctly
# on ARM hosts where the original hardcoded amd64 would produce a
# binary that can't execute at all).
ARG TARGETARCH
# VERSION is passed by docker-publish.yml from the resolved image tag (semver on a
# release, "edge"/branch on a plain main push); defaults to "dev" for local builds
# so `go build` without --build-arg still works as documented in CLAUDE.md.
ARG VERSION=dev
# COMMIT is stamped explicitly because this image is built from a copied source tree
# with no .git, so the Go toolchain has no VCS metadata to embed and /version would
# otherwise report commit "unknown" on every container. That matters now that branch
# images are deployable: they report version "dev", leaving nothing else to identify
# the build by.
ARG COMMIT=""
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w -X github.com/calnode/calnode/internal/buildinfo.Version=${VERSION} -X github.com/calnode/calnode/internal/buildinfo.Commit=${COMMIT}" \
    -o calnode ./cmd/calnode

# Download Litestream for the deployment target, matching TARGETARCH.
#
# The release does not publish a checksum file, so the expected sha256 of each
# tarball is recorded here and verified before anything is extracted — without it
# this step runs whatever that URL happens to serve at build time. Bumping
# LITESTREAM_VERSION means recomputing both hashes.
ARG LITESTREAM_VERSION=0.3.13
ARG LITESTREAM_SHA256_AMD64=eb75a3de5cab03875cdae9f5f539e6aedadd66607003d9b1e7a9077948818ba0
ARG LITESTREAM_SHA256_ARM64=9585f5a508516bd66af2b2376bab4de256a5ef8e2b73ec760559e679628f2d59
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) expected="${LITESTREAM_SHA256_AMD64}" ;; \
      arm64) expected="${LITESTREAM_SHA256_ARM64}" ;; \
      *) echo "no recorded Litestream checksum for TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    wget -qO /tmp/litestream.tar.gz \
      "https://github.com/benbjohnson/litestream/releases/download/v${LITESTREAM_VERSION}/litestream-v${LITESTREAM_VERSION}-linux-${TARGETARCH}.tar.gz"; \
    echo "${expected}  /tmp/litestream.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/litestream.tar.gz -C /usr/local/bin litestream; \
    rm -f /tmp/litestream.tar.gz

# ── Runtime stage ─────────────────────────────────────────────────────────────
# alpine (not scratch) — needed for the shell entrypoint and Litestream.
# No --platform pin here: inherits the build host's native architecture,
# matching whatever TARGETARCH the binary above was actually compiled for.
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /usr/local/bin/litestream /usr/local/bin/litestream
COPY --from=builder /build/calnode /calnode
COPY litestream.yml /etc/litestream.yml
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Note: no `VOLUME` directive — persistent storage is provided by the platform's
# managed volume mounted at /data (Railway rejects the Docker VOLUME instruction;
# Fly mounts via fly.toml). The dir is created at runtime by entrypoint.sh.
EXPOSE 3000

ENV PORT=3000 \
    DATABASE_URL=sqlite:///data/calnode.db

ENTRYPOINT ["/entrypoint.sh"]
