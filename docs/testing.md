# wg-stream-proxy - Testing strategy

Companion to [spec.md](spec.md). Section numbers refer to the spec.

## Principles

- No real broadcaster URLs in CI: they are geo-dependent, flaky and change. All upstreams in CI are local fixtures.
- Real WireGuard in CI without root: the test harness runs a second `wireguard-go` netstack as server peer in the same process, with HTTP and DNS servers listening on its tunnel address.
- Fixtures listen on loopback and tunnel-private addresses, so tests inject a guard allowlist for them (SPEC 9). One dedicated test runs with the production guard.
- Time-dependent behaviour uses the injected clock, never `time.Sleep`.
- Every bug gets a regression test at the lowest layer that reproduces it.

## Layers

| Layer | Scope | When | Version |
|---|---|---|---|
| L1 Unit | Rules of SPEC 3-7 | every push | v0.1 |
| L2 Fuzz | Parsers | push (10s each), nightly (10min each) | v0.1 |
| L3 Integration | Proxy + fixture upstreams + in-process tunnels | every push | v0.1 |
| L4 Player | ffmpeg against the running binary | every push | v0.1 |
| L5 Platform | Cross-compile, container smoke; manifests (Later) | every push | v0.1 / Later |
| L6 Manual | Real players, streams and hardware | before each release | v0.1 |

All Go tests run with `-race` and `go.uber.org/goleak`.

## L1 Unit

Test every rule of SPEC 3.1-3.5, 4, 5 and 6, table-driven, with golden files in `testdata/`. Non-obvious cases:

- HLS rewriter: `URI=` in arbitrary tags, non-http schemes untouched, unknown tags and comments byte for byte, CRLF, BOM, relative URIs after a redirect.
- DASH rewriter: golden manifests of realistic structure (live with `SegmentTimeline` and a relative Period `BaseURL`, VOD with `SegmentList` and on-demand files, nested and multiple `BaseURL`), absolute and escaping references, `UTCTiming`, removed `Location` and `PatchLocation`, `ContentProtection` refused, every rejection rule of spec 3.6; unknown elements and namespaces byte for byte; parser differentials (comments and processing instructions that other parsers end earlier, CDATA, `>` in attribute values), removed event streams and descriptors, rewritten font URLs, output limit (also for HLS).
- Prefix-signed URLs (spec 4.1): format and domain separation from `/r/`, non-canonical prefixes rejected, escapes (`..`, `%2e%2e`, `%2F`, `//`, `\`, `..;`, double encoding, segment names of dots and spaces, overlong UTF-8, query without mode `q`, other host, scheme, port, channel, key or signature) give `404`; redirect targets below the prefix only.
- Sniffing (spec 3.1, 3.3): a root element after a long comment or a doctype with internal subset, no root within 64 KiB and UTF-16/32 bodies are detected or refused, never streamed.
- Channel list: extra/attrs ordering, `"` replacement, commas and Greek characters in names, empty values omitted, `.m3u8` suffix decision.
- Config: route resolution incl. fail-closed error, three-layer merge with case-insensitive header names, reserved attrs, forbidden directives, quoted dotted `extra` keys, duplicate `PrivateKey`, slug table (Greek names, truncation trim, collisions).
- Signing: tampered signature, URL or channel, wrong key, short key rejected.
- Guard: each blocked range incl. IPv4-mapped IPv6, per-route allowlist.
- Clips: the timestamp and continuity counter rewrite of the embedded segments (copies 0 to 63 concatenated parse as one continuous stream: PTS, DTS and PCR without gap or wrap, counters continuous per PID), only header fields change; malformed packets are errors.
- EPG: XMLTV fixture with made-up channels (Greek display names), plain and gzip; `epg ids` against an `httptest` EPG server (guard allowlist for loopback only in tests).
- EPG proxy mode (spec 6.1): filter (wanted ids, first channel element, ended programmes across time zones, `stop` forms, tokens written again: dropped comments, PIs, doctype, prefixed and invalid names, repeated attributes, CDATA, escaping, other encodings), every rejection rule, errors without input, size limits of input, output and tokens; merge (priority, `epg_source`, missing ids, no mixing, sources without copy); guide with a manual clock (first fetch, refresh, unchanged guide keeps its `ETag`, failed source keeps its last good copy while the other updates, retry interval, startup without a guide, reload of ids and sources, shorter refresh, fetch timeout, shutdown during a fetch); `epg ids` with `--source`, also through an in-process tunnel.
- Probe: fixture upstreams per format (HLS master and media, raw, ICY, refused PLS, M3U and DASH, 404, 500, redirect, oversized playlist, rejected playlist), guard refusal, header timeout with a manual clock, timing fields present, no query or path in the output; the command against an in-process tunnel (tunnel start in the timing, unused tunnel files not loaded, tunnel down), exit codes 0, 1 and 2. `POST /debug/probe` against a running server with an in-process tunnel (tunnel starts once, second probe finds it up, headers, guard, unknown route, wrong method, oversized body, host name and cross-origin rejected, no metric or recent request, no URL path or query in the log); the command with a debug listener (fake and real `serve`), without one, with nothing listening, `--standalone`, and error answers that are not worked around.

## L2 Fuzz

Targets: HLS rewriter, DASH rewriter (`internal/dash`), XMLTV channel parser, XMLTV filter (`FuzzFilter`), wg-quick parser, `/r/` URL decoding, `/p/` prefix path validation (`FuzzParsePrefix`), ICY status line wrapper, playlist format sniffer (spec 3.3), clip segment rewrite (`FuzzShift`).

Invariants:
- No panic.
- Rewriter output parses again.
- Every URI line and every `URI="http..."` attribute in the output points to `/r/`; no other line changes.
- DASH rewriter: the output passes the rewriter's checks again and rewrites again; every `BaseURL` and every segment reference that is not plain relative points to a prefix URL; no `Location`, `PatchLocation` or chaining property is left; `UTCTiming` is `direct` or the proxy's.
- Prefix path validation: an accepted request reaches an upstream URL below the signed prefix (same scheme, host and port; after decoding no empty, `.` or `..` segment) and carries a query only with mode `q`.
- ICY wrapper: a leading `ICY ` becomes `HTTP/1.0 `, every other input passes byte for byte, for any split into reads; on a connection with two responses the second is checked like the first after a plain HTTP response, and no request follows an ICY response.
- Clip segment rewrite: the input is untouched and keeps its length; shifting the output back by the inverse amounts gives the input byte for byte.
- XMLTV filter: the guide parses strictly, has only the root `tv` and `<channel>` and `<programme>` elements of the wanted ids, plain names, no comments, processing instructions, declarations or CDATA, no ended programme, and filters to itself again.

## L3 Integration

Proxy in-process with a test config dir, fixture upstreams as `httptest.Server`s, tunnel harness from the principles.

- HLS: master with variants, demuxed audio and subtitle renditions, AES-128 key, fMP4 init segment, sliding live window advancing over time, VOD with `ENDLIST`, gzip-compressed playlist.
- DASH: a redirected live manifest with a relative Period `BaseURL` and `$RepresentationID$`/`$Time$` templates, found by `.mpd`, by `type = "dash"` and by the XML root on an `auto` channel; the segment URLs a player builds reach the upstream with the channel's headers; an absolute template with a query; `UTCTiming` and `/time`; `ContentProtection` (`drm`) and remote elements refused without leaking URLs.
- HTTP edge cases: `HEAD` on `/c` and `/r`, `Range` passthrough on segments, `Range` not forwarded for `.m3u8`, `Content-Length` absent after decompression, `ICY 200 OK` raw stream (also on a kept-alive connection after plain HTTP answers of the same host), `/c/` polls of `auto` and `raw` channels reuse one upstream connection, `Icy-MetaData` forwarding, 4xx and 5xx mapping, timeouts, 10 redirects ok and 11 fail, playlists the proxy does not rewrite refused with `502` on `auto` and `raw` channels and `/r/` while media (MPEG-TS, fMP4, MP3, AAC, Ogg) streams.
- Routing and leaks:
  - Tunnel channel requests arrive at the server peer with the tunnel source address.
  - A recording direct dialer sees zero dials for tunnel channels, including segments, keys and redirects.
  - `direct` channels never touch a tunnel.
  - Two tunnels with overlapping address ranges at the same time.
  - Upstream DNS answered by the tunnel's DNS server, not the host resolver.
- Guard with the production settings: redirect and DNS rebinding to a private IP are blocked.
- Tunnel lifecycle: concurrent first requests share one start, idle close, no handshake gives `502`, recovery, drain on shutdown.
- Sharing: 5 channels on one tunnel use one WireGuard device; repeated segments to one host reuse pooled connections.
- Reload: valid change applied, invalid change rejected while the old config keeps serving, removed tunnel drained, changed tunnel recreated, unchanged tunnel keeps running, reload during a tunnel start.
- Error clips (spec 3.4): `GET /c/` of an HLS channel answers every failure class with its clip playlist (upstream `5xx` and `4xx`, timeout, refused connection, guard, rejected, refused and oversized playlists, a second `206`; through a tunnel: no handshake within `connect_timeout`, a name outside `AllowedIPs`); `HEAD`, `/r/`, raw channels and `error = false` keep the HTTP errors; `/clip/` serves the rewritten segments, `404` for anything else, and never asks an upstream; logs and metrics as in spec 7.3 and 8.
- Security: unsigned or tampered `/r/` and disabled channel give `404`, logo path traversal rejected; `/p/` escapes through the server reach no upstream, nor do upstream redirects out of the prefix; a prefix URL follows the channel's route after a reload that moves the channel to another tunnel, and dies with the channel.
- Capacity: 3 simulated viewers on different channels; 30s on push, 5 minutes nightly; no stalls, no goroutine or memory growth.
- EPG proxy mode: two direct sources (gzip and plain) filtered and merged by priority and `epg_source`, served plain and gzip with `ETag`, conditional and `HEAD` requests, linked from the channel list; `503` before the first fetch, a failing source logged; a reload that adds a channel fetches again; back to link mode gives `404`; a source through an in-process tunnel never dials direct, a source on a private address is refused by the guard (`result="guard"`).
- Observability (v0.2):
  - Timing histograms (`wsp_tunnel_start_seconds`, `wsp_upstream_phase_seconds`): the phases of direct and tunnel requests are observed and served on `/metrics`; the Grafana dashboard only uses metrics the registry has.
  - Debug log: one `request` line per `/c/`, `/r/` and `/p/` request; logs never contain upstream paths.
  - Debug endpoints: off by default, on their own listener (never on the main one), recent requests, profiles, tunnel state and cool-down (`POST /debug/tunnels/{name}/stop`); requests addressed by a host name are rejected (DNS rebinding). The `tunnel list` and `tunnel stop` commands run against a fake debug server.

## L4 Player

- Media fixtures generated once with ffmpeg into `testdata/media/` (a few seconds each): TS, fMP4, AES-128, demuxed audio, DASH (`SegmentTemplate` with `$Time$` and `SegmentTimeline` below a relative Period `BaseURL`); served by a fixture server simulating a live window for HLS and a redirect to a CDN path.
- IPTV Simple Client plays HLS via Kodi's built-in ffmpeg by default, so ffmpeg stands in for Kodi: `ffmpeg -i http://127.0.0.1:PORT/c/{id}.m3u8 -t 10 -f null -` must exit 0 and decode frames, for every fixture, through a tunnel.
- Raw: `ffmpeg -i http://127.0.0.1:PORT/c/{radio-id} -t 5 -f null -` against an Icecast-like and an `ICY` fixture.
- DASH: `ffmpeg -i http://127.0.0.1:PORT/c/{id}.mpd -t 10 -f null -` through a tunnel, the channel URL redirected to the fixture's directory; ffmpeg must fetch the init segments and media segments of both representations through `/p/` URLs.
- Error clip: an HLS channel whose upstream playlist is missing plays the error clip through the proxy: ffmpeg decodes 4.8 s (120 frames) without errors and exits 0.
- VLC is covered manually in L6 (`cvlc` exits 0 even when playback fails).
- Code: `internal/server/playertest` (fixture server and `TestPlayer`); fixtures and their generator: `testdata/media/generate.sh`. The player tests run only with `WSP_PLAYER_TESTS=1` and ffmpeg in `PATH`, so `go test ./...` passes without ffmpeg; CI runs them in the `Player` job ([CONTRIBUTING.md](../CONTRIBUTING.md#checks)).

## L5 Platform

- v0.1: cross-compile `linux/amd64`, `linux/arm64`, `darwin/arm64` on every push; container smoke test (build image, start with `examples/config`, wait for `healthcheck`, fetch `/playlist.m3u8`); `docker compose config` for Compose.
- v0.1, nightly: unit and integration tests on a native arm64 CI runner (QEMU only as fallback, without `-race`).
- Later (with distribution): `kubeconform` for Kustomize output, `systemd-analyze verify` for the unit.

## L6 Manual release checklist

- Hardware: Raspberry Pi 5 (64-bit OS), Apple Silicon, amd64 PC. No Raspberry Pi 4 is available for testing.
- Players: Kodi IPTV Simple Client with default settings, Kodi with timeshift enabled (inputstream.ffmpegdirect), VLC.
- Streams from the real channel list: at least one per tunnel, one `direct`, one Icecast radio, one Shoutcast `/;` radio. EPG and logos visible, parental lock (`parent-code`) works, channel switching within the idle timeout.
- Load: 3 concurrent HD streams through one tunnel on the Raspberry Pi 5 for 30 minutes; record CPU, memory and throughput.

## CI and coverage

- Every push ([`ci.yml`](../.github/workflows/ci.yml)): `golangci-lint` (incl. `gofumpt`, `errcheck`, `staticcheck`), `govulncheck`, L1, L2 short (`Fuzz`: every target 10 s with `-fuzzminimizetime 2s`, a failing input is attached to the run as artifact `fuzz-failure`), L3 including the 30 s capacity test (`Capacity`, `WSP_CAPACITY=30s` with `-race`, a job of its own next to `Player`), L4, L5.
- Nightly ([`nightly.yml`](../.github/workflows/nightly.yml), not a required check): L2 long, capacity 5 minutes, native arm64 run.
  - L2 long: every fuzz target (found with `go test -list '^Fuzz' ./...`) for 10 minutes with `-fuzzminimizetime 2s` (the default 60s stalls `FuzzRewrite`); a failing input is attached to the run as an artifact.
  - Capacity: `TestCapacity` in `internal/server/capacitytest` with `-race`. `WSP_CAPACITY` sets the duration (`5m` nightly; `30s` or `1` for the 30s run on push); unset, the test skips, so plain `go test ./...` does not run it. Bounds: every segment arrives, in order and unaltered, within 3 target durations of being published; goroutines grow by at most 20 and the heap in use (after GC) by at most 32 MiB over the first sample after the warm-up; `goleak` after shutdown.
  - arm64: `go test -race ./...` on `ubuntu-26.04-arm`; no QEMU fallback, the job fails if the runner is not arm64.
- Coverage target 80% for `internal/hls`, `internal/dash`, `internal/config`, `internal/route`, `internal/server`; none for `cmd/`.
