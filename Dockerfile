# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Container image for linux/amd64 and linux/arm64 (deploy.md).
#
# The builder stage runs on the build host's platform and cross-compiles a
# static binary for the target platform, so the final stage has no RUN step
# and multi-arch builds need no QEMU:
#
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     --build-arg VERSION="$(git describe --tags --always)" .

FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build

ARG TARGETOS
ARG TARGETARCH
# Reported by `wg-stream-proxy version` and /healthz; CI passes git describe.
ARG VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/wg-stream-proxy ./cmd/wg-stream-proxy

# Static base without shell or package manager; runs as nonroot (UID 65532).
FROM gcr.io/distroless/static:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931

ARG VERSION=dev
ARG REVISION=
LABEL org.opencontainers.image.title="wg-stream-proxy" \
      org.opencontainers.image.description="Routes IPTV streams through userspace WireGuard tunnels and serves them as one M3U8 playlist." \
      org.opencontainers.image.source="https://github.com/KingOfKalk/wg-stream-proxy" \
      org.opencontainers.image.url="https://github.com/KingOfKalk/wg-stream-proxy" \
      org.opencontainers.image.documentation="https://github.com/KingOfKalk/wg-stream-proxy/blob/main/docs/deploy.md" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"

COPY --from=build /out/wg-stream-proxy /wg-stream-proxy

# Mount the config dir here (read-only is fine); tunnel files in ./tunnels.
ENV WG_STREAM_PROXY_CONFIG_DIR=/etc/wg-stream-proxy
USER 65532:65532
EXPOSE 2349

# Runs `wg-stream-proxy healthcheck` (GET /healthz on the listen address).
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --start-interval=1s \
  CMD ["/wg-stream-proxy", "healthcheck"]

# Other commands: docker run … ghcr.io/kingofkalk/wg-stream-proxy:<tag> check
ENTRYPOINT ["/wg-stream-proxy"]
CMD ["serve"]
