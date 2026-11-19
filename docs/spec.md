# wg-stream-proxy - Specification

> Route each IPTV and internet radio channel through its own userspace WireGuard tunnel (or direct) and serve them as one M3U playlist for Kodi, VLC and other players. Proxies HLS (TS, fMP4, AES-128) and raw/ICY audio streams; no root, no TUN device, fails closed.

- Repository: `github.com/KingOfKalk/wg-stream-proxy`, Go, AGPL-3.0-or-later
- Status: binding for the latest [release](https://github.com/KingOfKalk/wg-stream-proxy/releases); the roadmap (section 10) lists what is released and what is planned
- Related: [architecture.md](architecture.md), [deploy.md](deploy.md), [testing.md](testing.md), [observability.md](observability.md)

## 1. Scope

### 1.1 Goals

- Route each channel through a chosen WireGuard tunnel, or explicitly `direct`. Nothing else on the host is affected.
- Multiple tunnels in one process, fully in userspace (no TUN device, no root, no kernel module).
- One extended M3U channel list (`/playlist.m3u8`) for Kodi (IPTV Simple Client), VLC and other players.
- HLS passthrough with playlist rewriting; raw passthrough for radio and MPEG-TS.
- Fail closed: a channel never silently bypasses its tunnel.
- Migration from an existing M3U file via `import`.
- Capacity: 3 concurrent viewers (household use).

### 1.2 Non-goals

- Decryption, DRM licenses or keys inside the proxy (permanent). AES-128 HLS keys are only passed through.
- Transcoding, restreaming, Widevine passthrough, Kodi add-on, web UI, Xtream Codes, multi-user.
- DASH: not supported (DASH without DRM planned for v0.4, spec 10).

### 1.3 Platforms

- Build targets (cross-compiled in CI): `linux/amd64`, `linux/arm64`, `darwin/arm64`; `CGO_ENABLED=0`.
- Distribution: the container image (`linux/amd64` + `linux/arm64`). Binaries: build from source or `go install`; release binaries are planned (Later, spec 10).
- LibreELEC 12+ uses an aarch64 userland on Raspberry Pi 4/5 (since 12.0, May 2024), so the `linux/arm64` container (Docker add-on) runs on default installs. Only 32-bit images (Raspberry Pi 2/3, Allwinner H3/R40, Rockchip RK3288) would need `linux/arm`, which is deferred.

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
  - Start failure or no handshake within `[upstream] connect_timeout`: request fails with `502` (or an error clip, 3.4); the next request retries. Start, tunnel DNS and connect share one `connect_timeout` deadline.
  - On shutdown, reload removal or cool-down (7.1): drain in-flight requests for max 30s, then close.
- `direct` uses a plain `http.Transport` on the host network with the same settings.

## 3. Stream handling

### 3.1 Request flow for `/c/{id}`

1. Fetch the channel URL through the channel's route.
2. Detect the type:
   1. `upstream.type = "hls"` or `"raw"` set: use it.
   2. `Content-Type` is `application/vnd.apple.mpegurl` or `application/x-mpegurl` (case-insensitive, parameters ignored): HLS.
   3. Body starts with `#EXTM3U` (after optional UTF-8 BOM and whitespace, within the first 512 bytes): HLS.
   4. Otherwise: raw.
3. HLS: rewrite (3.2) and return. Raw: pipe (3.3), unless it is a playlist the proxy does not rewrite (refused, 3.3).

Resources under `/r/` use the same detection (steps 2.2-2.4): playlists are rewritten, playlists the proxy does not rewrite are refused (3.3), everything else is streamed with `io.Copy`.

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
- Fail closed: a playlist the proxy does not rewrite is refused with `502` instead of piped, on `raw` and `auto` channels and `/r/` resources, because the player would fetch the URLs in it directly, past the tunnel. Checked on `GET` responses with a `2xx` status (`HEAD` and other statuses send no playlist); `hls` channels are always rewritten (3.2). Detected, after the HLS detection of 3.1:
  - by `Content-Type` (case-insensitive, parameters ignored; not on `raw` channels, whose configured type overrides it): PLS `audio/x-scpls`, `audio/scpls`, `application/pls+xml`; M3U `audio/x-mpegurl`, `audio/mpegurl`; DASH `application/dash+xml`; XSPF `application/xspf+xml`; ASX `video/x-ms-asx`, `audio/x-ms-asx`, `video/x-ms-wvx`, `audio/x-ms-wax`;
  - by the start of the body (first 512 bytes, after optional UTF-8 BOM and whitespace): `[playlist]` (PLS) or `[Reference]` (ASX reference file), case-insensitive; `#EXTM3U` (reaches this check only on `raw` channels); a first line that is neither blank nor a `#` comment and is an absolute URL (`scheme://`, scheme of two or more characters) or a scheme-relative one (`//`) without control characters (plain M3U); an XML root element `MPD`, `playlist` or `ASX` (case-insensitive, namespace prefix ignored, after XML declaration, comments and doctype).
  - Media (MPEG-TS, fMP4, MP3, AAC, Ogg, ICY streams) matches none of these. A `warn` log line names channel, route, URL (8) and format (`pls`, `m3u`, `dash`, `xspf`, `asx`); metrics reason `unsupported` (7.3).
  - Rewriting PLS and plain M3U (internet radio) is a possible later feature; DASH is planned for v0.4 (10).
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
- Limits: max 10 redirects (`301`, `302`, `303`, `307`, `308` with `Location`; each hop goes through the guard), playlist body max `[upstream] max_playlist_size` (default 8 MiB, at most 32 MiB; a larger body gives `502`, metrics reason `size`), upstream URL max 4096 bytes (redirect targets included).
- Timeouts (`[upstream]`): `connect_timeout` (10s), `header_timeout` until response headers (15s), `read_timeout` between body reads (30s, implemented as a resettable timer). No total timeout.
  - `connect_timeout` covers DNS lookup and TCP connect of one connection; running out counts as a connection error (`502`), like a tunnel without handshake (2.1).
  - `header_timeout` runs from the start of the request until the final response headers, across redirects; `504`.
  - `read_timeout` runs only while waiting for upstream data, not while the client is slow to accept it; `504` if it fires before the response starts, else the stream ends.
- Error clips (`[clips] error`, default on, 5.3; design: [clips.md 6.2](clips.md#62-error-clips)): a `GET` of `/c/` of an HLS channel (listed with `.m3u8`, 6) answers a failure with a short video instead of an HTTP error, so the player shows the error as text and stops instead of a failure dialog.
  - Failures: every `502` and `504` of this section, of the tunnel start (2.1), of the playlist checks (3.2, 3.3) and of the guard (3.5), and an upstream `4xx`.
  - Answer: `200`, `Content-Type: application/vnd.apple.mpegurl`, `Cache-Control: no-store`, a VOD media playlist (`#EXT-X-VERSION:3`, `#EXT-X-TARGETDURATION:1`, `#EXT-X-MEDIA-SEQUENCE:0`, `#EXT-X-PLAYLIST-TYPE:VOD`, `#EXT-X-ENDLIST`) that lists the clip's segment 5 times (`#EXTINF:0.960,`, 4.8 s) as `/clip/{name}/{n}.ts` with `n` = 0-4 (7.1), absolute like resource URLs.
  - Clip per failure: `tunnel-unreachable` ("Tunnel not reachable") when the tunnel did not start (start failure, no handshake within `connect_timeout`, a start cancelled by a reload); `channel-blocked` ("Channel blocked by configuration") for a guard refusal (3.5) or an upstream address outside the tunnel's `AllowedIPs` (5.5, also when a name resolves only to such addresses); `channel-not-available` ("Channel not available") for everything else: DNS, connect, TLS, redirects, size, timeouts, upstream statuses, rejected and refused playlists.
  - Unchanged: `HEAD`, `/r/`, channels listed without `.m3u8` and `error = false` keep the statuses above; an unknown or disabled channel stays `404`.
  - Logs and metrics count the failure as they do without a clip; its `warn` line gets `clip=<name>`. An upstream `4xx` answered with a clip gets the `upstream error status` line of a `5xx`. Served clips count in `error_clips_total` (7.3).

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
max_playlist_size = "8MiB"            # B, KiB or MiB; at most 32MiB

[upstream.allow_private]              # optional, per route name
direct = []

[tunnels]
idle_timeout = "60m"

[reload]
watch_interval = "2s"                 # how often the config files are checked for changes (5.6); "0" turns it off

[clips]
error = true                          # error clips instead of 502/504 on GET /c/ of HLS channels (3.4)
inflight = false                      # in-flight clips while a channel starts: not built yet, only validated
grace = "1s"                          # wait before an in-flight clip starts: not used yet

[epg]
url = "https://epg.example.org/epg/epg.xml.gz"  # optional, no default

[debug]
listen = ""                           # off; e.g. "127.0.0.1:2350" (7.1)
```

Unknown keys and wrong value types are config errors. Values are validated: `listen` is `[host]:port` with a numeric port; `debug.listen` is empty or of the same form, with a port other than the one of `listen` (as in effect after flag and environment); durations are positive (`reload.watch_interval` also accepts `"0"`, which turns watching off); `max_playlist_size` is a whole number directly followed by `B`, `KiB` or `MiB`, positive and at most `32MiB`; `public_base_url` and `epg.url` are empty or absolute `http(s)` URLs; `user_agent` has no line breaks; `allow_private` keys are `direct` or existing tunnel names, values are CIDR prefixes without host bits.

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
url = "https://live.example.com/mpegts/100100_200200/master_mpegts.m3u8"  # required
id = "ert-world"                      # optional, default: slug of name
tunnel = "gr"                         # optional
group = "Greek TV"                    # optional
enabled = true

[channel.upstream]
type = "auto"                         # auto | hls | raw
headers = { Referer = "https://www.example.org/" }

[channel.playlist]
tvg_id = "ertworld"
logo = "EPTWORLD"
attrs = { tvg-chno = "5" }

[channel.playlist.extra.KODIPROP]
"inputstream.adaptive.manifest_type" = "hls"   # dotted keys must be quoted

[[channel]]
name = "Das Erste"
tunnel = "direct"
url = "https://live.example.net/daserste/live/hls/de/master.m3u8"
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
- File watching: unless `reload.watch_interval` (5.3) is `"0"`, the proxy checks the files a reload reads (`config.toml`, `channels.d/*.toml` with `_defaults.toml`, `<tunnels-dir>/*.conf`) every interval by path, size and modification time; other files do not count. Added, changed and removed files all count. A change triggers the same reload as `SIGHUP` once the files have stayed the same for one further interval, so an editor that writes in several steps causes one reload. Nothing is reloaded while the files are unchanged; a rejected config is logged once and not tried again until the files change. `SIGHUP` works either way. Polling works on every file system (bind mounts, network shares); a change made without touching size and modification time is not seen.
- Requests in flight finish with the config they started with; new requests use the new one. This includes `[clips]`: a changed `error` applies to the next `/c/` request.
- Unchanged tunnels (same parsed content; comments and formatting do not count) keep running; changed or removed tunnels are drained (2.1) and recreated lazily.
- A route (`direct` or a tunnel) keeps its pooled upstream connections only if its network, its `allow_private` entries and the whole `[upstream]` block are unchanged; otherwise new requests use a new route and the old route's idle connections are closed, while an unchanged tunnel keeps running.
- A reload that changes or removes a tunnel which is still starting cancels the start; requests waiting for it get `502`; the new tunnel starts lazily on the next request.
- Restart required for: `listen`, `debug.listen`, `log_format`, `reload.watch_interval`, signing key, tunnels dir location. A reload that changes one of them logs a warning and keeps the old value.
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
#EXTM3U x-tvg-url="https://epg.example.org/epg/epg.xml.gz" url-tvg="https://epg.example.org/epg/epg.xml.gz"
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
| `GET/HEAD /clip/{name}/{n}.ts` | Built-in clip segment (3.4): `{name}` a clip of `internal/clips` (`tunnel-unreachable`, `channel-blocked`, `channel-not-available`, and the in-flight texts), `{n}` the position, 0-63 in decimal without leading zeros. PTS, DTS and PCR are shifted by 10 s + `n` × 0.96 s and the continuity counters continued, so positions 0, 1, … play as one stream. `video/mp2t`, unsigned; no upstream request. Anything else under `/clip/`: `404` |
| `GET /healthz` | 200 while serving; body: version and source URL (AGPL section 13) |
| `GET /metrics` | Prometheus |

Absolute URLs in responses use `public_base_url`, else `X-Forwarded-Proto` / `X-Forwarded-Host`, else the request `Host`.
Of `X-Forwarded-Proto` / `X-Forwarded-Host` only the first comma-separated value counts; each replaces the scheme or host on its own, and invalid values (scheme other than `http`/`https`, host with characters outside `[A-Za-z0-9.:_[]-]`) are ignored. Without a usable `Host`, the local address of the connection is used.

#### Debug endpoints

Served only on their own listener `debug.listen` (5.3), off by default and never on `listen`; no authentication (8). Unsafe methods from a browser page of another origin (`Sec-Fetch-Site`, `Origin`) are rejected with `403`. So are requests whose `Host` is not an IP address or `localhost` (`403`, against DNS rebinding): address the listener by IP, not by host name.

| Path | Description |
|---|---|
| `/debug/pprof/…` | Go profiler (`net/http/pprof`: index, `profile`, `heap`, `goroutine`, `trace`, …) |
| `GET /debug/requests` | JSON `{"requests": [...]}`: the last 256 finished `/c/`, `/r/` and `/clip/` requests, newest first |
| `GET /debug/tunnels` | JSON `{"tunnels": [...]}`: per configured tunnel, sorted by name |
| `POST /debug/tunnels/{name}/stop` | Cool-down of a tunnel |

- `/debug/requests`: the fields of the debug line (8): `id`, `time` (start of the request, RFC 3339), `method`, `channel`, `route`, `kind`, `status`, `bytes`, `client_gone`, `upstream_status` and `reason` (omitted without), `tunnel_start_ms` (only if the request waited for a tunnel start), `dns_ms`, `connect_ms`, `connect_attempts`, `tls_ms`, `ttfb_ms`, `total_ms`, `conn_reused`, `redirects`, `url`. Durations in milliseconds, to the microsecond; `url` as in the log (8: scheme, host, file name). A `/clip/` request has `kind` `clip`, empty `channel` and `route`, and its path as `url`. A request is recorded when it ends (a stream: when it closes), at every log level, while the debug listener is on; the buffer has a fixed size and never grows.
- `/debug/tunnels`: `name`, `state` (`stopped`, `starting`, `running`, `draining`), `started_at` (first handshake of the running device) and `last_handshake` (RFC 3339, omitted while no device runs), `active_requests`.
- Cool-down: the tunnel is drained like a reload removal (2.1: open streams finish, or the 30s drain timeout cuts them; requests arriving meanwhile use the running device), then its device stops, the route's pooled connections close and its DNS cache is emptied; `tunnel closed reason=cooldown` is logged and the next request starts it cold. The answer comes when the tunnel is stopped: `200` with `{"name": "<name>", "was_running": true}`; for a tunnel that was not running nothing changes and `was_running` is `false`. Unknown tunnel (also `direct`): `404`; other methods: `405`.

### 7.2 CLI

```
wg-stream-proxy serve                 # default
wg-stream-proxy check                 # validate config, exit 0/1
wg-stream-proxy import <file.m3u> [--tunnel T] [--match-epg [--config-dir DIR]] [--out FILE]
wg-stream-proxy epg ids               # list id and display-name from epg.url (config flags as for check)
wg-stream-proxy epg match             # suggest tvg_id per channel, print TOML, write nothing
wg-stream-proxy healthcheck           # GET /healthz on the listen address, exit 0/1
wg-stream-proxy tunnel list           # tunnels and their state, from the debug endpoints (7.1)
wg-stream-proxy tunnel stop <name>    # cool-down of a tunnel (7.1)
wg-stream-proxy version               # print version and source URL (AGPL section 13)
wg-stream-proxy help [command]        # description and command list, or the flags of a command
```

`help`: `wg-stream-proxy help`, `-h` and `--help` print a one-line description, the usage line and the list of commands with one line each, to stdout, exit 0. `help <command>` (`help epg ids`) and `<command> -h` (also after other flags, `serve --listen :1 -h`) print the usage, description and flags of that command to stdout, exit 0; for `epg` and `tunnel` they list the subcommands. Only a first argument `-h` or `--help` is the help of the program, so `serve -h` stays the help of `serve`. An unknown command, also for `help <unknown>`, prints one error line and the command list to stderr and exits 2. A flag error prints the flag package's message and a pointer to `help <command>` to stderr and exits 2. Without any argument the program runs `serve`; it prints no help.

`serve` stops on `SIGINT` or `SIGTERM`: it accepts no new connections, gives in-flight requests up to 30s (2.1), then closes the remaining connections and the tunnels; a second signal exits at once.

`check` also parses the tunnel files and the signing key variables (4), lists every error found and starts nothing.

`healthcheck` resolves the config like `serve` (same flags and environment), sends `GET /healthz` to the listen address (an unspecified host such as `:2349` or `0.0.0.0` becomes `127.0.0.1`, `[::]` becomes `[::1]`) with a 3s timeout, follows no redirect and prints nothing on `200` (exit 0). Any other result, a connection error or an invalid config prints one line to stderr and exits 1. It needs no shell or curl, so it serves as the `HEALTHCHECK` of the container image.

`tunnel list` and `tunnel stop <name>` resolve the config like `healthcheck` and call the debug endpoints (7.1) on `debug.listen` (unspecified host as for `healthcheck`), so they work through `docker exec` in the image. `list` (`GET /debug/tunnels`, 3s timeout) prints a table: name, state, active requests, start and last handshake time. `stop` (`POST /debug/tunnels/{name}/stop`, 45s timeout, covering the drain) waits until the tunnel is stopped and prints `tunnel <name> stopped`, or `tunnel <name> was not running`. The name may stand before or after the flags. Debug listener off, unknown tunnel, a connection error or another status: one line on stderr, exit 1; usage errors (missing or invalid name): exit 2.

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
- `upstream_errors_total{channel,reason}`: `reason` = `connect`, `timeout`, `guard`, `tls`, `redirects`, `size`, `unsupported`, `other`.
- `error_clips_total{clip}`: error clips served instead of an HTTP error (3.4), by clip name (`tunnel-unreachable`, `channel-blocked`, `channel-not-available`; each starts at 0). The failure behind it counts as without a clip.
- `tunnel_start_seconds{tunnel}` (histogram): one observation per successful tunnel start, the time from the start request until the first handshake completed (device up included). Observed where the tunnel starts, once per start however many requests waited for it; a failed start is not observed (the start failure line, spec 8, logs it).
- `upstream_phase_seconds{route,phase}` (histogram): where the time of the upstream requests of `/c/` and `/r/` goes, per route (`direct` or a tunnel name), `phase` = `dns` (host lookups), `connect` (connection setup, Happy-Eyeballs fallback included), `tls` (TLS handshakes), `first_byte` (start of the upstream request until the first response byte); the same definitions as the debug line (`dns`, `connect`, `tls`, `ttfb`, spec 8), without a tunnel start wait (that is `tunnel_start_seconds`).
- Buckets of both histograms, in seconds: `0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 15` (classic histograms).

Only `/c/` and `/r/` requests of enabled channels count; `channel` is the channel id, never a URL. `/clip/` requests count in no metric.

- `kind`: `playlist` if the response is detected as a playlist (3.1) or refused as one (3.3), else `raw` for `/c/` and `segment` for `/r/`. Until the type is detected, and if it never is (error, non-`2xx` status), an `hls` channel and, without a configured type, an upstream path ending in `.m3u8` or `.m3u` count as `playlist`.
- `upstream_requests_total` counts every upstream request, the refetch of a partial playlist (3.4) included.
- `upstream_errors_total` counts failures before the response (with `code="error"`) and after it: playlist body or sniff read errors, a rejected playlist or a second `206` (`other`), a refused playlist format (`unsupported`, 3.3), a stream ended by an upstream error. Upstream statuses count only in `upstream_requests_total`.
- A request the client gave up counts neither as error nor, if no response came, as upstream request.
- Timing histograms: labels are the route or tunnel name and the phase only, never a channel or URL. They are collected for every `/c/` and `/r/` request at every log level and recorded when the request ends (a stream: when it closes). A request observes only the phases that happened: no `dns` for an IP literal or a pooled connection, no `connect` for a pooled connection, no `tls` for plain HTTP, no `first_byte` without a response; phases that failed (a refused connect, a failed handshake) count with their duration. `first_byte` is observed once per request, for the last upstream response (a refetch, 3.4, does not count twice). Requests the client gave up observe what happened before.

`/metrics` also serves the standard `go_*` and `process_*` metrics of the Prometheus Go client.

## 8. Operations and security

- Intended for LAN use; exposure to the internet needs an authenticating reverse proxy.
- WireGuard keys only in tunnel files and never logged; upstream URLs (in log lines and in error messages) are logged as scheme, host and file name only, e.g. `https://cdn.example.com/…/7.m3u8`: no user info, query string, fragment or other path segment, because CDNs put session ids and tokens there. The file name is reduced like `{name}` (spec 4); a URL without path shows scheme and host only.
- Debug log (`log_level = "debug"`): one `request` line per `/c/`, `/r/` and `/clip/` request when it ends, with request id, channel, route, kind, client status and bytes, client disconnect, upstream status and error reason, and where the time went: `tunnel_start` (only if the request waited for a tunnel start), `dns`, `connect` (Happy-Eyeballs fallback included), `tls`, `ttfb` (first upstream byte), `total`; drain waits and retired tunnels; wireguard-go's own events (handshakes, keepalives, retries) with the peer shown by tunnel name. Tunnel start (with handshake), start failure and close lines carry their durations at every level. The log never contains keys, key fragments, query strings or upstream paths beyond the file name. Above debug the debug lines cost only a level check; the timing behind them is collected at every level for the timing metrics (7.3).
- Debug endpoints (7.1): off by default, on their own listener, without authentication. They answer only requests addressed by IP or `localhost`, so a web page cannot reach them by rebinding its own name to the debug address. Bind them to loopback (e.g. `127.0.0.1:2350`, used through `docker exec … tunnel`) or at most the LAN, never beyond: the profiler exposes process internals (memory, goroutine stacks, command line), and whoever reaches the port can cool tunnels down. `/debug/requests` holds no keys, tokens, query strings or upstream paths beyond the file name (the same reduction as the log).
- Container: non-root, read-only root filesystem, all capabilities dropped, single replica. Details: [deploy.md](deploy.md).

## 9. Engineering rules

- Layout: `cmd/wg-stream-proxy/`, `internal/{config,tunnel,route,hls,playlist,server,importer,epg,metrics,clips}`, `examples/config/`, `testdata/`, `deploy/`, `docs/`.
- Dependencies: stdlib `net/http`, `log/slog`, `wireguard-go`, gVisor netstack, a TOML parser, Prometheus client. No web framework.
- `internal/clips` embeds the built-in clip segments (design: [clips.md](clips.md)): six 0.96 s MPEG-TS files in `internal/clips/segments/`, rendered by `internal/clips/generate.sh` (pinned image, Inter 4.1 by version and checksum) and committed. Change a text by editing the script, rerunning it and committing the result. Only rendered pixels ship, not the font (SIL OFL 1.1). `clips.Repeat` shifts the timestamps and continues the continuity counters of a segment for its position in a stream of copies (7.1).
- gVisor stays at the version in `wireguard-go`'s `go.mod` and is only updated together with it (excluded from automatic bumps).
- `internal/tunnel/netstack` is a trimmed copy of `wireguard-go`'s `tun/netstack` (MIT, licence kept) with one change: a packet gVisor sends after `Close` is dropped instead of sent on a closed channel, which would crash the process (#47). The copy names the upstream commit it was taken from and is updated together with `wireguard-go`: on every `wireguard-go` update, compare upstream `tun/netstack` with the copy and port relevant changes.
- Injected for tests: `Dialer` per route, private-range guard allowlist, clock.
- Client disconnect cancels the upstream request. HTTP server: `ReadHeaderTimeout` set, no `WriteTimeout`.
- `atomic.Int64` and similar types only, never `atomic.AddInt64` on plain fields.
- Code comments in English; file header `// SPDX-License-Identifier: AGPL-3.0-or-later`.

## 10. Roadmap

- **v0.1 Core** (released as v0.1.0): the core of this spec (config, tunnels, routes and guard, HLS and raw/ICY streams, channel list, `SIGHUP` reload, metrics, `import`, `epg`), plus Dockerfile, Compose file and the v0.1 test layers in [testing.md](testing.md). Publishes the container image `ghcr.io/kingofkalk/wg-stream-proxy` (`linux/amd64` + `linux/arm64`, public package): tag `edge` on every push to `main`, version tags on release.
- **v0.2 Observability** (released as v0.2.0): timing histograms, debug endpoints with profiles, recent requests and cool-down.
- **v0.3 Experience:** placeholder and error clips with seamless switchover (design: [clips.md](clips.md)), per-request prefetch, file-watch reload.
- **v0.4 Content:** DASH without DRM, EPG proxy mode, logo proxying, ad-hoc proxy endpoint.
- **Later:** distribution (GoReleaser, systemd unit, launchd plist, Kustomize), 32-bit ARM, scaling beyond 3 viewers, per-tunnel overrides.

## 11. Risks

- WireGuard encryption is CPU-bound on small ARM boards; 3 HD streams on a Raspberry Pi 5 are expected to fit, measured by the L6 load test; a Raspberry Pi 4 is not tested (no device available).
- Geo-IP databases may disagree on a tunnel's exit country; a broadcaster can see a different country than expected and block the stream.
- Some upstreams need cookies or tokens from a website session; only static headers are supported.

Prior art: wireproxy (userspace WireGuard proxy, one tunnel per process), MediaFlow Proxy (HLS proxy with per-domain proxy routing), Dispatcharr (IPTV manager with one VPN via Gluetun). None selects a tunnel per channel inside one process.
