# wg-stream-proxy - Deployment

Companion to [spec.md](spec.md). All variants use the same config layout (SPEC 5.1) and env vars.

## Common rules

- Default paths: config dir `/etc/wg-stream-proxy`, tunnel files in `/etc/wg-stream-proxy/tunnels`.
- The process runs as a non-root user; tunnel files must be readable by it but not world-readable.
- Signing key via `WG_STREAM_PROXY_SIGNING_KEY_FILE` pointing to a secret file (SPEC 4). Generate with `openssl rand -base64 32`.
- Single instance: tunnel state lives in memory, and VPN providers often limit connections per key.
- Reload: `SIGHUP` re-reads config and channel files (SPEC 5.6). Where files are injected at start (systemd credentials, Kubernetes without file watching), changes need a restart.

## Standalone (v0.1)

```bash
wg-stream-proxy check --config-dir ./config
wg-stream-proxy serve --config-dir ./config
```

## Docker and Compose (v0.1)

Image: `ghcr.io/kingofkalk/wg-stream-proxy`, base `distroless/static:nonroot` (UID 65532), `linux/amd64` + `linux/arm64`.

```bash
docker run -d --name wg-stream-proxy -p 2349:2349 \
  -v ./config:/etc/wg-stream-proxy:ro \
  -e WG_STREAM_PROXY_SIGNING_KEY_FILE=/etc/wg-stream-proxy/signing.key \
  ghcr.io/kingofkalk/wg-stream-proxy:latest
```

- Host files must be readable by UID 65532 (e.g. `chown -R 65532 config` and `chmod 640` on tunnel files and `signing.key`).

```yaml
# deploy/compose/compose.yaml
services:
  wg-stream-proxy:
    image: ghcr.io/kingofkalk/wg-stream-proxy:latest
    restart: unless-stopped
    read_only: true
    cap_drop: [ALL]
    ports: ["2349:2349"]
    volumes:
      - ./config:/etc/wg-stream-proxy:ro
    environment:
      WG_STREAM_PROXY_SIGNING_KEY_FILE: /etc/wg-stream-proxy/signing.key
    healthcheck:
      test: ["CMD", "/wg-stream-proxy", "healthcheck"]
      interval: 30s
      timeout: 5s
```

- Reload: `docker kill -s HUP wg-stream-proxy`.
- LibreELEC 12+ on Raspberry Pi 4/5: the Docker add-on runs the `linux/arm64` image.

## systemd (v0.3)

`deploy/systemd/wg-stream-proxy.service`:

```ini
[Service]
DynamicUser=yes
ProtectSystem=strict
ProtectHome=yes
NoNewPrivileges=yes
PrivateTmp=yes
# one credential per tunnel file, named <tunnel>.conf
LoadCredential=gr.conf:/etc/wg-stream-proxy/tunnels/gr.conf
LoadCredential=uk.conf:/etc/wg-stream-proxy/tunnels/uk.conf
LoadCredential=signing.key:/etc/wg-stream-proxy/signing.key
Environment=WG_STREAM_PROXY_TUNNELS_DIR=%d
Environment=WG_STREAM_PROXY_SIGNING_KEY_FILE=%d/signing.key
ExecStart=/usr/local/bin/wg-stream-proxy serve
ExecReload=/bin/kill -HUP $MAINPID
```

- The tunnels dir (`%d`) also contains `signing.key`; only `*.conf` files are read as tunnels (SPEC 5.1).
- Credentials are copied at service start, so changed or added tunnel files need `systemctl restart`. Channel files are re-read on `systemctl reload`.
- Adding a tunnel needs a new `LoadCredential=` line.

## launchd (v0.3)

`deploy/launchd/` plist running `wg-stream-proxy serve` as the user, config in `~/.config/wg-stream-proxy`.

## Kubernetes (v0.3)

Kustomize base in `deploy/k8s/`:

- `Deployment`: `replicas: 1`, `strategy: Recreate`, `runAsNonRoot`, `fsGroup: 65532`, `readOnlyRootFilesystem`, `capabilities.drop: [ALL]`, liveness and readiness probe `GET /healthz`.
- `ConfigMap` with `config.toml`, mounted at `/etc/wg-stream-proxy/config.toml` via `subPath`; second `ConfigMap` with channel files, mounted at `/etc/wg-stream-proxy/channels.d`.
- `Secret` with one key per tunnel (`gr.conf`, `uk.conf`), mounted at `/etc/wg-stream-proxy/tunnels`, `defaultMode: 0440`.
- `Secret` with the signing key, injected via `env.valueFrom.secretKeyRef` as `WG_STREAM_PROXY_SIGNING_KEY`.
- `Service` as `LoadBalancer` or `NodePort` for LAN players. Ingress optional, with response buffering disabled and long read timeouts.
- Config changes: `kubectl rollout restart deployment/wg-stream-proxy` (no file watching before v0.2).
