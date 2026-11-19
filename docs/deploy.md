# wg-stream-proxy - Deployment

Companion to [spec.md](spec.md). All variants use the same config layout (spec 5.1) and env vars.

## Common rules

- Default paths: config dir `/etc/wg-stream-proxy`, tunnel files in `/etc/wg-stream-proxy/tunnels`.
- The process runs as a non-root user; tunnel files must be readable by it but not world-readable.
- Signing key via `WG_STREAM_PROXY_SIGNING_KEY_FILE` pointing to a secret file (spec 4). Generate with `openssl rand -base64 32`.
- Single instance: tunnel state lives in memory, and VPN providers often limit connections per key.
- Reload: the proxy checks `config.toml`, the channel files and the tunnel files every 2 s and reloads when they changed (`[reload] watch_interval`, `"0"` turns it off; spec 5.3 and 5.6); `SIGHUP` reloads at any time. `listen`, `debug.listen`, `log_format`, `reload.watch_interval`, the signing key and the tunnels dir location still need a restart. Where files are injected at start (systemd credentials, Kubernetes secrets that are copied), a change in the source needs a restart or a new copy.

## Standalone

There are no release binaries yet; release binaries are planned (Later, [spec 10](spec.md)). Build the binary from source (`go build ./cmd/wg-stream-proxy`) or install it with `go install` (Go 1.27.4 or newer, see `go.mod`); the container image below needs no Go toolchain.

<!-- x-release-please-start-version -->
```bash
go install github.com/KingOfKalk/wg-stream-proxy/cmd/wg-stream-proxy@v0.3.0
```
<!-- x-release-please-end -->

```bash
wg-stream-proxy check --config-dir ./config
wg-stream-proxy serve --config-dir ./config
```

## Docker and Compose

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
  ghcr.io/kingofkalk/wg-stream-proxy:0.3.0
```
<!-- x-release-please-end -->

Compose: [`deploy/compose/compose.yaml`](../deploy/compose/compose.yaml) (`read_only`, `cap_drop: [ALL]`, config mounted read-only, signing key file, healthcheck); put the config dir next to it as `./config`, then `docker compose up -d`.

- Reload: `docker kill -s HUP wg-stream-proxy`.
- LibreELEC 12+ on Raspberry Pi 4/5: the Docker add-on runs the `linux/arm64` image.

## Changes in Kodi

Kodi's IPTV Simple Client reads the M3U playlist at startup, or by its "Auto refresh mode" ([README](https://github.com/kodi-pvr/pvr.iptvsimple/blob/Omega/README.md)): "Repeated refresh" every N minutes (at most 120) or "Once per day". Kodi pulls the playlist; the proxy cannot tell it about changes. So after you save a channel file:

- Changes to an existing channel's upstream URL, route, headers or tunnel apply at once: Kodi only knows `/c/<id>`, and the proxy resolves the rest per request.
- New or removed channels, and changed names, groups, logos or EPG ids, appear in Kodi after its next refresh.

Recommended: in the IPTV Simple Client settings (General or Advanced, depending on the Kodi version) set "Auto refresh mode" to "Repeated refresh" with a "Refresh interval" of 60 minutes. Disabling the cache is a side effect of auto refresh (see the README), which is fine for a playlist on the LAN. Without it, restart the add-on or Kodi to see new channels.

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

## When a channel fails

An HLS channel (one listed with `.m3u8`) that fails to start plays a short error clip instead of Kodi's "Playback failed" dialog: about 5 s of a dark still with one line of text, then playback stops ([spec 3.4](spec.md)).

| Text | Meaning | Look at |
|---|---|---|
| Tunnel not reachable | The channel's tunnel did not start: no handshake within `connect_timeout` | The tunnel file, the VPN endpoint, `tunnel start failed` in the log |
| Channel blocked by configuration | The private-range guard or the tunnel's `AllowedIPs` refused the address | `[upstream.allow_private]`, `AllowedIPs` |
| Channel not available | Everything else: DNS, connect, TLS or timeout errors, an upstream error status (geo-block `403`, `404`, `5xx`), a playlist the proxy refused | The `warn` line of the request, with its `reason` and `clip` |

Radio and other channels listed without `.m3u8` still get an HTTP error, as do segment requests during playback. `[clips] error = false` in `config.toml` turns the clips off (applies on reload).

## Testing a channel URL

`probe` fetches a URL through a route the way the proxy does and says whether it would serve it, before you add a channel ([spec 7.2](spec.md)). It needs no shell: run it in the container like `tunnel list`. With `[debug] listen` set (see Debug endpoints below) the running proxy makes the requests, through its running tunnel: it works while the proxy plays channels and starts no second WireGuard device. The output says `via running proxy`. The proxy uses its loaded config, so after changing a channel, tunnel or `[upstream]` setting wait for the reload (file watching, or `SIGHUP`).

```bash
docker exec wg-stream-proxy /wg-stream-proxy probe --route gr 'https://cdn.example.com/live/master.m3u8'
docker exec wg-stream-proxy /wg-stream-proxy probe --route gr --header 'Referer=https://portal.example/' --json 'https://cdn.example.com/live/master.m3u8'
```

- Put the URL in single quotes: tokens in the query string contain `&`. The output shows the final URL as scheme, host and file name only, like the log.
- Exit code `0`: the proxy would serve it; `1`: it would not (the last lines say why: `guard`, `timeout`, `tls`, `status` for a `403` or `404`, `unsupported` for a playlist format the proxy refuses); `2`: wrong usage.
- Per request you see the format (HLS master or media playlist with the number of variants or segments, raw or ICY stream, refused playlist) and where the time went: tunnel start, DNS, connect, TLS, first byte. For HLS the first variant and the first segment are fetched too, which finds geo-blocks that only hit the segments.
- `--header Name=Value` is the same as `upstream.headers` of a channel (`User-Agent`, `Referer`, `Cookie`); once it works, copy the headers into the channel file.
- Without `[debug] listen`, or if nothing listens there, the probe runs standalone (`standalone` in the output, one line on stderr says so): it starts the route's tunnel itself and stops it at the end. That is a second device with the key of the tunnel, so while a proxy has that tunnel up, the provider's server switches to the new endpoint and a stream on that tunnel can stall until the next handshake. Prefer the debug listener; `--standalone` forces this mode (for a config the running proxy has not loaded yet, with no stream on that tunnel).

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

- The tunnels dir (`%d`) also contains `signing.key`; only `*.conf` files are read as tunnels (spec 5.1).
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
- Config changes: file watching (spec 5.6) picks up a mounted ConfigMap when Kubernetes swaps it; secrets copied at start need `kubectl rollout restart deployment/wg-stream-proxy`.
