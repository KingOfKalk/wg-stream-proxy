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

Image: `ghcr.io/kingofkalk/wg-stream-proxy` (public), base `distroless/static:nonroot` (UID 65532), `linux/amd64` + `linux/arm64`. Built from the [`Dockerfile`](../Dockerfile) by [`container.yml`](../.github/workflows/container.yml).

| Tag | Published on | Use |
|---|---|---|
| `X.Y.Z` (e.g. `0.1.0`) | release | pin this |
| `X.Y` (e.g. `0.1`) | release | latest patch of a minor version |
| `edge`, `sha-<commit>` | every push to `main` | unreleased, may break |

There is no `latest` tag: pin a version tag.

Retention: after every push to `main`, the `Prune` job keeps all `edge` and `X.Y.Z` / `X.Y` versions and the 10 newest `sha-<commit>` versions and deletes older ones. Images carry no provenance or SBOM attestations, so an index lists only `linux/amd64` and `linux/arm64`.

- Entry point `/wg-stream-proxy`, default command `serve`; other commands as arguments (`docker run --rm … check`).
- Config dir `/etc/wg-stream-proxy` (`WG_STREAM_PROXY_CONFIG_DIR`), port 2349.
- `HEALTHCHECK`: `wg-stream-proxy healthcheck` every 30s (every 1s for the first 30s). It loads the config like `serve` but does not see `serve` flags: set options in `config.toml` or as env vars (e.g. `WG_STREAM_PROXY_LISTEN`).
- Host files must be readable by UID 65532 (e.g. `chown -R 65532 config` and `chmod 640` on tunnel files and `signing.key`).

<!-- x-release-please-start-version -->
```bash
docker run -d --name wg-stream-proxy -p 2349:2349 \
  --read-only --cap-drop ALL \
  -v ./config:/etc/wg-stream-proxy:ro \
  -e WG_STREAM_PROXY_SIGNING_KEY_FILE=/etc/wg-stream-proxy/signing.key \
  ghcr.io/kingofkalk/wg-stream-proxy:0.1.0
```
<!-- x-release-please-end -->

Compose: [`deploy/compose/compose.yaml`](../deploy/compose/compose.yaml) (`read_only`, `cap_drop: [ALL]`, config mounted read-only, signing key file, healthcheck); put the config dir next to it as `./config`, then `docker compose up -d`.

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
