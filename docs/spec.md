# wg-stream-proxy - Specification

> Route individual IPTV streams through userspace WireGuard tunnels and serve them as one M3U8 playlist for Kodi, VLC and others.

- Repository: `github.com/KingOfKalk/wg-stream-proxy`, Go, AGPL-3.0-or-later
- Status: Draft v3
- Related: [deploy.md](deploy.md), [testing.md](testing.md)

## 1. Scope

### 1.1 Goals

- Route each channel through a chosen WireGuard tunnel, or explicitly `direct`. Nothing else on the host is affected.
- Multiple tunnels in one process, fully in userspace (no TUN device, no root, no kernel module).
- One extended M3U8 channel list for Kodi (IPTV Simple Client), VLC and other players.
- HLS passthrough with playlist rewriting; raw passthrough for radio and MPEG-TS.
- Fail closed: a channel never silently bypasses its tunnel.
- Migration from an existing M3U file via `import`.
- Capacity: 3 concurrent viewers (household use).

### 1.2 Non-goals

- Decryption, DRM licenses or keys inside the proxy (permanent). AES-128 HLS keys are only passed through.
- Transcoding, restreaming, DASH, Widevine passthrough, Kodi add-on, web UI, Xtream Codes, multi-user.

### 1.3 Platforms

- `linux/amd64`, `linux/arm64`, `darwin/arm64`; `CGO_ENABLED=0`; container image `linux/amd64` + `linux/arm64`.
- LibreELEC 12+ uses an aarch64 userland on Raspberry Pi 4/5 (since 12.0, May 2024), so `linux/arm64` binaries and containers (Docker add-on) run on default installs. Only 32-bit images (Raspberry Pi 2/3, Allwinner H3/R40, Rockchip RK3288) would need `linux/arm`, which is deferred.

## 2. Architecture

```
Player (Kodi / VLC)
   | HTTP (LAN)
wg-stream-proxy
   |-- /playlist.m3u8   channel list generator
   |-- /c/{id}          channel entry: type detection, HLS rewrite or raw pipe
   |-- /r/...           signed resources (sub-playlists, segments, keys)
   |-- routes           one per tunnel + "direct", each with http.Transport and private-range guard
   '-- tunnels          wireguard-go device + gVisor netstack per tunnel file
```

### 2.1 Tunnels

- One WireGuard device per tunnel file, shared by all channels and viewers using it (one key, one UDP session).
- Each tunnel has its own `http.Transport` dialing through the netstack: keep-alive, `ForceAttemptHTTP2 = true`, `MaxIdleConnsPerHost = 16`.
- DNS for upstream hosts is resolved inside the tunnel, using the `DNS` servers of the tunnel file.
- The `Endpoint` hostname is resolved by the host resolver when the tunnel starts.
- Lifecycle:
  - Lazy start on the first request; concurrent first requests share one start.
  - Closed after `[tunnels] idle_timeout` (default `60m`) without activity. Activity = any in-flight request or open stream.
  - Start failure or no handshake within `connect_timeout`: request fails with `502`; the next request retries.
  - On shutdown or reload removal: drain in-flight requests for max 30s, then close.
- `direct` uses a plain `http.Transport` on the host network with the same settings.

## 3. Stream handling

### 3.1 Request flow for `/c/{id}`

1. Fetch the channel URL through the channel's route.
2. Detect the type:
   1. `upstream.type = "hls"` or `"raw"` set: use it.
   2. `Content-Type` is `application/vnd.apple.mpegurl` or `application/x-mpegurl` (case-insensitive): HLS.
   3. Body starts with `#EXTM3U` (after optional UTF-8 BOM and whitespace): HLS.
   4. Otherwise: raw.
3. HLS: rewrite (3.2) and return. Raw: pipe (3.3).

Resources under `/r/` use the same detection (steps 2.2-2.4): playlists are rewritten, everything else is streamed with `io.Copy`.

### 3.2 HLS rewriting

- Stateless: master and media playlists are rewritten and returned; the player selects variants and renditions. Live and VOD are handled the same way.
- Rewritten:
  - Every URI line (non-empty, not starting with `#`).
  - Every `URI="..."` attribute in any tag.
- Only `http`, `https` and relative URIs are rewritten (relative ones resolved against the final URL after redirects). All other schemes (`data:`, `skd:`, ...) and all other lines stay byte for byte unchanged.
- AES-128 and SAMPLE-AES key URIs are rewritten like any URI; the player decrypts.
- Not supported: `EXT-X-DEFINE` variable substitution, LL-HLS `_HLS_*` query parameters (passed through unchanged, no guarantees).

### 3.3 Raw passthrough

- Upstream body piped with `io.Copy` and flush; no size or total time limit.
- Shoutcast v1 servers answer `ICY 200 OK`; the raw route's connection wrapper rewrites a leading `ICY ` status line to `HTTP/1.0 ` before Go's HTTP parser sees it.
- `Icy-MetaData` is forwarded from the client; `icy-*` response headers are copied back.

### 3.4 Upstream HTTP rules

- Request headers to upstream:
  - `User-Agent`: from merged `upstream.headers` if set, else `[upstream] user_agent`.
  - Merged `upstream.headers` (header names case-insensitive, canonicalised).
  - From the client only: `Range`, `If-Range`, `Icy-MetaData`. `Range` is not forwarded for URLs whose path ends in `.m3u8` or `.m3u`.
- `HEAD` requests: handled like `GET`, response headers relayed, upstream body closed immediately, no body sent.
- Response headers copied to the client: `Content-Type`, `Content-Range`, `Accept-Ranges`, `Cache-Control`, `Last-Modified`, `icy-*`; `Content-Length` and `ETag` only if the body is neither rewritten nor transparently decompressed.
- Rewritten playlists: `Content-Type: application/vnd.apple.mpegurl`, uncompressed, `Cache-Control: no-cache`, status `200` (a playlist answered with `206` is refetched without `Range`).
- Status mapping: upstream `2xx` and `4xx` passed through; upstream `5xx` and connection errors return `502`; timeouts return `504`.
- Limits: max 10 redirects (each hop goes through the guard), playlist body max 2 MiB, upstream URL max 4096 bytes.
- Timeouts (`[upstream]`): `connect_timeout` (10s), `header_timeout` until response headers (15s), `read_timeout` between body reads (30s, implemented as a resettable timer). No total timeout.

### 3.5 Private-range guard

- Applies to every upstream HTTP dial on every route, checked on the resolved IP at connect time (covers redirects and DNS rebinding).
- Not applied to: the WireGuard `Endpoint` (UDP, host side) and the tunnel's `DNS` servers.
- Blocked: loopback, RFC 1918, RFC 6598, link-local, IPv6 ULA, unspecified, multicast, IPv4-mapped forms of these. Public IPs and any port are allowed.
- Exceptions per route: `[upstream.allow_private] direct = ["192.168.1.0/24"]`, `gr = ["10.8.0.0/24"]`.

## 4. Resource URLs

- Format: `/r/{channel}/{sig}/{b64url}/{name}`
  - `{channel}`: channel id; route and headers always come from the channel config.
  - `{b64url}`: base64url (no padding) of the absolute upstream URL.
  - `{sig}`: base64url of the first 16 bytes of `HMAC-SHA256(key, channel + "\n" + upstream_url)`.
  - `{name}`: last path segment of the upstream URL, reduced to `[A-Za-z0-9._-]`, max 64 chars with the extension kept; fallback `data`. Ignored by the server, only for player format detection.
- Invalid signature, unknown or disabled channel: `404`. No expiry.
- Signing key: `WG_STREAM_PROXY_SIGNING_KEY`, or a file named by `WG_STREAM_PROXY_SIGNING_KEY_FILE`; raw bytes after trimming whitespace, minimum 32 bytes, else startup error. If neither is set: random key with a warning that resource URLs do not survive restarts. Generate with `openssl rand -base64 32`.
- `/c/{id}` URLs in the channel list are unsigned and stable.

## 5. Configuration

### 5.1 Directories and precedence

- Config dir: `--config-dir`, then `WG_STREAM_PROXY_CONFIG_DIR`, then `/etc/wg-stream-proxy` if it exists, then `$XDG_CONFIG_HOME/wg-stream-proxy` (fallback `~/.config/wg-stream-proxy`).
- Tunnel dir: `WG_STREAM_PROXY_TUNNELS_DIR`, default `<config-dir>/tunnels`.
- Read non-recursively: `<config-dir>/config.toml`, `<tunnels-dir>/*.conf`, `<config-dir>/channels.d/*.toml`; other files are ignored. Local logos: `<config-dir>/assets/logos/` (flat).
- Flag / env (CLI flag beats env beats `config.toml`): `--listen` / `WG_STREAM_PROXY_LISTEN`, `--log-level` / `WG_STREAM_PROXY_LOG_LEVEL`, `--log-format` / `WG_STREAM_PROXY_LOG_FORMAT`, `WG_STREAM_PROXY_PUBLIC_BASE_URL`.
- Invalid config at startup: exit code 1.

### 5.2 Names and conventions

- Tunnel names (file stem in tunnels dir) and channel ids are two separate namespaces; each must be unique within itself. Format `[a-z0-9-]`, 1-48 chars. `direct` is reserved as tunnel name.
- Route per channel, first match wins:
  1. `tunnel` on the channel
  2. `tunnel` at the top of the channel file
  3. A tunnel named like the channel file stem
  4. Otherwise: config error (fail closed). `direct` is only used when written explicitly.
- Group per channel: channel `group`, then file `group`, then file stem.
- Channel id default: slug of `name`:
  1. Greek letters mapped by this table (accents removed first): α a, β v, γ g, δ d, ε e, ζ z, η i, θ th, ι i, κ k, λ l, μ m, ν n, ξ x, ο o, π p, ρ r, σ ς s, τ t, υ y, φ f, χ ch, ψ ps, ω o.
  2. Unicode NFKD, combining marks removed, lowercase.
  3. Runs of characters outside `[a-z0-9]` become `-`; trim `-`; truncate to 48; trim `-` again.
  4. Empty result or duplicate id: config error.

### 5.3 `config.toml`

```toml
listen = ":2349"
log_level = "info"                    # debug | info | warn | error
log_format = "text"                   # text | json
public_base_url = ""                  # default: from Host / X-Forwarded-Proto / X-Forwarded-Host

[upstream]
user_agent = "Mozilla/5.0 (X11; Linux x86_64) Firefox/132.0"
connect_timeout = "10s"
header_timeout = "15s"
read_timeout = "30s"

[upstream.allow_private]              # optional, per route name
direct = []

[tunnels]
idle_timeout = "60m"

[epg]
url = "https://ext.greektv.app/epg/epg.xml.gz"
```

Unknown keys are config errors.

### 5.4 Channel files (`channels.d/*.toml`)

```toml
group = "Greek TV"                    # file defaults
tunnel = "gr"

[defaults.upstream]
headers = { User-Agent = "OKHTTP" }

[defaults.playlist]
attrs = { parent-code = "1234" }

[[channel]]
name = "ERT World"                    # required
url = "https://ertflix-ertworld.siliconweb.com/mpegts/618618_3479286/master_mpegts.m3u8"  # required
id = "ert-world"                      # optional, default: slug of name
tunnel = "gr"                         # optional
group = "Greek TV"                    # optional
enabled = true

[channel.upstream]
type = "auto"                         # auto | hls | raw
headers = { Referer = "https://www.ertflix.gr/" }

[channel.playlist]
tvg_id = "ertworld"
logo = "EPTWORLD"
attrs = { tvg-chno = "5" }

[channel.playlist.extra.KODIPROP]
"inputstream.adaptive.manifest_type" = "hls"   # dotted keys must be quoted

[[channel]]
name = "Das Erste"
tunnel = "direct"
url = "https://daserste-live.ard-mcdn.de/daserste/live/hls/de/master.m3u8"
```

- Sub-tables apply to the `[[channel]]` directly above.
- Merge order: `config.toml`, file `[defaults.*]`, channel. Maps (`headers`, `attrs`, `extra.<DIRECTIVE>`) merge key by key; the more specific layer wins. Single values are overridden.
- `playlist.attrs`: extra `#EXTINF` attributes. `tvg-id`, `tvg-name`, `tvg-logo`, `group-title` are reserved (config error).
- `playlist.extra.<DIRECTIVE>`: rendered as `#<DIRECTIVE>:<key>=<value>`.
  - Directive written as configured, must match `[A-Z][A-Z0-9-]*`.
  - Forbidden: `EXTM3U`, `EXTINF`, `EXTGRP` (conflicts with `group-title`), `EXT-X-*`.
  - No line breaks in keys or values.
- `playlist.logo`: `http(s)://` URL passed through; a file existing in `assets/logos/` is served as `/logos/{file}`; anything else is written as-is (player-side logo paths).

### 5.5 Tunnel files

`wg-quick` format, converted to UAPI internally.

- Honoured: `PrivateKey`, `Address` (IPv4/IPv6), `DNS` (IP entries; non-IP search domains ignored with a warning), `MTU` (default 1420), `PublicKey`, `PresharedKey`, `Endpoint`, `AllowedIPs`, `PersistentKeepalive`.
- `AllowedIPs` other than `0.0.0.0/0, ::/0` restricts which upstream IPs are reachable; others fail (fail closed).
- Ignored with a warning: `ListenPort`, `Table`, `SaveConfig`.
- Config errors: more than one `[Peer]`; `PreUp`, `PostUp`, `PreDown`, `PostDown`; two tunnel files with the same `PrivateKey`.
- Warning if the file is world-readable.

### 5.6 Reload

- `SIGHUP` reloads all files; the new config is validated completely, an invalid one is rejected and the old one keeps serving.
- Unchanged tunnels keep running; changed or removed tunnels are drained (2.1) and recreated lazily.
- Restart required for: `listen`, signing key, tunnels dir location.

## 6. Channel list

- `GET /playlist.m3u8`, `Content-Type: audio/x-mpegurl; charset=utf-8`, no BOM; disabled channels omitted; optional `?group=<name>` (exact match).
- Header: `#EXTM3U x-tvg-url="<epg.url>" url-tvg="<epg.url>"` if `epg.url` is set (EPG link mode: the player fetches it).
- Per channel:
  1. `playlist.extra` lines, sorted by directive, then key.
  2. `#EXTINF:-1 tvg-id=".." tvg-name=".." tvg-logo=".." group-title=".."`, then `attrs` sorted by key, then `,<name>`. Empty values are omitted; `"` in values and names is replaced by `'`. Commas in names are fine (IPTV Simple Client takes the name after the last quoted attribute).
  3. URL: `/c/{id}.m3u8` if `upstream.type = "hls"` or the upstream URL path contains `.m3u8`, otherwise `/c/{id}`. The handler strips a trailing `.m3u8` from the id.
- `#KODIPROP` and other `extra` lines appear only here, never in playlists under `/c/` or `/r/`.

```
#EXTM3U x-tvg-url="https://ext.greektv.app/epg/epg.xml.gz" url-tvg="https://ext.greektv.app/epg/epg.xml.gz"
#KODIPROP:inputstream.adaptive.manifest_type=hls
#EXTINF:-1 tvg-id="ertworld" tvg-name="ERT World" tvg-logo="EPTWORLD" group-title="Greek TV" parent-code="1234" tvg-chno="5",ERT World
http://wsp.lan:2349/c/ert-world.m3u8
```

## 7. Interfaces

### 7.1 HTTP

| Path | Description |
|---|---|
| `GET/HEAD /playlist.m3u8` | Channel list |
| `GET/HEAD /c/{id}`, `/c/{id}.m3u8` | Channel entry |
| `GET/HEAD /r/{channel}/{sig}/{b64url}/{name}` | Signed resource |
| `GET /logos/{file}` | Local logo, flat directory, no traversal |
| `GET /healthz` | 200 while serving; body: version and source URL (AGPL section 13) |
| `GET /metrics` | Prometheus |

Absolute URLs in responses use `public_base_url`, else `X-Forwarded-Proto` / `X-Forwarded-Host`, else the request `Host`.

### 7.2 CLI

```
wg-stream-proxy serve                 # default
wg-stream-proxy check                 # validate config, exit 0/1
wg-stream-proxy import <file.m3u> [--tunnel T] [--match-epg] [--out FILE]
wg-stream-proxy epg ids               # list id and display-name from epg.url
wg-stream-proxy epg match             # suggest tvg_id per channel, print TOML, write nothing
wg-stream-proxy healthcheck           # GET /healthz on the listen address, exit 0/1
wg-stream-proxy version
```

`import`:
- `tvg-id`, `tvg-logo`, `group-title` to their fields, `tvg-name` dropped (generated from `name`), other `#EXTINF` attributes to `playlist.attrs`.
- `#KODIPROP:inputstream.adaptive.stream_headers=`, `#EXTVLCOPT:http-user-agent=`, `#EXTVLCOPT:http-referrer=`, `url|Header=Value` to `upstream.headers`.
- Other `#<DIRECTIVE>:key=value` lines to `playlist.extra.<DIRECTIVE>` (keys quoted); non-matching lines dropped with a warning.
- Values shared by all channels of the output are written once as `[defaults.*]`.
- Commented-out URLs (`#http...`) become `enabled = false`.
- `--match-epg`: keep `tvg_id` if it exists in the EPG, else add the best suggestion as a TOML comment.

`epg match`: names normalised (5.2 slug steps 1-2, alphanumerics only), EPG display names ranked by similarity.

### 7.3 Metrics (prefix `wsp_`)

- `tunnel_up{tunnel}`: 1 while the device runs and the last handshake is younger than 180s.
- `tunnel_last_handshake_timestamp_seconds{tunnel}`
- `active_requests{channel,kind}`: in-flight requests incl. open raw streams; `kind` = `playlist`, `segment`, `raw`.
- `upstream_requests_total{channel,kind,code}`: `code` = upstream status, or `error`.
- `upstream_errors_total{channel,reason}`: `reason` = `connect`, `timeout`, `guard`, `tls`, `redirects`, `size`, `other`.

## 8. Operations and security

- Intended for LAN use; exposure to the internet needs an authenticating reverse proxy.
- WireGuard keys only in tunnel files and never logged; upstream URLs logged without query strings.
- Container: non-root, read-only root filesystem, all capabilities dropped, single replica. Details: [deploy.md](deploy.md).

## 9. Engineering rules

- Layout: `cmd/wg-stream-proxy/`, `internal/{config,tunnel,route,hls,playlist,server,importer,epg,metrics}`, `examples/config/`, `testdata/`, `deploy/`, `docs/`.
- Dependencies: stdlib `net/http`, `log/slog`, `wireguard-go`, gVisor netstack, a TOML parser, Prometheus client. No web framework.
- gVisor stays at the version in `wireguard-go`'s `go.mod` and is only updated together with it (excluded from automatic bumps).
- Injected for tests: `Dialer` per route, private-range guard allowlist, clock.
- Client disconnect cancels the upstream request. HTTP server: `ReadHeaderTimeout` set, no `WriteTimeout`.
- `atomic.Int64` and similar types only, never `atomic.AddInt64` on plain fields.
- Code comments in English; file header `// SPDX-License-Identifier: AGPL-3.0-or-later`.

## 10. Roadmap

- **v0.1 Core:** everything in this spec, plus Dockerfile, Compose file and the v0.1 test layers in [testing.md](testing.md). Publishes the container image `ghcr.io/kingofkalk/wg-stream-proxy` (`linux/amd64` + `linux/arm64`, public package): tag `edge` on every push to `main`, version tags on release.
- **v0.2 Experience:** placeholder and error clips with seamless switchover (own design doc), per-request prefetch, file-watch reload.
- **v0.3 Distribution:** GoReleaser, systemd unit, launchd plist, Kustomize.
- **Later:** 32-bit ARM, scaling beyond 3 viewers, EPG proxy mode, logo proxying, per-tunnel overrides, ad-hoc proxy endpoint, DASH.

## 11. Risks

- WireGuard encryption is CPU-bound on small ARM boards; 3 HD streams are expected to fit on a Raspberry Pi 5 (tested) and a Raspberry Pi 4 (no device available for testing, performance unmeasured).
- Geo-IP databases may disagree on a tunnel's exit country; a broadcaster can see a different country than expected and block the stream.
- Some upstreams need cookies or tokens from a website session; only static headers are supported.

Prior art: wireproxy (userspace WireGuard proxy, one tunnel per process), MediaFlow Proxy (HLS proxy with per-domain proxy routing), Dispatcharr (IPTV manager with one VPN via Gluetun). None selects a tunnel per channel inside one process.
