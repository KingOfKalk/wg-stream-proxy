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
  ghcr.io/kingofkalk/wg-stream-proxy:0.2.0
```
<!-- x-release-please-end -->

Compose: [`deploy/compose/compose.yaml`](../deploy/compose/compose.yaml) (`read_only`, `cap_drop: [ALL]`, config mounted read-only, signing key file, healthcheck); put the config dir next to it as `./config`, then `docker compose up -d`.

- Reload: `docker kill -s HUP wg-stream-proxy`.
- LibreELEC 12+ on Raspberry Pi 4/5: the Docker add-on runs the `linux/arm64` image.

## Radio channels in Kodi

Kodi's IPTV Simple Client puts a channel into its Radio section only if the `#EXTINF` line has `radio="true"`. Set it through `playlist.attrs`, for a whole channel file with a file default (see [`examples/config/channels.d/radio.toml`](../examples/config/channels.d/radio.toml)):

```toml
[defaults.playlist]
attrs = { radio = "true" }
```

The playlist then has `#EXTINF:-1 tvg-name="Example Radio" group-title="Radio" radio="true",Example Radio`. `import` keeps `radio="true"` from an M3U as an attr.

## HLS channels in Kodi

By default Kodi's IPTV Simple Client hands HLS to Kodi's internal FFmpeg. On a Raspberry Pi 5 two HLS channels stalled after about 20 s that way, although the proxy answered every request. With `inputstream.adaptive` they played stably and started faster. If an HLS channel stalls or starts slowly in Kodi, hand it to that add-on through `playlist.extra`:

```toml
[channel.playlist.extra.KODIPROP]
"inputstream" = "inputstream.adaptive"
```

For a whole channel file, set it once as a file default; for all channel files, set the same table once in `channels.d/_defaults.toml` (see [`examples/config/channels.d/_defaults.toml`](../examples/config/channels.d/_defaults.toml)):

```toml
[defaults.playlist.extra.KODIPROP]
"inputstream" = "inputstream.adaptive"
```

The playlist then has `#KODIPROP:inputstream=inputstream.adaptive` before the channel's `#EXTINF` line (see [spec 5.4 and 6](spec.md)). The add-on must be installed and enabled in Kodi (VideoPlayer InputStream, InputStream Adaptive); the proxy does not check this.

Kodi cannot play MP3 radio streams through `inputstream.adaptive`. With the global default, remove it for radio channels with an empty value, in the radio file (as in [`examples/config/channels.d/radio.toml`](../examples/config/channels.d/radio.toml)) or on a single channel (`[channel.playlist.extra.KODIPROP]`):

```toml
[defaults.playlist.extra.KODIPROP]
"inputstream" = ""
```

An empty value removes a key set by a lower layer for `headers`, `attrs` and `extra` (spec 5.4).

## Monitoring

`/metrics` serves the timing histograms and the other `wsp_` metrics ([spec 7.3](spec.md)). Prometheus scrape config, useful queries (timing, error ratio, tunnel starts), example alerts and the Grafana dashboard: [observability.md](observability.md).

## Debug endpoints

Profiles, the last 256 requests with their timings, the tunnels' state and an on-demand cool-down of a tunnel, without restarting the proxy ([spec 7.1](spec.md)). Off by default; enable them in `config.toml` and restart (a reload does not open or move the listener):

```toml
[debug]
listen = "127.0.0.1:2350"
```

The endpoints have no authentication (spec 8): keep them on loopback, or at most on the LAN, never on a port reachable from the internet.

In Docker, loopback means inside the container: nothing is published, and the CLI reaches it through `docker exec` (the image has no shell or curl; the command reads `[debug] listen` like `healthcheck` reads `listen`):

```bash
docker exec wg-stream-proxy /wg-stream-proxy tunnel list
docker exec wg-stream-proxy /wg-stream-proxy tunnel stop gr
```

`tunnel stop` drains the tunnel (open streams finish, or are cut after 30 s), stops it and returns; the log shows `tunnel closed reason=cooldown`. The next request starts it cold, so a cold start can be measured without waiting for `idle_timeout`: stop the tunnel, play a channel, then read `tunnel started … duration=…` in the log or `tunnel_start_ms` in `/debug/requests`.

For `go tool pprof` from another machine, listen on all interfaces inside the container and publish the port on the host's LAN address only (here `192.168.1.10`):

```toml
[debug]
listen = "0.0.0.0:2350"
```

```bash
docker run … -p 2349:2349 -p 192.168.1.10:2350:2350 …
go tool pprof http://192.168.1.10:2350/debug/pprof/profile?seconds=30   # CPU, 30 s
go tool pprof http://192.168.1.10:2350/debug/pprof/heap                 # memory
curl -s http://192.168.1.10:2350/debug/requests                         # recent requests, JSON
```

Use the IP address, not a host name such as `libreelec.local`: the debug listener answers other host names with `403` (protection against DNS rebinding). In Compose, add `"192.168.1.10:2350:2350"` to `ports`. Remove the port again when done.

## systemd (Later)

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

## launchd (Later)

`deploy/launchd/` plist running `wg-stream-proxy serve` as the user, config in `~/.config/wg-stream-proxy`.

## Kubernetes (Later)

Kustomize base in `deploy/k8s/`:

- `Deployment`: `replicas: 1`, `strategy: Recreate`, `runAsNonRoot`, `fsGroup: 65532`, `readOnlyRootFilesystem`, `capabilities.drop: [ALL]`, liveness and readiness probe `GET /healthz`.
- `ConfigMap` with `config.toml`, mounted at `/etc/wg-stream-proxy/config.toml` via `subPath`; second `ConfigMap` with channel files, mounted at `/etc/wg-stream-proxy/channels.d`.
- `Secret` with one key per tunnel (`gr.conf`, `uk.conf`), mounted at `/etc/wg-stream-proxy/tunnels`, `defaultMode: 0440`.
- `Secret` with the signing key, injected via `env.valueFrom.secretKeyRef` as `WG_STREAM_PROXY_SIGNING_KEY`.
- `Service` as `LoadBalancer` or `NodePort` for LAN players. Ingress optional, with response buffering disabled and long read timeouts.
- Config changes: `kubectl rollout restart deployment/wg-stream-proxy` (no file watching before v0.3).
