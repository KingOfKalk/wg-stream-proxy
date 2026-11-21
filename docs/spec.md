# wg-stream-proxy - Specification

> Route each IPTV and internet radio channel through its own userspace WireGuard tunnel (or direct) and serve them as one M3U playlist for Kodi, VLC and other players. Proxies HLS (TS, fMP4, AES-128), MPEG-DASH without DRM and raw/ICY audio streams; no root, no TUN device, fails closed.

- Repository: `github.com/KingOfKalk/wg-stream-proxy`, Go, AGPL-3.0-or-later
- Status: binding for the latest [release](https://github.com/KingOfKalk/wg-stream-proxy/releases); the roadmap (section 10) lists what is released and what is planned
- Related: [architecture.md](architecture.md), [deploy.md](deploy.md), [testing.md](testing.md), [observability.md](observability.md)

## 1. Scope

### 1.1 Goals

- Route each channel through a chosen WireGuard tunnel, or explicitly `direct`. Nothing else on the host is affected.
- Multiple tunnels in one process, fully in userspace (no TUN device, no root, no kernel module).
- One extended M3U channel list (`/playlist.m3u8`) for Kodi (IPTV Simple Client), VLC and other players.
- HLS and MPEG-DASH (without DRM) passthrough with playlist and manifest rewriting; raw passthrough for radio and MPEG-TS.
- Fail closed: a channel never silently bypasses its tunnel.
- Capacity: 3 concurrent viewers (household use).

### 1.2 Non-goals

- Decryption, DRM licenses or keys inside the proxy (permanent). AES-128 HLS keys are only passed through.
- Transcoding, restreaming, Widevine passthrough, Kodi add-on, web UI, Xtream Codes, multi-user.
- DASH with DRM: manifests with `ContentProtection` are refused (3.6).

### 1.3 Platforms

- Build targets (cross-compiled in CI): `linux/amd64`, `linux/arm64`, `darwin/arm64`; `CGO_ENABLED=0`.
- Distribution: the container image (`linux/amd64` + `linux/arm64`). Binaries: build from source or `go install`; there are no release binaries.
- LibreELEC 12+ uses an aarch64 userland on Raspberry Pi 4/5 (since 12.0, May 2024), so the `linux/arm64` container (Docker add-on) runs on default installs. Only 32-bit images (Raspberry Pi 2/3, Allwinner H3/R40, Rockchip RK3288) would need `linux/arm`, which is not built.

## 2. Architecture

One process: the HTTP interface for the players (7.1), one route per tunnel plus `direct`, each with its `http.Transport` and the private-range guard (3.5), and one wireguard-go device with a gVisor netstack per tunnel file. Diagrams and request flows: [architecture.md](architecture.md).

### 2.1 Tunnels

- One WireGuard device per tunnel file, shared by all channels and viewers using it (one key, one UDP session).
- Each tunnel has its own `http.Transport` dialing through the netstack: keep-alive, `ForceAttemptHTTP2 = true`, `MaxIdleConnsPerHost = 16`, no environment proxy, TLS handshake timeout `connect_timeout` (3.4). HTTP/2 connections are health-checked: after 15s without a frame from the upstream a ping is sent, and the connection is closed if no answer comes within 10s, so a silently dead connection fails one request instead of taking every request to that host.
- IPv4 and IPv6 addresses of a host are tried Happy-Eyeballs style (RFC 8305, simplified): families interleaved starting with the first DNS answer; the next address starts after 300ms or as soon as the previous attempt fails; the first connection wins. A tunnel with `::/0` but no working IPv6 upstream thus falls back to IPv4 after 300ms.
- DNS for upstream hosts is resolved inside the tunnel, using the `DNS` servers of the tunnel file. A and AAAA (only for address families the tunnel has) are queried in parallel. The first attempt of a query goes to every `DNS` server at once; the first answer that is not an error wins. An unanswered query is repeated after 0.5s, 1s, 2s, then every 4s, each time to the next `DNS` server, until `connect_timeout`. Until a `DNS` server has answered (any answer, also an error) since the tunnel started, an unanswered query is instead repeated every 0.25s to every `DNS` server, for at most 8s, then every 4s as above: the `DNS` servers of some VPN providers do not answer for several seconds after the handshake of a key that was idle for a while, and the first lookup should get its answer soon after they do. An error answer (e.g. `SERVFAIL`) to a repeat asks the next server at once; a server that answered with an error is not asked again, and the query fails once every server did. A truncated answer is repeated over TCP, NXDOMAIN ends the lookup at once.
- Tunnel DNS answers are cached per tunnel, host name and record type: for the smallest TTL of the records used (CNAME chain included), at least 10s and at most 5min; an answer without records of the type for the negative TTL of its SOA record (RFC 2308, same bounds, 10s without SOA); NXDOMAIN for 10s. Error answers and unanswered queries are not cached. At most 256 entries per tunnel (when full, expired entries go first, then the one expiring soonest). The cache is emptied whenever the tunnel stops, so a restarted tunnel resolves again. Concurrent lookups of the same host share one query; a caller whose deadline ends stops waiting without cancelling it for the others.
- Tunnel DNS results list IPv4 first.
- The `Endpoint` hostname is resolved by the host resolver when the tunnel starts (IPv4 preferred).
- Lifecycle:
  - Lazy start on the first request; concurrent first requests share one start.
  - Closed after `[tunnels] idle_timeout` (default `60m`) without activity. Activity = any in-flight request or open stream; the idle time runs from the end of the last one. Pooled idle upstream connections are not activity and close with the tunnel; the next request starts it again.
  - Start failure or no handshake within `[upstream] connect_timeout`: request fails with `502` (or an error clip, 3.4); the next request retries. Start, tunnel DNS and connect share one `connect_timeout` deadline.
  - On shutdown (7.2) the requests in flight get up to 30s, then the tunnels close. On reload removal or cool-down (7.1): drain in-flight requests for max 30s, then close.
- `direct` uses a plain `http.Transport` on the host network with the same settings.

## 3. Stream handling

### 3.1 Request flow for `/c/{id}`

1. Fetch the channel URL through the channel's route.
2. Detect the type:
   1. `upstream.type = "hls"`, `"dash"` or `"raw"` set: use it.
   2. `Content-Type` (case-insensitive, parameters ignored) is `application/vnd.apple.mpegurl` or `application/x-mpegurl`: HLS; `application/dash+xml`: DASH.
   3. Body starts with `#EXTM3U` (after optional UTF-8 BOM and whitespace, within the first 512 bytes): HLS.
   4. Body is XML with the root element `MPD` (case-insensitive, namespace prefix ignored, after optional UTF-8 BOM, XML declaration, comments and a doctype, as an XML parser reads them): DASH. The part before the root element (prolog) may hold only whitespace, the XML declaration, comments and a doctype without internal subset, none of them with `<` or `>` inside and no comment starting with `-`: players' lighter parsers end these at other places and could find another root element. Otherwise no root is found. The first 512 bytes are read; if the body starts with `<` or `WEBVTT` (after optional BOMs and whitespace) and they are text (3.3), up to 64 KiB.
   5. Otherwise: raw.
3. HLS: rewrite (3.2) and return. DASH: rewrite (3.6) and return. Raw: pipe (3.3), unless it is a playlist the proxy does not rewrite (refused, 3.3).

Resources under `/r/` and `/p/` (4, 4.1) use the same detection (steps 2.2-2.5): playlists and manifests are rewritten, playlists the proxy does not rewrite are refused (3.3), everything else is streamed with `io.Copy`.

### 3.2 HLS rewriting

- Stateless: master and media playlists are rewritten and returned; the player selects variants and renditions. Live and VOD are handled the same way.
- Rewritten:
  - Every URI line (non-empty, not starting with `#`).
  - Every `URI="..."` attribute in any tag, including names ending in `-URI` (`SERVER-URI`, `X-ASSET-URI`, ...), and `X-ASSET-LIST="..."` (the JSON asset list of an interstitial, refused under `/r/`, 3.3).
- Only `http`, `https` and relative URIs are rewritten (relative ones resolved against the final URL after redirects). All other schemes (`data:`, `skd:`, ...) and all other lines stay byte for byte unchanged.
- AES-128 and SAMPLE-AES key URIs are rewritten like any URI; the player decrypts.
- Lines end at LF, CRLF or a lone CR (as ffmpeg reads them). Tags are lines starting with `#EXT`; other lines starting with `#` are comments. `#EXTINF` has no attribute list and is never rewritten. Other tags: attributes may be separated by commas and/or whitespace; the list is read up to the first element that is not `NAME=VALUE`. Blank lines, line endings, a leading BOM and whitespace around URI lines are kept.
- Fail closed: the whole playlist is rejected (`502`) instead of leaving a URL the player could fetch directly, if
  - an `http`, `https` or relative URI does not parse, or resolves to an upstream URL longer than 4096 bytes;
  - it contains a NUL byte (a line end for ffmpeg);
  - a tag other than `#EXTINF` contains `URI=` or `X-ASSET-LIST=` followed by an `http(s)` or scheme-relative URL outside a recognised attribute (unquoted value, escaped quote, broken attribute list): players parse such lines differently;
  - the rewritten playlist would be larger than 16 times the playlist, at least 1 MiB and at most 64 MiB (metrics reason `size`): a short URI becomes a long resource URL.
- Not supported: `EXT-X-DEFINE` variable substitution, LL-HLS `_HLS_*` query parameters (passed through unchanged, no guarantees).

### 3.3 Raw passthrough

- Upstream body piped with `io.Copy` and flush; no size or total time limit.
- Fail closed: a playlist the proxy does not rewrite is refused with `502` instead of piped, on `raw` and `auto` channels and `/r/` and `/p/` resources, because the player would fetch the URLs in it directly, past the tunnel. Checked on `GET` responses with a `2xx` status (`HEAD` and other statuses send no playlist); `hls` and `dash` channels are always rewritten (3.2, 3.6). Detected, after the HLS and DASH detection of 3.1:
  - by `Content-Type` (case-insensitive, parameters ignored; not on `raw` channels, whose configured type overrides it): PLS `audio/x-scpls`, `audio/scpls`, `application/pls+xml`; M3U `audio/x-mpegurl`, `audio/mpegurl`; XSPF `application/xspf+xml`; ASX `video/x-ms-asx`, `audio/x-ms-asx`, `video/x-ms-wvx`, `audio/x-ms-wax`;
  - by the start of the body (first 512 bytes, after optional UTF-8 BOM and whitespace): `[playlist]` (PLS) or `[Reference]` (ASX reference file), case-insensitive; `#EXTM3U` (reaches this check only on `raw` channels); a first line that is neither blank nor a `#` comment and is an absolute URL (`scheme://`, scheme of two or more characters) or a scheme-relative one (`//`) without control characters (plain M3U); an XML root element `MPD` (DASH, reaches this check only on `raw` channels), `playlist` or `ASX` (case-insensitive, namespace prefix ignored, read as in 3.1 step 2.4), whether the bytes are text or not: also in a legacy encoding or followed by a control byte such as NUL.
  - format `dash`: `<mpd` or `:mpd` (any case) followed by a byte that cannot continue an XML name (anything but `[A-Za-z0-9._:-]`: lighter parsers end the name at `=` too) or the end of the bytes read, anywhere in them (players keep leading text or bytes as a text node and still take the `MPD`).
  - fail closed, format `unknown`: text that is no format the proxy handles: the bytes read are text (after any number of UTF-8 BOMs: no control characters other than TAB, LF, CR and FF, no DEL; bytes from 0x80 are text in any encoding, so UTF-8 and legacy encodings such as ISO-8859-1 and Windows-1252 are text alike, while 512 random bytes avoid control characters with a probability of about 10^-27) and longer than 16 bytes (an AES-128 key may be printable), and are not HLS or DASH (3.1), a refused format above, WebVTT (`WEBVTT` after BOMs and whitespace) or the TTML case below. JSON is refused too: HLS content steering manifests and interstitial asset lists fetched under `/r/` carry URLs the player would fetch directly, so these HLS features do not work through the proxy. A text body is never a media stream; binary bodies (MPEG-TS, fMP4, ADTS, MP3 also from mid-frame, Ogg, FLAC, images, AES ciphertext) stream.
  - fail closed, format `unknown`: every other body that starts with `<` and whose root element step 2.4 finds, text or not (XML or HTML; players such as dash.js and Shaka take a playlist root from any top-level element, also a later one), unless it is one TTML document (text, root element `tt`): whole within the 64 KiB read (3.1 step 2.4), exactly one top-level element followed only by whitespace and comments, no processing instruction after the declaration, doctype, CDATA section, `<` or `>` inside a tag, or comment starting with `-` or holding `<` or `>`. Also a text body whose root element step 2.4 does not find (one of at most 16 bytes only if it is valid UTF-8: an AES-128 key may be printable); any other body without a root element (e.g. binary because of a NUL, while a legacy byte in an attribute or the declaration stops the XML parser) is read lexically: a playlist root (`playlist`, `ASX`, `MPD`, any case, namespace prefix ignored, the name ending at any byte that cannot continue an XML name or at the end of the bytes read) as the first element after an optional XML declaration, comments and whitespace is refused with its format, a body starting with `<?xml`, `<!--` or `<!DOCTYPE` (any case) as `unknown`; everything else without a root element streams (AES ciphertext and MP3 from mid-frame may start with `<`; of random bytes that start with `<` about 1 in 800 begin with a complete start tag and are refused, about 1 in 200,000 random bodies).
  - fail closed, format `unknown`: UTF-16 or UTF-32 text (the proxy reads no playlist in these encodings): a UTF-32 byte order mark (`00 00 FE FF`, `FF FE 00 00`); a UTF-16 byte order mark (`FE FF`, `FF FE`) followed, after any further byte order marks and whitespace code units, by a printable ASCII code unit (every playlist starts with one: `<`, `#`, `[`, a URL) or by nothing, or, in a body longer than 16 bytes (an AES-128 key may start with the mark), followed by code units that are all text (no control characters other than TAB, LF, CR and FF, no DEL, no unpaired surrogate: a playlist whose first line starts with a non-ASCII character); or, without a byte order mark, `<` as a 16-bit (after UTF-16 whitespace) or 32-bit code unit followed by an ASCII character. Media and AES ciphertext start with `FE FF` or `FF FE` by chance in about 1 of 32,768 random bodies (refusing all of them would fail an encrypted 6-second segment about every 2 days of watching); of random bodies of at least 512 bytes, about 1 in 18 million match the UTF-16 rule (about 1 in 560 of those that start with a mark), shorter ones more often as fewer code units must be text (a fifth of 100-byte bodies that start with a mark); the rule without a byte order mark matches about 1 in 17 million, the UTF-32 marks about 1 in 2 billion.
  - Media (MPEG-TS, fMP4, MP3, AAC, Ogg, ICY streams) matches none of these. A `warn` log line names channel, route, URL (8) and format (`pls`, `m3u`, `dash`, `xspf`, `asx`, `unknown`); metrics reason `unsupported` (7.3).
- Shoutcast v1 servers answer `ICY 200 OK`; the raw route's connection wrapper rewrites a leading `ICY ` status line to `HTTP/1.0 ` before Go's HTTP parser sees it.
  - Used for `/c/` requests of `raw` and `auto` channels (the type of `auto` is only known from the response; the wrapper leaves `HTTP/` responses unchanged). These connections have their own keep-alive pool, so repeated polls of an `auto` channel whose URL is a media playlist reuse a connection; `hls` and `dash` channels and `/r/` and `/p/` resources use the normal pool.
  - The first bytes of every response are checked: a server may answer one port with ICY or HTTP per path, also on a kept-alive connection. A connection that carried an ICY response takes no further request (an ICY response has no length and ends with its connection); the next request uses a new connection, even if the server claimed keep-alive. Over `https` the wrapper sees TLS records and changes nothing (Shoutcast v1 has no TLS).
- `Icy-MetaData` is forwarded from the client; `icy-*` response headers are copied back.

### 3.4 Upstream HTTP rules

- Request headers to upstream:
  - `User-Agent`: from merged `upstream.headers` if set, else `[upstream] user_agent`; none if a layer removed it with an empty value (5.4).
  - Merged `upstream.headers` (header names case-insensitive, canonicalised).
  - From the client only: `Range`, `If-Range`, `Icy-MetaData`. `Range` is not forwarded for URLs whose path ends in `.m3u8`, `.m3u` or `.mpd` (case-insensitive, query ignored).
  - The same rules apply on every redirect hop; no automatic `Referer`.
- `HEAD` requests: handled like `GET` (the upstream request is always `GET`), response headers relayed, upstream body closed immediately, no body sent; `auto` type detection uses only the `Content-Type`.
- Response headers copied to the client: `Content-Type`, `Content-Range`, `Accept-Ranges`, `Cache-Control`, `Last-Modified`, `icy-*`; `Content-Length` and `ETag` only if the body is neither rewritten nor transparently decompressed.
- Content codings: Go's transport decodes `gzip` only if it asked for it, not for a request with `Range`, and never decodes `x-gzip`; some servers send pre-compressed objects whatever the request said. A `Content-Encoding` the transport left is handled before the detection of 3.1 and 3.3 and before a body is relayed (`2xx` and relayed `4xx`, also for `HEAD`), on `/c/`, `/r/` and `/p/` and in the probe (7.2), so a compressed playlist is rewritten or refused like the plain one and no body reaches the player encoded without its `Content-Encoding`:
  - no `Content-Encoding`, an empty value, or only `identity` or `none` (case-insensitive; `none` is no registered coding, but servers send it on plain bodies): the body is unchanged;
  - exactly one `gzip` or `x-gzip` (case-insensitive, `identity` and `none` entries ignored): decoded, headers as for a transparently decompressed body (no `Content-Encoding`, `Content-Length` or `ETag`); the limits of this section and of 3.1 and 3.3 count decoded bytes, a streamed body stays without limit like any stream;
  - any other coding or several (`br`, `deflate`, `zstd`, `gzip, gzip`): `502` (metrics reason `other`; fail closed: the proxy cannot see whether the body is a playlist);
  - a `206` with a coding, also one the transport decoded: its `Content-Range` counts encoded bytes, so it is fetched again without `Range` and `If-Range`, like a partial playlist; another `206` gives `502`.
- Rewritten playlists: `Content-Type: application/vnd.apple.mpegurl` (DASH manifests: `application/dash+xml`), uncompressed, `Cache-Control: no-cache`, status `200` (a playlist answered with `206` is refetched without `Range` and `If-Range`; another `206` gives `502`).
- Status mapping: upstream `2xx` and `4xx` passed through; upstream `5xx`, other statuses (e.g. a `3xx` that is not followed) and connection errors return `502`; timeouts return `504`.
- Limits: max 10 redirects (`301`, `302`, `303`, `307`, `308` with `Location`; each hop goes through the guard), playlist body max `[upstream] max_playlist_size` (default 8 MiB, at most 32 MiB; a larger body gives `502`, metrics reason `size`), upstream URL max 4096 bytes (redirect targets included).
- Timeouts (`[upstream]`): `connect_timeout` (10s), `header_timeout` until response headers (15s), `read_timeout` between body reads (30s, implemented as a resettable timer). No total timeout.
  - `connect_timeout` covers DNS lookup and TCP connect of one connection; running out counts as a connection error (`502`), like a tunnel without handshake (2.1).
  - The TLS handshake that follows has its own `connect_timeout`; running out gives `502`, metrics reason `tls`. It also bounds a handshake whose request already ended (client disconnect, `header_timeout`): Go's transport finishes a dial it started even then, so an upstream that never answers holds the connection at most this long.
  - A redirect target is checked like a configured upstream URL before the next hop; one that does not parse (e.g. a scheme-relative `Location` such as `//::/…`) gives `502`, and like every error message it names neither its path nor its query (8).
  - `header_timeout` runs from the start of the request until the final response headers, across redirects; `504`.
  - `read_timeout` runs only while waiting for upstream data, not while the client is slow to accept it; `504` if it fires before the response starts, else the stream ends.
- Error clips (`[clips] error`, default on, 5.3): a `GET` of `/c/` of an HLS channel (listed with `.m3u8`, 6) answers a failure with a short video instead of an HTTP error, so the player shows the error as text and stops instead of a failure dialog.
  - Failures: every `502` and `504` of this section, of the tunnel start (2.1), of the playlist checks (3.2, 3.3) and of the guard (3.5), and an upstream `4xx`.
  - Answer: `200`, `Content-Type: application/vnd.apple.mpegurl`, `Cache-Control: no-store`, a VOD media playlist (`#EXT-X-VERSION:3`, `#EXT-X-TARGETDURATION:5`, `#EXT-X-MEDIA-SEQUENCE:0`, `#EXT-X-PLAYLIST-TYPE:VOD`, `#EXT-X-ENDLIST`) that lists the clip's one 4.8 s segment (`#EXTINF:4.800,`) as `/clip/{name}.ts` (7.1), absolute like resource URLs.
  - Clip per failure: `tunnel-unreachable` ("Tunnel not reachable") when the tunnel did not start (start failure, no handshake within `connect_timeout`, a start cancelled by a reload); `channel-blocked` ("Channel blocked by configuration") for a guard refusal (3.5) or an upstream address outside the tunnel's `AllowedIPs` (5.5, also when a name resolves only to such addresses); `channel-not-available` ("Channel not available") for everything else: DNS, connect, TLS, redirects, size, timeouts, upstream statuses, rejected and refused playlists.
  - Unchanged: `HEAD`, `/r/`, `/p/`, channels listed without `.m3u8` and `error = false` keep the statuses above; an unknown or disabled channel stays `404`.
  - Logs and metrics count the failure as they do without a clip; its `warn` line gets `clip=<name>`. An upstream `4xx` answered with a clip gets the `upstream error status` line of a `5xx`. Served clips count in `error_clips_total` (7.3).

### 3.5 Private-range guard

- Applies to every upstream HTTP dial on every route, checked on the resolved IP at connect time (covers redirects and DNS rebinding).
- Not applied to: the WireGuard `Endpoint` (UDP, host side) and the tunnel's `DNS` servers.
- Blocked: loopback, RFC 1918, RFC 6598, link-local, IPv6 ULA, unspecified (`0.0.0.0/8`, `::`), multicast, IPv4-mapped forms of these. Public IPs and any port are allowed.
- Exceptions per route: `[upstream.allow_private] direct = ["192.168.1.0/24"]`, `gr = ["10.8.0.0/24"]`.
- Blocked addresses of a DNS answer are skipped; if none is left, the request fails like a connection error with `502` (metrics reason `guard`).

### 3.6 DASH rewriting

MPEG-DASH manifests (MPD) without DRM. Players build segment URLs themselves from `SegmentTemplate`, so per-URL signatures (4) cannot work: every base URL of the manifest becomes a prefix-signed URL (4.1) instead, and the player builds the segment URLs below it.

- Stateless like HLS (3.2): the manifest is fetched and rewritten on every player request, so `type="dynamic"` (live) manifests reload through the proxy; `max_playlist_size` applies (3.4).
- Written again from what an XML parser read: every tag with its names as read, attribute values escaped in double quotes and single spaces between attributes, text escaped (line ends read as LF), the XML declaration as `<?xml version="1.0" encoding="UTF-8"?>`; comments are dropped. The output holds only what the rewriter parsed, nothing a lighter player parser could read differently (players end comments and processing instructions at other places, some at the first `-->` after `<!-` or the first `>`, and skip leading non-letters in attribute names, the last attribute of a name winning). Unknown elements and namespaces, whitespace and `SegmentTimeline` stay. Names of the elements and attributes below match by local name, case-insensitively, whatever their namespace prefix; scheme URIs (`schemeIdUri`) compare case-insensitively.
- Rewritten:
  - Every `BaseURL`, on any level, wherever it stands among its siblings (in schema order, descriptors such as the font download come before the `BaseURL` of an `AdaptationSet`; the bases of a level are known before its attributes and children are resolved): resolved against the first `BaseURL` of the level above (the manifest's final URL after redirects at the top) and replaced by the prefix URL of its directory plus its file name and query, e.g. `dash/` becomes `http://wsp.lan:2349/p/{channel}/{sig}/n/{b64prefix}/`. Of its attributes only `serviceLocation`, `availabilityTimeOffset`, `availabilityTimeComplete`, `timeShiftBufferDepth`, `rangeAccess`, DVB's `priority` and `weight` and namespace declarations stay; others are dropped, among them `byteRange` (its templates build URLs). Several `BaseURL` elements on one level (CDN failover) each get their own prefix URL; relative ones on the level below resolve against the first.
  - An `MPD` without its own `BaseURL` gets one for the manifest's directory, as its first child, so references the player would resolve against the proxy URL of the manifest stay below the upstream directory.
  - Segment references (`SegmentTemplate` `media`, `initialization`, `index`, `bitstreamSwitching`; `SegmentURL` `media`, `index`; `sourceURL` of `Initialization`, `RepresentationIndex`, `BitstreamSwitching`; `initialization` of the MPD-level `InitializationSet`): plain relative references stay unchanged and resolve against the rewritten `BaseURL`. Absolute, scheme-relative (`//`), rooted (`/`) and dot-segment (`.`, `..`) references, and relative ones with `:` in the first segment, become the prefix URL of their fixed directory (up to the last `/` before the first `$`, `?` or `#`, resolved against the `BaseURL` above) plus the rest, template identifiers kept.
  - `dvb:url` of a DVB font download descriptor (`SupplementalProperty` or `EssentialProperty` with `urn:dvb:dash:fontdownload:2014`), which players fetch themselves: always the prefix URL of its directory, resolved like a segment reference, plus its file name.
  - `UTCTiming` other than `direct` (`urn:mpeg:dash:utc:direct:2014` or `:2012`): `schemeIdUri="urn:mpeg:dash:utc:http-iso:2014"` and `value` the proxy's `/time` (7.1), so the player never asks an upstream time server.
- Prefix URLs allow a query (mode `q`, 4.1) if the URL they stand for has one; `BaseURL` prefix URLs also if a plain segment reference of the manifest has one.
- Removed with their content: `Location` (the player reloads the proxy URL it has), `PatchLocation` (the player reloads the whole manifest), `ContentSteering`, `Metrics` (reporting URLs), `ImportedMPD` (the MPD a Linked Period imports; the Period stays as a regular one, as after a failed import), `OverlayEvent` (an overlay whose media the player fetches, also in a kept event stream), `SupplementalProperty` of MPD chaining or fallback (`urn:mpeg:dash:chaining:2016`, `urn:mpeg:dash:mpd-chaining:2016`, `urn:mpeg:dash:fallback:2016`), and every `EventStream` and `InbandEventStream` except SCTE-35 (`urn:scte:scte35:…`), ID3 (`https://aomedia.org/emsg/ID3`, `https://developer.apple.com/streaming/emsg-id3`) and MPD expiry (`urn:mpeg:dash:event:2012` with `value="1"`): other events can make the player call a URL (callback events, `urn:mpeg:dash:event:callback:2015`) or load another manifest (alternative MPD, MPD patches, inband MPDs).
- Refused with `502` (fail closed, metrics reason `drm`, 7.3): a manifest with a `ContentProtection` element anywhere. It is never passed through (1.2).
- Rejected with `502` (metrics reason `other`), instead of leaving a URL the player could fetch directly or that players read differently:
  - XML that is not well-formed UTF-8 XML 1.0, a `DOCTYPE` or other declaration (no entities), text or elements outside the single root element, a root element other than `MPD`, an empty `MPD`, nesting deeper than 32 elements;
  - a processing instruction other than the XML declaration at the start (after an optional BOM), a CDATA section outside a `BaseURL`, `<` or `>` inside a tag (in a quoted attribute value): parsers that end these at the first `>` would read elements the rewriter did not;
  - duplicate attributes (also an interpreted attribute twice in another case or with another prefix), `xml:base`, remote elements (`xlink:href` other than `urn:mpeg:dash:resolve-to-zero:2013`);
  - a `BaseURL` with child elements; a `BaseURL`, segment reference or font URL with whitespace (other than around it), control characters, non-ASCII bytes (JavaScript players trim Unicode whitespace such as NBSP) or `\`, that does not parse, does not resolve to `http(s)` with a host, has user info, or has a template identifier before the end of its host;
  - a `Representation` `id` with whitespace, control characters, non-ASCII bytes, `:` or `\`, or starting with `/` (`$RepresentationID$` must not turn a relative reference into an absolute one); an `EssentialProperty` of MPD chaining or fallback;
  - an element or attribute name (namespace prefix or local part) that is not `[A-Za-z][A-Za-z0-9._-]*`: lighter parsers read `_media` or `:media` as a second `media`.
- Fails with `502` (metrics reason `size`) if the rewritten manifest would be larger than 16 times the manifest, at least 1 MiB and at most 64 MiB: every reference that is not plain becomes a whole prefix URL.
- Error clips (3.4) are only for channels listed with `.m3u8`; DASH channels (listed with `.mpd`, 6) get the HTTP status.
- Not supported: `xlink` remote elements, `Location` and `PatchLocation` updates, DRM. A rooted or dot-segment template on a level above a `Representation` with its own `BaseURL` resolves against the base of its own level.

## 4. Resource URLs

- Format: `/r/{channel}/{sig}/{b64url}/{name}`
  - `{channel}`: channel id; route and headers always come from the channel config.
  - `{b64url}`: base64url (no padding) of the absolute upstream URL.
  - `{sig}`: base64url of the first 16 bytes of `HMAC-SHA256(key, channel + "\n" + upstream_url)`.
  - `{name}`: last path segment of the upstream URL, reduced to `[A-Za-z0-9._-]`, max 64 chars with the extension kept; fallback `data` (also for a name of only dots). Ignored by the server, only for player format detection.
- Invalid signature, unknown or disabled channel: `404`. Only the canonical encoding is accepted (no padding, no line breaks, zero trailing bits). No expiry.
- Signing key: a file named by `WG_STREAM_PROXY_SIGNING_KEY_FILE`; raw bytes after trimming leading and trailing ASCII whitespace, minimum 32 bytes, else startup error. An empty variable counts as unset. If it is not set: random key with a warning that resource URLs do not survive restarts. Generate with `openssl rand -base64 32`. The key is only read from a file, never from a plain variable.
- `/c/{id}` URLs in the channel list are unsigned and stable.

### 4.1 Prefix-signed resources

For DASH (3.6): one signature covers every URL below an upstream prefix.

- Format: `/p/{channel}/{sig}/{mode}/{b64prefix}/{path}`
  - `{b64prefix}`: base64url (no padding) of the upstream prefix: `http://` or `https://`, host, optional port and a path that starts and ends with `/`; no user info, query, fragment or dot segments, at most 4096 bytes, in the form of Go's `url.URL.String`.
  - `{mode}`: `n` (requests below it carry no query) or `q` (a query is passed on).
  - `{sig}`: base64url of the first 16 bytes of `HMAC-SHA256(key, "prefix:" + channel + "\n" + mode + "\n" + prefix)`, with the signing key of 4. The `prefix:` label keeps the two kinds apart: a `/r/` signature never verifies as a prefix signature and the other way round.
  - `{path}`: the path the player appended, as it sent it (escaped). The upstream URL is the prefix plus `{path}`, plus `?` and the request's query if the mode is `q`.
- Valid only if the upstream URL stays below the prefix: `{path}` has one or more non-empty segments of RFC 3986 path characters (unreserved, sub-delims, `:`, `@`, valid percent escapes); decoded once, no segment contains `/`, `\`, `%` (double encoding), control characters or non-ASCII bytes (overlong UTF-8 such as `%c0%ae` is a dot for some servers), and no segment name (the part before a `;`) is empty or only dots and spaces (`..`, `%2e%2e`, `...`, `..%20`: Windows servers drop trailing dots and spaces); with mode `n` the request has no query (not even an empty `?`); with mode `q` the query is printable ASCII without space and `#`; the upstream URL is at most 4096 bytes and parses with the prefix's scheme, host and port.
- Upstream redirects (3.4) are followed only if the target is valid below the same prefix by the rules above (same scheme, host and port, path below the prefix directory, a query only with mode `q`); any other redirect fails with `502` (metrics reason `redirects`). The client chooses the path, so an open redirect on the prefix host must not reach other URLs.
- Everything else as for `/r/` (4): invalid signature, path or query, unknown or disabled channel give `404`; only the canonical encoding is accepted; no expiry; route, headers and guard (3.5) come from the channel config at request time, so a reload that changes the channel's route applies to existing prefix URLs too.

## 5. Configuration

### 5.1 Directories and precedence

- Config dir: `--config-dir`, then `WG_STREAM_PROXY_CONFIG_DIR` (the container image sets it), then `/etc/wg-stream-proxy` if it exists; else a startup error that names the flag and the variable. There is no `$XDG_CONFIG_HOME` or home directory fallback.
- Tunnel dir: `<config-dir>/tunnels`.
- Read non-recursively: `<config-dir>/config.toml`, `<tunnels-dir>/*.conf`, `<config-dir>/channels.d/*.toml` (channel files, plus the optional global layer `channels.d/_defaults.toml`, 5.4); other files are ignored. A missing `config.toml`, tunnels dir or `channels.d` counts as empty (defaults, no tunnels, no channels); a missing config dir is an error. Channel order: by file name, then by position in the file. Local logos: `<config-dir>/assets/logos/` (flat).
- Flags (a CLI flag beats `config.toml`): `--listen`, `--log-level`, `--log-format`. Empty values count as not set. `public_base_url` is only set in `config.toml`.
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
public_base_url = ""                  # default: from the request (scheme, Host)

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

[epg]
# url = "https://epg.example.org/epg/epg.xml.gz"  # link mode (6): the player fetches it; optional, not together with [[epg.source]]
refresh = "6h"                        # proxy mode: time between two fetches of a source, at least 1m

[[epg.source]]                        # proxy mode (6.1): the proxy fetches, filters and merges
name = "gr"                           # unique, [a-z0-9-]; used in logs, metrics and epg_source
url = "https://epg.example.org/epg/epg.xml.gz"
                                      # tunnel omitted: direct
[[epg.source]]
name = "de"
url = "https://epg.example.net/guide.xml.gz"
tunnel = "de"                         # any number of sources may share a tunnel

[debug]
listen = ""                           # off; e.g. "127.0.0.1:2350" (7.1)
```

Unknown keys and wrong value types are config errors. Values are validated: `listen` is `[host]:port` with a numeric port; `debug.listen` is empty or of the same form, with a host that is empty, an IP address (IPv6 in brackets, zones allowed) or `localhost` (the debug listener answers no other name, 7.1, so the CLI could not reach it) and a port other than the one of `listen` (as in effect after the flag); durations are positive (`reload.watch_interval` also accepts `"0"`, which turns watching off; `epg.refresh` is at least `1m`); `max_playlist_size` is a whole number directly followed by `B`, `KiB` or `MiB`, positive and at most `32MiB`; `public_base_url` and `epg.url` are empty or absolute `http(s)` URLs; `user_agent` has no line breaks; `allow_private` keys are `direct` or existing tunnel names, values are CIDR prefixes without host bits. `[[epg.source]]`: `name` required, `[a-z0-9-]`, 1-48 characters, unique; `url` required, absolute `http(s)`, at most 4096 bytes; `tunnel` `direct` or an existing tunnel name. `epg.url` together with `[[epg.source]]` is a config error: link mode (the player fetches `epg.url`) or proxy mode (the proxy serves the guide, 6.1).

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
type = "auto"                         # auto | hls | dash | raw
headers = { Referer = "https://www.example.org/" }

[channel.playlist]
tvg_id = "ertworld"
epg_source = "gr"                     # optional: the guide of this channel only from this [[epg.source]] (6.1)
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
- `channels.d/_defaults.toml` (optional): the global layer for all channel files, e.g. one `inputstream.adaptive` for every channel (deploy.md). It takes only `[defaults.upstream]` (`type`, `headers`) and `[defaults.playlist]` (`logo`, `epg_source`, `attrs`, `extra.<DIRECTIVE>`), validated like file defaults. `tunnel` (a global route would bypass fail closed, 5.2), `group`, `[[channel]]` and `tvg_id` are config errors. It is not a channel file: no channels, no file-stem route. Other `channels.d/_*.toml` names are reserved (config error).
- Channel `url`: absolute `http(s)` URL, max 4096 bytes. `name`: required, not blank.
- `upstream.headers`: names are HTTP tokens, values without CR, LF or NUL; two names in one layer that differ only in case are a config error. `Host` (net/http sends the host of the URL, so it would have no effect) and `Accept-Encoding` (it turns off the transparent decompression of 3.4) are config errors, in any case and on every layer (global defaults, file defaults, channel), also with an empty value.
- No line breaks in `name`, `group`, `playlist.tvg_id`, `playlist.logo` and `attrs` values.
- `playlist.epg_source`: the name of an `[[epg.source]]` (5.3), else a config error (also in link mode); the channel's guide comes only from that source (6.1). Empty: the first source that has the `tvg_id`, also over a lower layer's value. Two channels with the same `tvg_id` that name different sources are a config error (the guide has one `<channel>` element per id).
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
- A reload builds new routes (`direct` and tunnels): new requests use new upstream connections (one extra handshake per host) and the old routes' idle connections are closed. Requests in flight finish on the old routes; when the last of them ends, the idle connections they left there are closed as well, and the old routes are not kept. An unchanged tunnel keeps running.
- A reload that changes or removes a tunnel which is still starting cancels the start; requests waiting for it get `502`; the new tunnel starts lazily on the next request.
- `[epg]` and `[[epg.source]]` apply on reload without a restart: a reload that changes the sources or the wanted ids fetches every source again at once (6.1).
- Restart required for: `listen`, `debug.listen`, `log_format`, `reload.watch_interval`, signing key. A reload whose value differs from the one in effect (the value at startup, not the last loaded one) logs a warning and keeps the value in effect; the warning names both values, except for the signing key. A reload back to the value in effect logs nothing.
- A successful reload logs one info line `reloaded` with the channel and tunnel counts, `channels_added`, `channels_changed` and `channels_removed` (counts; changed means any difference of the merged channel config, comments, formatting and order do not count) and `tunnels_added`, `tunnels_changed` and `tunnels_removed` (names); a reload that changes nothing logs the same line with zeros. At debug level a second line lists the ids of the added, changed and removed channels; no line contains URLs.

## 6. Channel list

- `GET /playlist.m3u8`, `Content-Type: audio/x-mpegurl; charset=utf-8`, no BOM; disabled channels omitted; the query string is ignored.
- Header: `#EXTM3U x-tvg-url="<epg>" url-tvg="<epg>"`: with `[[epg.source]]` the guide of the proxy, `<base>/epg.xml.gz` (proxy mode, 6.1, absolute like the channel URLs, 7.1); else `epg.url` if set (link mode: the player fetches it); else no attributes.
- Per channel:
  1. `playlist.extra` lines, sorted by directive, then key; keys removed with an empty value (5.4) are not written.
  2. `#EXTINF:-1 tvg-id=".." tvg-name=".." tvg-logo=".." group-title=".."`, then `attrs` sorted by key, then `,<name>`. Empty values are omitted; `"` in values and names is replaced by `'`. Commas in names are fine (IPTV Simple Client takes the name after the last quoted attribute). Kodi lists a channel in its Radio section only with `radio="true"`, which comes from `attrs = { radio = "true" }` (no extra key).
  3. URL: `/c/{id}.m3u8` if `upstream.type = "hls"`, `/c/{id}.mpd` if it is `"dash"`; otherwise `/c/{id}.m3u8` if the upstream URL path contains `.m3u8`, `/c/{id}.mpd` if it contains `.mpd`, else `/c/{id}`. The handler strips a trailing `.m3u8` or `.mpd` from the id.
- `#KODIPROP` and other `extra` lines appear only here, never in playlists or manifests under `/c/`, `/r/` or `/p/`. Kodi plays DASH only with `inputstream.adaptive` ([deploy.md](deploy.md)).

```
#EXTM3U x-tvg-url="https://epg.example.org/epg/epg.xml.gz" url-tvg="https://epg.example.org/epg/epg.xml.gz"
#KODIPROP:inputstream.adaptive.manifest_type=hls
#EXTINF:-1 tvg-id="ertworld" tvg-name="ERT World" tvg-logo="EPTWORLD" group-title="Greek TV" parent-code="1234" tvg-chno="5",ERT World
http://wsp.lan:2349/c/ert-world.m3u8
```

### 6.1 EPG proxy mode

With `[[epg.source]]` (5.3) the proxy fetches the EPG sources, keeps of each only the configured channels and serves them merged as one XMLTV guide: Kodi's IPTV Simple Client takes one EPG URL, and a real source (tens of MB of XML, hundreds of channels, days of programmes) is mostly channels nobody configured.

- Fetch: each source through its route (`tunnel`, else `direct`), one after the other in config order, with the upstream rules of 3.4: private-range guard (3.5, with the route's `allow_private`), `[upstream]` timeouts and `User-Agent`, at most 10 redirects. A fetch, download and filter, takes at most 5 minutes in total, else it fails (`timeout`). A source on a tunnel starts the tunnel like a channel request does (2.1); it closes again after `idle_timeout`.
- Body: status `200`; gzip (recognised by its magic bytes, not by name or header) or plain XML; at most 200 MiB after decompression, which also bounds a gzip bomb, else the fetch fails (`size`). The source is read as a stream and never held in memory.
- When: at startup; then each source every `[epg] refresh` (default `6h`), counted from the start of its last fetch; a failed source again after 10 minutes (or `refresh` if shorter); every source at once after a reload (5.6) that changes the sources (names, URLs, tunnels, order) or the wanted ids (`tvg_id` and `epg_source` of the enabled channels). A changed `refresh` alone moves the next fetch times without a fetch.
- Filter: the `<channel>` elements whose `id` is the `tvg_id` of an enabled channel (the first element per id) and the `<programme>` elements of these ids that have not ended at the time of the fetch: `stop` of the form `YYYYMMDDhhmm` or `YYYYMMDDhhmmss`, optionally followed by a zone `+hhmm` or `-hhmm` (no zone: UTC), at or before that time. Programmes without `stop` or with another form are kept. A source is filtered only for the ids it may provide (`epg_source`, 5.4). Ids compare exactly.
- Fail closed: a source that is not well-formed XML 1.0 (strict: undefined entities, invalid UTF-8, mismatched end tags; a declared encoding other than UTF-8 is converted), whose root element is not `tv`, with text or elements outside the root, nesting deeper than 32 elements, or a token (tag with its attributes, text, comment, CDATA section, declaration) larger than about 1 MiB fails (`invalid`): the parser holds one token at a time, so memory stays small however the source is built. A doctype is accepted and ignored; its entities are never expanded.
- Written again: the guide holds only what the source had for the kept channels, re-serialised from the parsed tokens, never copied: `<?xml version="1.0" encoding="UTF-8"?>`, `<tv>` without attributes, then the kept `<channel>` and `<programme>` elements with their content. Element and attribute names must be `[A-Za-z][A-Za-z0-9._-]*` without namespace prefix: other elements are dropped with their content, other attributes dropped, of repeated attributes only the first is kept; values in double quotes and text escaped; comments, processing instructions and doctype dropped, CDATA sections written as escaped text; an element without content as `<name/>`. So players with lighter XML parsers read the same guide.
- Merge: per id, in the order of the channels (5.1), the `<channel>` element and all programmes come from one source: the one named by `epg_source`, else the first source in list order whose copy has a `<channel>` element with the id. Programmes of one channel are never mixed across sources; an id no source has is missing. All `<channel>` elements come first, then the programmes (XMLTV DTD order).
- Failure: a failed source keeps serving its last good copy, the other sources still update; its channels are not handed over to another source. A removed source, and one whose URL or tunnel changed, loses its copy.
- Memory: only the filtered copies and the guide (plain and gzip) are held.
- Serving: `GET/HEAD /epg.xml.gz` (`application/gzip`) and `/epg.xml` (`application/xml; charset=utf-8`), with `ETag` (one per form), `Last-Modified` (when the guide last changed), `Cache-Control: no-cache`, conditional and range requests. `503` until a source was fetched once (sources not fetched yet are missing from the guide until they are); `404` without `[[epg.source]]` (also in link mode).
- Log: per fetch an `info` line `epg source fetched` (`source`, `route`, `url` in the logged form of 8, `bytes`, `filtered_bytes`, `channels`, `programmes`, `duration`) or a `warn` line `epg source failed: keeping the last good copy` (`source`, `route`, `url`, `result`, `error`, `duration`, `serving_copy_age` or `none`); an `info` line `epg guide updated` (`channels`, `programmes`, `bytes`, `gzip_bytes`) when the guide changed. Metrics: 7.3.

## 7. Interfaces

### 7.1 HTTP

| Path | Description |
|---|---|
| `GET/HEAD /playlist.m3u8` | Channel list |
| `GET/HEAD /epg.xml.gz`, `/epg.xml` | EPG guide of the proxy, gzip and plain (proxy mode, 6.1); `404` without `[[epg.source]]`, `503` before the first fetch |
| `GET/HEAD /c/{id}`, `/c/{id}.m3u8`, `/c/{id}.mpd` | Channel entry |
| `GET/HEAD /r/{channel}/{sig}/{b64url}/{name}` | Signed resource |
| `GET/HEAD /p/{channel}/{sig}/{mode}/{b64prefix}/{path}` | Prefix-signed resource (4.1) |
| `GET/HEAD /time` | Current UTC time for DASH players (3.6), ISO 8601 with milliseconds (`2026-11-19T12:01:30.500Z`), `text/plain; charset=utf-8`, `Cache-Control: no-store`; no upstream request, no metrics |
| `GET/HEAD /logos/{file}` | Local logo, flat directory, no traversal (5.4) |
| `GET/HEAD /clip/{name}.ts` | Built-in error clip segment (3.4): `{name}` one of `tunnel-unreachable`, `channel-blocked`, `channel-not-available`; 4.8 s of MPEG-TS, timestamps from 10 s. `video/mp2t`, unsigned; no upstream request. Anything else under `/clip/`: `404` |
| `GET /healthz` | 200 while serving; body: version and source URL (AGPL section 13) |
| `GET /metrics` | Prometheus |

Absolute URLs in responses use `public_base_url`, else the scheme of the request (`https` if TLS, else `http`) and its `Host`. `X-Forwarded-Proto` and `X-Forwarded-Host` are not read: behind a reverse proxy set `public_base_url`. Without a usable `Host` (empty, or characters outside `[A-Za-z0-9.:_[]-]`), the local address of the connection is used.

#### Debug endpoints

Served only on their own listener `debug.listen` (5.3), off by default and never on `listen`; no authentication (8). Unsafe methods from a browser page of another origin (`Sec-Fetch-Site`, `Origin`) are rejected with `403`. So are requests whose `Host` is not an IP address or `localhost` (`403`, against DNS rebinding): address the listener by IP, not by host name. `debug.listen` itself must therefore name an IP address, `localhost` or no host (5.3), as the CLI (7.2) calls it by that host.

| Path | Description |
|---|---|
| `/debug/pprof/…` | Go profiler (`net/http/pprof`: index, `profile`, `heap`, `goroutine`, `trace`, …) |
| `GET /debug/tunnels` | JSON `{"tunnels": [...]}`: per configured tunnel, sorted by name |
| `POST /debug/tunnels/{name}/stop` | Cool-down of a tunnel |
| `POST /debug/probe` | The `probe` command (7.2) run by the server: JSON in, JSON out |

- `/debug/tunnels`: `name`, `state` (`stopped`, `starting`, `running`, `draining`), `started_at` (first handshake of the running device) and `last_handshake` (RFC 3339, omitted while no device runs), `active_requests`.
- Cool-down: the tunnel is drained like a reload removal (2.1: open streams finish, or the 30s drain timeout cuts them; requests arriving meanwhile use the running device), then its device stops, the route's pooled connections close and its DNS cache is emptied; `tunnel closed reason=cooldown` is logged and the next request starts it cold. The answer comes when the tunnel is stopped: `200` with `{"name": "<name>", "was_running": true}`; for a tunnel that was not running nothing changes and `was_running` is `false`. Unknown tunnel (also `direct`): `404`; other methods: `405`.
- `POST /debug/probe`: request body (at most 16 KiB) a JSON object `{"route": "direct", "url": "https://…", "headers": {"Name": "Value"}}`; `route` defaults to `direct`, `headers` is optional, unknown fields are refused. The URL is in the body, never in the query string, so it stays out of access logs. The server probes with its current configuration (`probe`, 7.2): the route of the running generation, so a running tunnel is used and no second device starts, with its private-range guard, `[upstream]` settings and limits; a probe takes at most 60s. The answer is `200` with the JSON of `probe --json` (`"mode": "proxy"`), also if the probe failed (`ok: false`); `400` for an invalid body, URL or header, `404` for an unknown route, `413` for a larger body, each with `{"error": "…"}` without the URL; other methods: `405`. A probe is no channel request: it counts in no metric (7.3); it logs one `info` line `probe` with `route` and `url` in the logged form (8).

### 7.2 CLI

```
wg-stream-proxy serve                 # default
wg-stream-proxy check                 # validate config, exit 0/1
wg-stream-proxy probe [--route R] [--header K=V]... [--json] <url>   # fetch a URL through a route of the running proxy, report format and timing, exit 0/1/2
wg-stream-proxy epg ids [--source S]  # list id and display-name of an EPG source (config flags as for check)
wg-stream-proxy healthcheck           # GET /healthz on the listen address, exit 0/1
wg-stream-proxy tunnel list           # tunnels and their state, from the debug endpoints (7.1)
wg-stream-proxy tunnel stop <name>    # cool-down of a tunnel (7.1)
wg-stream-proxy version               # print version and source URL (AGPL section 13)
wg-stream-proxy help [command]        # command list, or the usage and flags of a command
```

`help`: `wg-stream-proxy help`, `-h` and `--help` print one static text: a one-line description, the usage line and the list of commands with one line each, to stdout, exit 0. `help <command>` (`help epg ids`) and `<command> -h` (also after other flags, `serve --listen :1 -h`) print the usage line and the flags of that command to stdout, exit 0 (the output of `flag.PrintDefaults`); for `epg` and `tunnel` the usage line lists the subcommands. Only a first argument `-h` or `--help` is the help of the program, so `serve -h` stays the help of `serve`. An unknown command, also for `help <unknown>`, prints one error line and the command list to stderr and exits 2. A flag error prints the flag package's message and a pointer to `help <command>` to stderr and exits 2. Without any argument the program runs `serve`; it prints no help.

`serve` stops on `SIGINT` or `SIGTERM`: it accepts no new connections, gives in-flight requests up to 30s (2.1), then closes the remaining connections and the tunnels; a second signal exits at once.

`check` also parses the tunnel files and the signing key variable (4), lists every error found and starts nothing.

`healthcheck` resolves the listen address like `serve` (same flags and `WG_STREAM_PROXY_CONFIG_DIR`): it reads only `listen` and `[debug] listen` from config.toml with the overrides applied, not the tunnel and channel files, so an edit the serving proxy rejects as a reload (5.6) does not leave the container without a working healthcheck. It sends `GET /healthz` to the listen address (an unspecified host such as `:2349` or `0.0.0.0` becomes `127.0.0.1`, `[::]` becomes `[::1]`) with a 3s timeout, follows no redirect and prints nothing on `200` (exit 0). Any other result, a connection error or an invalid config.toml prints one line to stderr and exits 1. It needs no shell or curl, so it serves as the `HEALTHCHECK` of the container image.

`tunnel list` and `tunnel stop <name>` read `[debug] listen` like `healthcheck` (only the addresses of config.toml, not the tunnel and channel files) and call the debug endpoints (7.1) on `debug.listen` (unspecified host as for `healthcheck`), so they work through `docker exec` in the image. `list` (`GET /debug/tunnels`, 3s timeout) prints a table: name, state, active requests, start and last handshake time. `stop` (`POST /debug/tunnels/{name}/stop`, 45s timeout, covering the drain) waits until the tunnel is stopped and prints `tunnel <name> stopped`, or `tunnel <name> was not running`. The name may stand before or after the flags. Debug listener off, unknown tunnel, a connection error or another status: one line on stderr, exit 1; usage errors (missing or invalid name): exit 2.

`probe [--route <name>] [--header Name=Value]... [--json] <url>` answers "does this URL work through this route?" without adding a channel. It reads only the addresses of config.toml like `tunnel list` (same flags and `WG_STREAM_PROXY_CONFIG_DIR`). `--route` is `direct` (default) or a tunnel name; any other name is answered by the proxy with `404` and exits 2. The URL must be absolute `http(s)`, may stand before, between or after the flags and is never echoed; `--header` is repeatable, a later value of a name replaces an earlier one, an empty value drops the header (for `User-Agent`: sends none). It makes the requests of serving: the same fetch (private-range guard with `[upstream.allow_private]`, `[upstream]` timeouts, `User-Agent` and redirect limit, 3.4), the same content codings (3.4), detection and refusals (3.1, 3.3: also an ICY status line is accepted), a playlist or DASH manifest read whole within `max_playlist_size` and checked by its rewriter (3.2, 3.6; a manifest with DRM fails with class `drm`); no `Range`.
- Running proxy required: the running proxy makes the requests (`POST /debug/probe`, 7.1), through its running tunnel, so no second device with the same key ever starts; it uses its loaded configuration, so a changed channel, tunnel or config file counts only after the reload (5.6, file watching). Without `[debug] listen`, or if nothing listens there (connection refused), the command exits 1 with one line on stderr that says to set `[debug] listen` in config.toml and start the proxy; it makes no request of its own. If the debug listener answers with an error (not found, bad request, server error, an answer that is no probe report), the error is printed and the command exits (1; 2 for `400` and `404`). The output shows `"mode": "proxy"` in JSON and `(via running proxy)` after the route in the text form.
- Steps: the URL; if it is an HLS master playlist, its first variant (which must be a media playlist); if the last playlist is a media playlist, its first segment (only the first bytes are read). A step that fails ends the probe.
- Per step (text, or JSON with `--json`: one object `{"mode", "route", "ok", "steps": [...]}`): `role` (`url`, `variant`, `segment`); `url`, the final URL after redirects in the logged form (8: scheme, host and file name only); `status`; `content_type` (control characters replaced); `redirects` if any; `size` (a playlist as read, else `Content-Length`) or `streaming: true` for a body without length; `format` (`hls-master` and `variants`, `hls-media` and `segments`, `raw`, `icy` for a raw stream with `icy-*` headers, `dash` and `periods` and `adaptation_sets` for a DASH manifest, `refused`, empty for a segment); `refused` the playlist format of a refused response (`pls`, `m3u`, `xspf`, `asx`, `unknown`: the formats of 3.3; a probe reads like an `auto` channel, so DASH is `dash`, not refused); `ok`; `class` and `error` on failure; `timing`.
- `class`: the reasons of `upstream_errors_total` (7.3: `connect`, `timeout`, `guard`, `tls`, `redirects`, `size`, `unsupported`, `drm`, `other`) and `status` for an upstream status other than `2xx`. `error` carries no query string or path beyond the file name (8).
- `timing` (milliseconds, to the microsecond): `total_ms`, and only for phases that happened `tunnel_start_ms`, `dns_ms`, `connect_ms`, `tls_ms`, `ttfb_ms` (the phases of the debug line and `upstream_phase_seconds`, 7.3, 8), plus `conn_reused` for a pooled connection.
- Exit code 0 if the proxy would serve the URL (every step `ok`), 1 if not (failed step, invalid config, no running proxy, tunnel that does not start), 2 for usage errors (no or several URLs, invalid URL, header or route, flag errors).

`epg ids` downloads one EPG file: the `[[epg.source]]` named by `--source`, by default the first one, else `epg.url`; `--source` with a name that is not an `[[epg.source]]` exits 2. The file is fetched through the source's route (`epg.url`: `direct`): the private-range guard (with the route's `[upstream.allow_private]`) and the `[upstream]` timeouts apply. A source on a tunnel starts that tunnel in the command's process, with a note on stderr: this starts a second WireGuard device with the key of the tunnel, so a running proxy with the same tunnel key may stall until its next handshake. The file is plain XMLTV or gzip (recognised by its magic bytes), read as a stream, at most 200 MiB after decompression (else error); only the `<channel>` elements before the first `<programme>` are read (XMLTV DTD order).

`epg ids`: one line per EPG channel, sorted by id: the id, then its display names, separated by tabs.

### 7.3 Metrics (prefix `wsp_`)

- `tunnel_up{tunnel}`: 1 while the device runs and the last handshake is younger than 180s.
- `tunnel_last_handshake_timestamp_seconds{tunnel}`: `0` while the tunnel is not running or has no handshake yet.
- `active_requests{channel,kind}`: in-flight requests incl. open raw streams; `kind` = `playlist`, `segment`, `raw`.
- `upstream_requests_total{channel,kind,code}`: `code` = upstream status, or `error`.
- `upstream_errors_total{channel,reason}`: `reason` = `connect`, `timeout`, `guard`, `tls`, `redirects`, `size`, `unsupported`, `drm`, `other`.
- `error_clips_total{clip}`: error clips served instead of an HTTP error (3.4), by clip name (`tunnel-unreachable`, `channel-blocked`, `channel-not-available`; each starts at 0). The failure behind it counts as without a clip.
- `tunnel_start_seconds{tunnel}` (histogram): one observation per successful tunnel start, the time from the start request until the first handshake completed (device up included). Observed where the tunnel starts, once per start however many requests waited for it; a failed start is not observed (the start failure line, spec 8, logs it).
- `upstream_phase_seconds{route,phase}` (histogram): where the time of the upstream requests of `/c/`, `/r/` and `/p/` goes, per route (`direct` or a tunnel name), `phase` = `dns` (host lookups), `connect` (connection setup, Happy-Eyeballs fallback included), `tls` (TLS handshakes), `first_byte` (start of the upstream request until the first response byte); the same definitions as the debug line (`dns`, `connect`, `tls`, `ttfb`, spec 8), without a tunnel start wait (that is `tunnel_start_seconds`).
- Buckets of both histograms, in seconds: `0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 15` (classic histograms).
- EPG proxy mode (6.1), per `[[epg.source]]` (`source` is its name, so the labels are bounded by the config; a removed source disappears):
  - `epg_fetches_total{source,result}`: fetches by `result` = `ok`, `connect`, `timeout`, `guard`, `tls`, `redirects`, `size` (larger than 200 MiB), `status` (other than `200`), `invalid` (no gzip or XMLTV the filter accepts), `other`; every result is reported from 0.
  - `epg_last_success_timestamp_seconds{source}`: when the copy in use was fetched, `0` without one; `time()` minus it is the age of what the guide serves for the source.

Only `/c/`, `/r/` and `/p/` requests of enabled channels count; `channel` is the channel id, never a URL. `/clip/` requests count in no metric; EPG fetches only in the `epg_` metrics.

- `kind`: `playlist` if the response is detected as a playlist or DASH manifest (3.1) or refused as one (3.3), else `raw` for `/c/` and `segment` for `/r/` and `/p/`. Until the type is detected, and if it never is (error, non-`2xx` status), an `hls` or `dash` channel and, without a configured type, an upstream path ending in `.m3u8`, `.m3u` or `.mpd` count as `playlist`.
- `upstream_requests_total` counts every upstream request, the refetch of a partial playlist or of a `206` with a content coding (3.4) included.
- `upstream_errors_total` counts failures before the response (with `code="error"`) and after it: playlist body or sniff read errors, a rejected playlist, a second `206` or a content coding the proxy does not decode (`other`, 3.4), a refused playlist format (`unsupported`, 3.3), a refused DRM manifest (`drm`, 3.6), a rewritten playlist or manifest over its output limit (`size`, 3.2, 3.6), a redirect of a `/p/` request out of its prefix (`redirects`, 4.1), a stream ended by an upstream error. Upstream statuses count only in `upstream_requests_total`.
- A request the client gave up counts neither as error nor, if no response came, as upstream request.
- Timing histograms: labels are the route or tunnel name and the phase only, never a channel or URL. They are collected for every `/c/`, `/r/` and `/p/` request at every log level and recorded when the request ends (a stream: when it closes). A request observes only the phases that happened: no `dns` for an IP literal or a pooled connection, no `connect` for a pooled connection, no `tls` for plain HTTP, no `first_byte` without a response; phases that failed (a refused connect, a failed handshake) count with their duration. `first_byte` is observed once per request, for the last upstream response (a refetch, 3.4, does not count twice). Requests the client gave up observe what happened before.

`/metrics` also serves the standard `go_*` and `process_*` metrics of the Prometheus Go client.

## 8. Operations and security

- Intended for LAN use; exposure to the internet needs an authenticating reverse proxy.
- WireGuard keys only in tunnel files and never logged; upstream URLs (in log lines and in error messages) are logged as scheme, host and file name only, e.g. `https://cdn.example.com/…/7.m3u8`: no user info, query string, fragment or other path segment, because CDNs put session ids and tokens there. The file name is reduced like `{name}` (spec 4); a URL without path shows scheme and host only.
- Debug log (`log_level = "debug"`): one `request` line per `/c/`, `/r/`, `/p/` and `/clip/` request when it ends, with request id, channel, route, kind, client status and bytes, client disconnect, upstream status and error reason, and where the time went: `tunnel_start` (only if the request waited for a tunnel start), `dns`, `connect` (Happy-Eyeballs fallback included), `tls`, `ttfb` (first upstream byte), `total`; drain waits and retired tunnels; wireguard-go's own events (handshakes, keepalives, retries) with the peer shown by tunnel name. Tunnel start (with handshake), start failure and close lines carry their durations at every level. The log never contains keys, key fragments, query strings or upstream paths beyond the file name. Above debug the debug lines cost only a level check; the timing behind them is collected at every level for the timing metrics (7.3).
- Debug endpoints (7.1): off by default, on their own listener, without authentication. They answer only requests addressed by IP or `localhost`, so a web page cannot reach them by rebinding its own name to the debug address. Bind them to loopback (e.g. `127.0.0.1:2350`, used through `docker exec … tunnel`) or at most the LAN, never beyond: the profiler exposes process internals (memory, goroutine stacks, command line), and whoever reaches the port can cool tunnels down.
- Prefix-signed URLs (4.1): whoever has a valid `/p/` URL can fetch every URL below its prefix (same scheme, host and port, below the prefix directory) through the channel's route, with the channel's headers, and with a query only if the mode allows one; nothing outside the prefix (also not through an upstream redirect, which is followed only below the prefix), nothing for another channel, and nothing once the channel is removed or disabled. A response below a prefix that is itself a playlist or manifest is rewritten and signed again, as under `/r/`; the guard applies to every dial.
- DASH manifests (3.6) reach the player only as the rewriter parsed them: written again token by token, names restricted to plain XML names, comments and processing instructions gone, so players' lighter XML parsers (which end comments early, skip leading non-letters in attribute names and let a later attribute win) read the same URLs. XML bodies that are not rewritten are refused, never piped (3.3), except a single complete TTML document within 64 KiB: a later top-level `MPD`, a prolog with `<` or `>`, a document longer than the window, HTML, other text that is no handled format and UTF-16 or UTF-32 text (with or without byte order mark) never reach the player.
- EPG sources (6.1) are untrusted input: fetched like channel upstreams (guard, timeouts, redirect limit) plus a total time limit, read as a stream with a size limit after decompression (gzip bombs end at 200 MiB), parsed strictly (no entity expansion, depth limit, tokens of at most about 1 MiB, so a small gzip cannot make the parser hold a huge text), and the guide is written again from the parsed tokens with plain names only, so it is well-formed and holds nothing the source did not have for the configured channels. URLs inside the guide (icons, programme links) are data the player may fetch directly, past the tunnel, like logo URLs in the channel list; the proxy does not rewrite them.
- Residual risk of the sniffing (3.1, 3.3): only the first 512 bytes of a body that does not start like XML text are read (64 KiB for XML). A malicious upstream that sends binary bytes longer than that window followed by an `MPD`, opened by a browser DASH player (dash.js, Shaka) that searches a whole response for an `MPD` element, cannot be detected without buffering whole streams. The same holds for a WebVTT body with an `MPD` after the first 64 KiB. Kodi, ffmpeg and VLC do not read such a body as a manifest. The proxy targets LAN players, and upstreams are configured by the owner. A prefix is the directory of a `BaseURL` or segment reference of a manifest the channel served, so a manifest whose `BaseURL` is a host root opens that whole host. Like `/r/` URLs they do not expire and reach the LAN players only (above).
- Container: non-root, read-only root filesystem, all capabilities dropped, single replica. Details: [deploy.md](deploy.md).

## 9. Engineering rules

- Layout: `cmd/wg-stream-proxy/`, `internal/{config,tunnel,route,hls,dash,xmlw,playlist,server,epg,probe,metrics,clips}`, `examples/config/`, `testdata/`, `deploy/`, `docs/`.
- Dependencies: stdlib `net/http`, `log/slog`, `wireguard-go`, gVisor netstack, a TOML parser, Prometheus client. No web framework.
- `internal/clips` embeds the built-in error clips (3.4): three 4.8 s MPEG-TS files in `internal/clips/segments/`, rendered by `internal/clips/generate.sh` (pinned image, Inter 4.1 by version and checksum) and committed. Change a text by editing the script, rerunning it and committing the result. Only rendered pixels ship, not the font (SIL OFL 1.1).
- gVisor stays at the version in `wireguard-go`'s `go.mod` and is only updated together with it (excluded from automatic bumps).
- `internal/tunnel/netstack` is a trimmed copy of `wireguard-go`'s `tun/netstack` (MIT, licence kept) with one change: a packet gVisor sends after `Close` is dropped instead of sent on a closed channel, which would crash the process (#47). The copy names the upstream commit it was taken from and is updated together with `wireguard-go`: on every `wireguard-go` update, compare upstream `tun/netstack` with the copy and port relevant changes.
- Injected for tests: `Dialer` per route, private-range guard allowlist, clock.
- Client disconnect cancels the upstream request. HTTP server: `ReadHeaderTimeout` set, no `WriteTimeout`.
- `atomic.Int64` and similar types only, never `atomic.AddInt64` on plain fields.
- Code comments in English; file header `// SPDX-License-Identifier: AGPL-3.0-or-later`.

## 10. Roadmap

- Released ([changelog](../CHANGELOG.md)): **v0.1 Core** (config, tunnels, routes and guard, HLS and raw/ICY streams, channel list, `SIGHUP` reload, metrics, container image), **v0.2 Observability** (timing histograms, debug endpoints), **v0.3 Experience** (error clips, file-watch reload), **v0.4 Content** (DASH without DRM, EPG proxy mode, `probe`).
- **v0.5 Lean** ([#129](https://github.com/KingOfKalk/wg-stream-proxy/issues/129)): a smaller project that is cheaper to maintain: fixes, removal of unused features, shorter docs, leaner tests and CI, less duplicated code. Breaking changes allowed.
- **Later**, only if the timing data (7.3) shows segment loading as a bottleneck: per-request segment prefetch ([#84](https://github.com/KingOfKalk/wg-stream-proxy/issues/84)).
- Decided against: in-flight clips ([#116](https://github.com/KingOfKalk/wg-stream-proxy/issues/116), the design is in the git history), logo proxy ([#88](https://github.com/KingOfKalk/wg-stream-proxy/issues/88)), ad-hoc proxy endpoint ([#89](https://github.com/KingOfKalk/wg-stream-proxy/issues/89)).

## 11. Risks

- WireGuard encryption is CPU-bound on small ARM boards; 3 HD streams on a Raspberry Pi 5 are expected to fit, measured by the L6 load test; a Raspberry Pi 4 is not tested (no device available).
- Geo-IP databases may disagree on a tunnel's exit country; a broadcaster can see a different country than expected and block the stream.
- Some upstreams need cookies or tokens from a website session; only static headers are supported.

Prior art: wireproxy (userspace WireGuard proxy, one tunnel per process), MediaFlow Proxy (HLS proxy with per-domain proxy routing), Dispatcharr (IPTV manager with one VPN via Gluetun). None selects a tunnel per channel inside one process.
