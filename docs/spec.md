# wg-stream-proxy - Specification

> Route individual IPTV streams through userspace WireGuard tunnels and serve them as one M3U8 playlist for Kodi, VLC and others.

- Repository: `github.com/KingOfKalk/wg-stream-proxy`, Go, AGPL-3.0-or-later
- Status: Draft v3
- Related: [architecture.md](architecture.md), [deploy.md](deploy.md), [testing.md](testing.md)

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
- Each tunnel has its own `http.Transport` dialing through the netstack: keep-alive, `ForceAttemptHTTP2 = true`, `MaxIdleConnsPerHost = 16`, no environment proxy.
- IPv4 and IPv6 addresses of a host are tried Happy-Eyeballs style (RFC 8305, simplified): families interleaved starting with the first DNS answer; the next address starts after 300ms or as soon as the previous attempt fails; the first connection wins. A tunnel with `::/0` but no working IPv6 upstream thus falls back to IPv4 after 300ms.
- DNS for upstream hosts is resolved inside the tunnel, using the `DNS` servers of the tunnel file. A and AAAA (only for address families the tunnel has) are queried in parallel. The first attempt of a query goes to every `DNS` server at once; the first answer that is not an error wins. An unanswered query is repeated after 0.5s, 1s, 2s, then every 4s, each time to the next `DNS` server, until `connect_timeout`. An error answer (e.g. `SERVFAIL`) to a repeat asks the next server at once; a server that answered with an error is not asked again, and the query fails once every server did. A truncated answer is repeated over TCP, NXDOMAIN ends the lookup at once.
- Tunnel DNS answers are cached per tunnel, host name and record type: for the smallest TTL of the records used (CNAME chain included), at least 10s and at most 5min; an answer without records of the type for the negative TTL of its SOA record (RFC 2308, same bounds, 10s without SOA); NXDOMAIN for 10s. Error answers and unanswered queries are not cached. At most 256 entries per tunnel (when full, expired entries go first, then the one expiring soonest). The cache is emptied whenever the tunnel stops, so a restarted tunnel resolves again. Concurrent lookups of the same host share one query; a caller whose deadline ends stops waiting without cancelling it for the others.
- Tunnel DNS results list IPv4 first; an IPv6 connect through a tunnel that also carries IPv4 gives up after 2s.
- The `Endpoint` hostname is resolved by the host resolver when the tunnel starts (IPv4 preferred).
- Lifecycle:
  - Lazy start on the first request; concurrent first requests share one start.
  - Closed after `[tunnels] idle_timeout` (default `60m`) without activity. Activity = any in-flight request or open stream; the idle time runs from the end of the last one. Pooled idle upstream connections are not activity and close with the tunnel; the next request starts it again.
  - Start failure or no handshake within `[upstream] connect_timeout`: request fails with `502`; the next request retries. Start, tunnel DNS and connect share one `connect_timeout` deadline.
  - On shutdown or reload removal: drain in-flight requests for max 30s, then close.
- `direct` uses a plain `http.Transport` on the host network with the same settings.

## 3. Stream handling

### 3.1 Request flow for `/c/{id}`

1. Fetch the channel URL through the channel's route.
2. Detect the type:
   1. `upstream.type = "hls"` or `"raw"` set: use it.
   2. `Content-Type` is `application/vnd.apple.mpegurl` or `application/x-mpegurl` (case-insensitive, parameters ignored): HLS.
   3. Body starts with `#EXTM3U` (after optional UTF-8 BOM and whitespace, within the first 512 bytes): HLS.
   4. Otherwise: raw.
3. HLS: rewrite (3.2) and return. Raw: pipe (3.3).

Resources under `/r/` use the same detection (steps 2.2-2.4): playlists are rewritten, everything else is streamed with `io.Copy`.

### 3.2 HLS rewriting

- Stateless: master and media playlists are rewritten and returned; the player selects variants and renditions. Live and VOD are handled the same way.
- Rewritten:
  - Every URI line (non-empty, not starting with `#`).
  - Every `URI="..."` attribute in any tag, including names ending in `-URI` (`SERVER-URI`, `X-ASSET-URI`, ...).
- Only `http`, `https` and relative URIs are rewritten (relative ones resolved against the final URL after redirects). All other schemes (`data:`, `skd:`, ...) and all other lines stay byte for byte unchanged.
- AES-128 and SAMPLE-AES key URIs are rewritten like any URI; the player decrypts.
- Lines end at LF, CRLF or a lone CR (as ffmpeg reads them). Tags are lines starting with `#EXT`; other lines starting with `#` are comments. `#EXTINF` has no attribute list and is never rewritten. Other tags: attributes may be separated by commas and/or whitespace; the list is read up to the first element that is not `NAME=VALUE`. Blank lines, line endings, a leading BOM and whitespace around URI lines are kept.
- Fail closed: the whole playlist is rejected (`502`) instead of leaving a URL the player could fetch directly, if
  - an `http`, `https` or relative URI does not parse, or resolves to an upstream URL longer than 4096 bytes;
  - it contains a NUL byte (a line end for ffmpeg);
  - a tag other than `#EXTINF` contains `URI=` followed by an `http(s)` or scheme-relative URL outside a recognised attribute (unquoted value, escaped quote, broken attribute list): players parse such lines differently.
- Not supported: `EXT-X-DEFINE` variable substitution, LL-HLS `_HLS_*` query parameters (passed through unchanged, no guarantees).

### 3.3 Raw passthrough

- Upstream body piped with `io.Copy` and flush; no size or total time limit.
- Shoutcast v1 servers answer `ICY 200 OK`; the raw route's connection wrapper rewrites a leading `ICY ` status line to `HTTP/1.0 ` before Go's HTTP parser sees it.
  - Used for `/c/` requests of `raw` and `auto` channels (the type of `auto` is only known from the response; the wrapper leaves `HTTP/` responses unchanged). These connections are not reused (one request per connection); `hls` channels and `/r/` resources use the normal pool.
  - Only the first bytes of each connection are checked, so a connection carries one request: an ICY response has no length and ends with its connection, and a server may answer one port with ICY or HTTP per path. Over `https` the wrapper sees TLS records and changes nothing (Shoutcast v1 has no TLS).
- `Icy-MetaData` is forwarded from the client; `icy-*` response headers are copied back.

### 3.4 Upstream HTTP rules

- Request headers to upstream:
  - `User-Agent`: from merged `upstream.headers` if set, else `[upstream] user_agent`; none if a layer removed it with an empty value (5.4).
  - Merged `upstream.headers` (header names case-insensitive, canonicalised).
  - From the client only: `Range`, `If-Range`, `Icy-MetaData`. `Range` is not forwarded for URLs whose path ends in `.m3u8` or `.m3u` (case-insensitive, query ignored).
  - The same rules apply on every redirect hop; no automatic `Referer`.
- `HEAD` requests: handled like `GET` (the upstream request is always `GET`), response headers relayed, upstream body closed immediately, no body sent; `auto` type detection uses only the `Content-Type`.
- Response headers copied to the client: `Content-Type`, `Content-Range`, `Accept-Ranges`, `Cache-Control`, `Last-Modified`, `icy-*`; `Content-Length` and `ETag` only if the body is neither rewritten nor transparently decompressed.
- Rewritten playlists: `Content-Type: application/vnd.apple.mpegurl`, uncompressed, `Cache-Control: no-cache`, status `200` (a playlist answered with `206` is refetched without `Range` and `If-Range`; another `206` gives `502`).
- Status mapping: upstream `2xx` and `4xx` passed through; upstream `5xx`, other statuses (e.g. a `3xx` that is not followed) and connection errors return `502`; timeouts return `504`.
- Limits: max 10 redirects (`301`, `302`, `303`, `307`, `308` with `Location`; each hop goes through the guard), playlist body max 2 MiB, upstream URL max 4096 bytes (redirect targets included).
- Timeouts (`[upstream]`): `connect_timeout` (10s), `header_timeout` until response headers (15s), `read_timeout` between body reads (30s, implemented as a resettable timer). No total timeout.
  - `connect_timeout` covers DNS lookup and TCP connect of one connection; running out counts as a connection error (`502`), like a tunnel without handshake (2.1).
  - `header_timeout` runs from the start of the request until the final response headers, across redirects; `504`.
  - `read_timeout` runs only while waiting for upstream data, not while the client is slow to accept it; `504` if it fires before the response starts, else the stream ends.

### 3.5 Private-range guard

- Applies to every upstream HTTP dial on every route, checked on the resolved IP at connect time (covers redirects and DNS rebinding).
- Not applied to: the WireGuard `Endpoint` (UDP, host side) and the tunnel's `DNS` servers.
- Blocked: loopback, RFC 1918, RFC 6598, link-local, IPv6 ULA, unspecified (`0.0.0.0/8`, `::`), multicast, IPv4-mapped forms of these. Public IPs and any port are allowed.
- Exceptions per route: `[upstream.allow_private] direct = ["192.168.1.0/24"]`, `gr = ["10.8.0.0/24"]`.
- Blocked addresses of a DNS answer are skipped; if none is left, the request fails like a connection error with `502` (metrics reason `guard`).

## 4. Resource URLs

- Format: `/r/{channel}/{sig}/{b64url}/{name}`
  - `{channel}`: channel id; route and headers always come from the channel config.
  - `{b64url}`: base64url (no padding) of the absolute upstream URL.
  - `{sig}`: base64url of the first 16 bytes of `HMAC-SHA256(key, channel + "\n" + upstream_url)`.
  - `{name}`: last path segment of the upstream URL, reduced to `[A-Za-z0-9._-]`, max 64 chars with the extension kept; fallback `data` (also for a name of only dots). Ignored by the server, only for player format detection.
- Invalid signature, unknown or disabled channel: `404`. Only the canonical encoding is accepted (no padding, no line breaks, zero trailing bits). No expiry.
- Signing key: `WG_STREAM_PROXY_SIGNING_KEY`, or a file named by `WG_STREAM_PROXY_SIGNING_KEY_FILE`; raw bytes after trimming leading and trailing ASCII whitespace, minimum 32 bytes, else startup error. An empty variable counts as unset; setting both is a startup error. If neither is set: random key with a warning that resource URLs do not survive restarts. Generate with `openssl rand -base64 32`.
- `/c/{id}` URLs in the channel list are unsigned and stable.

## 5. Configuration

### 5.1 Directories and precedence

- Config dir: `--config-dir`, then `WG_STREAM_PROXY_CONFIG_DIR`, then `/etc/wg-stream-proxy` if it exists, then `$XDG_CONFIG_HOME/wg-stream-proxy` (fallback `~/.config/wg-stream-proxy`).
- Tunnel dir: `WG_STREAM_PROXY_TUNNELS_DIR`, default `<config-dir>/tunnels`.
- Read non-recursively: `<config-dir>/config.toml`, `<tunnels-dir>/*.conf`, `<config-dir>/channels.d/*.toml` (channel files, plus the optional global layer `channels.d/_defaults.toml`, 5.4); other files are ignored. A missing `config.toml`, tunnels dir or `channels.d` counts as empty (defaults, no tunnels, no channels); a missing config dir is an error. Channel order: by file name, then by position in the file. Local logos: `<config-dir>/assets/logos/` (flat).
- Flag / env (CLI flag beats env beats `config.toml`): `--listen` / `WG_STREAM_PROXY_LISTEN`, `--log-level` / `WG_STREAM_PROXY_LOG_LEVEL`, `--log-format` / `WG_STREAM_PROXY_LOG_FORMAT`, `WG_STREAM_PROXY_PUBLIC_BASE_URL`. Empty values count as not set.
- Invalid config at startup: exit code 1. All errors found are reported, each with its file (and line for TOML errors).

### 5.2 Names and conventions

- Tunnel names (file stem in tunnels dir) and channel ids are two separate namespaces; each must be unique within itself. Format `[a-z0-9-]`, 1-48 chars. `direct` is reserved as tunnel name. A `*.conf` file with an invalid or reserved stem is a config error.
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
url = "https://ext.greektv.app/epg/epg.xml.gz"  # optional, no default
```

Unknown keys and wrong value types are config errors. Values are validated: `listen` is `[host]:port` with a numeric port; durations are positive; `public_base_url` and `epg.url` are empty or absolute `http(s)` URLs; `user_agent` has no line breaks; `allow_private` keys are `direct` or existing tunnel names, values are CIDR prefixes without host bits.

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
- Merge order: `config.toml`, `channels.d/_defaults.toml`, file `[defaults.*]`, channel. Maps (`headers`, `attrs`, `extra.<DIRECTIVE>`) merge key by key; the more specific layer wins (header names case-insensitively). Single values are overridden. `[defaults.upstream]` and `[defaults.playlist]` take the same keys as the channel. `[upstream] user_agent` is the `config.toml` layer of `headers` (as `User-Agent`).
- Opt-out: an empty value in `headers`, `attrs` or `extra.<DIRECTIVE>` removes the key set by a lower layer (and is not set itself): the header is not sent (for `User-Agent` also not `[upstream] user_agent`), the attribute or extra line is not written. A more specific layer can set the key again. `logo = ""` gives no logo.
- `channels.d/_defaults.toml` (optional): the global layer for all channel files, e.g. one `inputstream.adaptive` for every channel (deploy.md). It takes only `[defaults.upstream]` (`type`, `headers`) and `[defaults.playlist]` (`logo`, `attrs`, `extra.<DIRECTIVE>`), validated like file defaults. `tunnel` (a global route would bypass fail closed, 5.2), `group`, `[[channel]]` and `tvg_id` are config errors. It is not a channel file: no channels, no file-stem route. Other `channels.d/_*.toml` names are reserved (config error).
- Channel `url`: absolute `http(s)` URL, max 4096 bytes. `name`: required, not blank.
- `upstream.headers`: names are HTTP tokens, values without CR, LF or NUL; two names in one layer that differ only in case are a config error.
- No line breaks in `name`, `group`, `playlist.tvg_id`, `playlist.logo` and `attrs` values.
- `playlist.attrs`: extra `#EXTINF` attributes. `tvg-id`, `tvg-name`, `tvg-logo`, `group-title` are reserved in any case (config error). Names must not be empty or contain whitespace, control characters, `"`, `=` or `,`.
- `playlist.extra.<DIRECTIVE>`: rendered as `#<DIRECTIVE>:<key>=<value>`.
  - Directive written as configured, must match `[A-Z][A-Z0-9-]*`.
  - Forbidden: `EXTM3U`, `EXTINF`, `EXTGRP` (conflicts with `group-title`), `EXT-X-*`.
  - No line breaks in keys or values; keys must not be empty.
- `playlist.logo`: `http(s)://` URL (scheme case-insensitive) passed through; a plain file name (no `/` or `\`) existing in `assets/logos/` is served as `/logos/{file}`; anything else is written as-is (player-side logo paths).
  - `GET/HEAD /logos/{file}` serves any regular file directly in `<config-dir>/assets/logos/`, not only the names channels use, so a logo added while running works. `{file}` is one path segment: empty, dot-leading (`.`, `..`, hidden files), over 255 bytes, or containing `/`, `\` or a control character gives `404`, as does a missing logos dir, a directory, or a symbolic link whose target is outside the logos dir (links inside it work; the logos dir itself may be a link). Encoded forms are decoded once, then checked like plain ones.
  - `Content-Type` by extension (`png`, `jpg`/`jpeg`, `gif`, `webp`, `svg`, `ico`, `avif`; else the system type or `application/octet-stream`), `X-Content-Type-Options: nosniff`, a restrictive `Content-Security-Policy` (no scripts in SVG), `Cache-Control: public, max-age=3600`, `Last-Modified` and conditional and range requests.

### 5.5 Tunnel files

`wg-quick` format, converted to UAPI internally.

- Honoured: `PrivateKey`, `Address` (IPv4/IPv6), `DNS` (IP entries; non-IP search domains ignored with a warning), `MTU` (default 1420), `PublicKey`, `PresharedKey`, `Endpoint`, `AllowedIPs`, `PersistentKeepalive`.
- `AllowedIPs` other than `0.0.0.0/0, ::/0` restricts which upstream IPs are reachable; others fail (fail closed).
- Ignored with a warning: `ListenPort`, `Table`, `SaveConfig`.
- Config errors: more than one `[Peer]`; `PreUp`, `PostUp`, `PreDown`, `PostDown`; two tunnel files with the same `PrivateKey`.
- Warning if the file is world-readable.
- Format details:
  - `#` starts a comment anywhere in a line; keys and section names are case-insensitive; a UTF-8 BOM and CRLF line endings are accepted.
  - `Address`, `DNS` and `AllowedIPs` may repeat; values are separated by commas and/or whitespace. `Address` and `AllowedIPs` entries without prefix length are host prefixes. Every other key may appear once.
  - Required: one `[Interface]` with `PrivateKey` and `Address`, one `[Peer]` with `PublicKey`, `Endpoint` and `AllowedIPs`. Without an IP `DNS` entry: warning, upstream host names cannot be resolved.
  - Config errors also: unknown keys or sections, empty values, invalid values, zero `PrivateKey` or `PublicKey`, `MTU` outside 1280-65535, files over 64 KiB.
  - Errors and warnings name the tunnel and line, never values from the file.

### 5.6 Reload

- `SIGHUP` reloads all files; the new config is validated completely (tunnel files included), an invalid one is rejected with all errors logged and the old one keeps serving. Reloads run one at a time.
- Requests in flight finish with the config they started with; new requests use the new one.
- Unchanged tunnels (same parsed content; comments and formatting do not count) keep running; changed or removed tunnels are drained (2.1) and recreated lazily.
- A route (`direct` or a tunnel) keeps its pooled upstream connections only if its network, its `allow_private` entries and the whole `[upstream]` block are unchanged; otherwise new requests use a new route and the old route's idle connections are closed, while an unchanged tunnel keeps running.
- A reload that changes or removes a tunnel which is still starting cancels the start; requests waiting for it get `502`; the new tunnel starts lazily on the next request.
- Restart required for: `listen`, `log_format`, signing key, tunnels dir location. A reload that changes one of them logs a warning and keeps the old value.
- A successful reload logs one info line `reloaded` with the channel and tunnel counts, `channels_added`, `channels_changed` and `channels_removed` (counts; changed means any difference of the merged channel config, comments, formatting and order do not count) and `tunnels_added`, `tunnels_changed` and `tunnels_removed` (names); a reload that changes nothing logs the same line with zeros. At debug level a second line lists the ids of the added, changed and removed channels; no line contains URLs.

## 6. Channel list

- `GET /playlist.m3u8`, `Content-Type: audio/x-mpegurl; charset=utf-8`, no BOM; disabled channels omitted; optional `?group=<name>` (exact match; empty lists all).
- Header: `#EXTM3U x-tvg-url="<epg.url>" url-tvg="<epg.url>"` if `epg.url` is set (EPG link mode: the player fetches it).
- Per channel:
  1. `playlist.extra` lines, sorted by directive, then key; keys removed with an empty value (5.4) are not written.
  2. `#EXTINF:-1 tvg-id=".." tvg-name=".." tvg-logo=".." group-title=".."`, then `attrs` sorted by key, then `,<name>`. Empty values are omitted; `"` in values and names is replaced by `'`. Commas in names are fine (IPTV Simple Client takes the name after the last quoted attribute). Kodi lists a channel in its Radio section only with `radio="true"`, which comes from `attrs = { radio = "true" }` (no extra key).
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
| `GET/HEAD /logos/{file}` | Local logo, flat directory, no traversal (5.4) |
| `GET /healthz` | 200 while serving; body: version and source URL (AGPL section 13) |
| `GET /metrics` | Prometheus |

Absolute URLs in responses use `public_base_url`, else `X-Forwarded-Proto` / `X-Forwarded-Host`, else the request `Host`.
Of `X-Forwarded-Proto` / `X-Forwarded-Host` only the first comma-separated value counts; each replaces the scheme or host on its own, and invalid values (scheme other than `http`/`https`, host with characters outside `[A-Za-z0-9.:_[]-]`) are ignored. Without a usable `Host`, the local address of the connection is used.

### 7.2 CLI

```
wg-stream-proxy serve                 # default
wg-stream-proxy check                 # validate config, exit 0/1
wg-stream-proxy import <file.m3u> [--tunnel T] [--match-epg [--config-dir DIR]] [--out FILE]
wg-stream-proxy epg ids               # list id and display-name from epg.url (config flags as for check)
wg-stream-proxy epg match             # suggest tvg_id per channel, print TOML, write nothing
wg-stream-proxy healthcheck           # GET /healthz on the listen address, exit 0/1
wg-stream-proxy version               # print version and source URL (AGPL section 13)
```

`serve` stops on `SIGINT` or `SIGTERM`: it accepts no new connections, gives in-flight requests up to 30s (2.1), then closes the remaining connections and the tunnels; a second signal exits at once.

`check` also parses the tunnel files and the signing key variables (4), lists every error found and starts nothing.

`healthcheck` resolves the config like `serve` (same flags and environment), sends `GET /healthz` to the listen address (an unspecified host such as `:2349` or `0.0.0.0` becomes `127.0.0.1`, `[::]` becomes `[::1]`) with a 3s timeout, follows no redirect and prints nothing on `200` (exit 0). Any other result, a connection error or an invalid config prints one line to stderr and exits 1. It needs no shell or curl, so it serves as the `HEALTHCHECK` of the container image.

`import`:
- Input: UTF-8 (invalid bytes become U+FFFD with a warning), optional BOM, lines end at LF, CRLF or CR; lines with control characters other than tab are dropped with a warning.
- `tvg-id`, `tvg-logo`, `group-title` to their fields, `tvg-name` dropped (generated from `name`), other `#EXTINF` attributes to `playlist.attrs`. Attribute names are case-insensitive for this mapping; values may be quoted or unquoted. The name is the text after the first comma outside quotes and may contain commas.
- `#KODIPROP:inputstream.adaptive.stream_headers=`, `#EXTVLCOPT:http-user-agent=`, `#EXTVLCOPT:http-referrer=`, `url|Header=Value` to `upstream.headers`. Header lists are `Name=Value` pairs separated by `&`, values percent-decoded; a later value replaces an earlier one, the `|` part comes last.
- Other `#<DIRECTIVE>:key=value` lines to `playlist.extra.<DIRECTIVE>` (keys quoted); non-matching lines dropped with a warning. Directive lines after the previous channel's URL belong to the next channel.
- Values shared by all channels of the output (at least two) are written once as `[defaults.*]`, a shared `group` once at the top of the file.
- Commented-out URLs (`#http...`) become `enabled = false`. Other comment lines are dropped with a warning, also between `#EXTINF` and its URL.
- A channel is dropped with a warning if its `#EXTINF` has no name, no URL line follows, or the URL is not an absolute `http(s)` URL; a URL without `#EXTINF` is dropped too. If no channel is left, `import` exits 1.
- `id` is written only where the slug of `name` (5.2) is empty or already used by an earlier channel: the slug (or `channel`) with the first free suffix `-2`, `-3`, ...
- `--tunnel` is written as the file-level `tunnel`; without it, the route follows 5.2.
- `url-tvg` and `x-tvg-url` of `#EXTM3U` are written as a TOML comment for `[epg] url` in `config.toml`; other header attributes are dropped with a warning.
- Output on stdout, or in `--out FILE`, which must not exist yet (never overwritten, exit 1). Warnings go to stderr with file and line.
- `--match-epg`: keep `tvg_id` if it exists in the EPG; else drop it with a warning (if there was one) and add the best suggestion as a TOML comment in the channel's `[channel.playlist]` table (`# tvg_id = "<id>"  # EPG: <name>`; none if nothing matches). Needs `epg.url` in `config.toml` (read like `check`: `--config-dir`, `WG_STREAM_PROXY_CONFIG_DIR`); without `--match-epg`, `import` reads no config and makes no request.

The EPG commands (`epg ids`, `epg match`, `import --match-epg`) download `epg.url` over the `direct` route: the private-range guard (with `[upstream.allow_private] direct`) and the `[upstream]` timeouts apply, and there is no separate config key. The file is plain XMLTV or gzip (recognised by its magic bytes), read as a stream, at most 200 MiB after decompression (else error); only the `<channel>` elements before the first `<programme>` are read (XMLTV DTD order).

`epg ids`: one line per EPG channel, sorted by id: the id, then its display names, separated by tabs.

`epg match`: names normalised (5.2 slug steps 1-2, then only letters and digits kept), EPG display names ranked by similarity.
- Similarity: `1 - d / n`, with `d` the Levenshtein distance of the two normalised names and `n` the length of the longer one. An EPG channel scores by its best display name (its id if it has none); below 0.6 it is no suggestion; an exact normalised match scores 1 and ranks first; ties order by EPG id.
- Output per channel (all channels, in config order): the best suggestion as a `[channel.playlist]` snippet with `tvg_id`, up to two alternatives as comments, no suggestion as a comment line. A channel whose `tvg_id` is in the EPG is only reported. Nothing is written.

### 7.3 Metrics (prefix `wsp_`)

- `tunnel_up{tunnel}`: 1 while the device runs and the last handshake is younger than 180s.
- `tunnel_last_handshake_timestamp_seconds{tunnel}`: `0` while the tunnel is not running or has no handshake yet.
- `active_requests{channel,kind}`: in-flight requests incl. open raw streams; `kind` = `playlist`, `segment`, `raw`.
- `upstream_requests_total{channel,kind,code}`: `code` = upstream status, or `error`.
- `upstream_errors_total{channel,reason}`: `reason` = `connect`, `timeout`, `guard`, `tls`, `redirects`, `size`, `other`.

Only `/c/` and `/r/` requests of enabled channels count; `channel` is the channel id, never a URL.

- `kind`: `playlist` if the response is detected as a playlist (3.1), else `raw` for `/c/` and `segment` for `/r/`. Until the type is detected, and if it never is (error, non-`2xx` status), an `hls` channel and, without a configured type, an upstream path ending in `.m3u8` or `.m3u` count as `playlist`.
- `upstream_requests_total` counts every upstream request, the refetch of a partial playlist (3.4) included.
- `upstream_errors_total` counts failures before the response (with `code="error"`) and after it: playlist body or sniff read errors, a rejected playlist or a second `206` (`other`), a stream ended by an upstream error. Upstream statuses count only in `upstream_requests_total`.
- A request the client gave up counts neither as error nor, if no response came, as upstream request.

`/metrics` also serves the standard `go_*` and `process_*` metrics of the Prometheus Go client.

## 8. Operations and security

- Intended for LAN use; exposure to the internet needs an authenticating reverse proxy.
- WireGuard keys only in tunnel files and never logged; upstream URLs logged without query strings (and without fragment and user info).
- Debug log (`log_level = "debug"`): one `request` line per `/c/` and `/r/` request when it ends, with request id, channel, route, kind, client status and bytes, client disconnect, upstream status and error reason, and where the time went: `tunnel_start` (only if the request waited for a tunnel start), `dns`, `connect` (Happy-Eyeballs fallback included), `tls`, `ttfb` (first upstream byte), `total`; drain waits and retired tunnels; wireguard-go's own events (handshakes, keepalives, retries) with the peer shown by tunnel name. Tunnel start (with handshake), start failure and close lines carry their durations at every level. The log never contains keys, key fragments or query strings. Above debug the debug lines cost only a level check.
- Container: non-root, read-only root filesystem, all capabilities dropped, single replica. Details: [deploy.md](deploy.md).

## 9. Engineering rules

- Layout: `cmd/wg-stream-proxy/`, `internal/{config,tunnel,route,hls,playlist,server,importer,epg,metrics}`, `examples/config/`, `testdata/`, `deploy/`, `docs/`.
- Dependencies: stdlib `net/http`, `log/slog`, `wireguard-go`, gVisor netstack, a TOML parser, Prometheus client. No web framework.
- gVisor stays at the version in `wireguard-go`'s `go.mod` and is only updated together with it (excluded from automatic bumps).
- `internal/tunnel/netstack` is a trimmed copy of `wireguard-go`'s `tun/netstack` (MIT, licence kept) with one change: a packet gVisor sends after `Close` is dropped instead of sent on a closed channel, which would crash the process (#47). The copy names the upstream commit it was taken from and is updated together with `wireguard-go`: on every `wireguard-go` update, compare upstream `tun/netstack` with the copy and port relevant changes.
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

- WireGuard encryption is CPU-bound on small ARM boards; 3 HD streams on a Raspberry Pi 5 are expected to fit, measured by the L6 load test; a Raspberry Pi 4 is not tested (no device available).
- Geo-IP databases may disagree on a tunnel's exit country; a broadcaster can see a different country than expected and block the stream.
- Some upstreams need cookies or tokens from a website session; only static headers are supported.

Prior art: wireproxy (userspace WireGuard proxy, one tunnel per process), MediaFlow Proxy (HLS proxy with per-domain proxy routing), Dispatcharr (IPTV manager with one VPN via Gluetun). None selects a tunnel per channel inside one process.
