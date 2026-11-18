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
| L5 Platform | Cross-compile, container smoke; manifests from v0.3 | every push | v0.1 / v0.3 |
| L6 Manual | Real players, streams and hardware | before each release | v0.1 |

All Go tests run with `-race` and `go.uber.org/goleak`.

## L1 Unit

Test every rule of SPEC 3.1-3.5, 4, 5 and 6, table-driven, with golden files in `testdata/`. Non-obvious cases:

- HLS rewriter: `URI=` in arbitrary tags, non-http schemes untouched, unknown tags and comments byte for byte, CRLF, BOM, relative URIs after a redirect.
- Channel list: extra/attrs ordering, `"` replacement, commas and Greek characters in names, empty values omitted, `.m3u8` suffix decision.
- Config: route resolution incl. fail-closed error, three-layer merge with case-insensitive header names, reserved attrs, forbidden directives, quoted dotted `extra` keys, duplicate `PrivateKey`, slug table (Greek names, truncation trim, collisions).
- Signing: tampered signature, URL or channel, wrong key, short key rejected.
- Guard: each blocked range incl. IPv4-mapped IPv6, per-route allowlist.
- Importer: the user's real M3U (anonymised) as golden fixture: header conversions, pipe syntax, `extra` mapping, `[defaults.*]` extraction, commented-out entries.
- EPG: XMLTV fixture with made-up channels (Greek display names), plain and gzip; `epg match` ranking table; `epg ids`, `epg match` and `import --match-epg` against an `httptest` EPG server (guard allowlist for loopback only in tests).

## L2 Fuzz

Targets: HLS rewriter, M3U importer, XMLTV channel parser, wg-quick parser, `/r/` URL decoding, ICY status line wrapper.

Invariants:
- No panic.
- Rewriter output parses again.
- Every URI line and every `URI="http..."` attribute in the output points to `/r/`; no other line changes.
- ICY wrapper: a leading `ICY ` becomes `HTTP/1.0 `, every other input passes byte for byte, for any split into reads.

## L3 Integration

Proxy in-process with a test config dir, fixture upstreams as `httptest.Server`s, tunnel harness from the principles.

- HLS: master with variants, demuxed audio and subtitle renditions, AES-128 key, fMP4 init segment, sliding live window advancing over time, VOD with `ENDLIST`, gzip-compressed playlist.
- HTTP edge cases: `HEAD` on `/c` and `/r`, `Range` passthrough on segments, `Range` not forwarded for `.m3u8`, `Content-Length` absent after decompression, `ICY 200 OK` raw stream, `Icy-MetaData` forwarding, 4xx and 5xx mapping, timeouts, 10 redirects ok and 11 fail.
- Routing and leaks:
  - Tunnel channel requests arrive at the server peer with the tunnel source address.
  - A recording direct dialer sees zero dials for tunnel channels, including segments, keys and redirects.
  - `direct` channels never touch a tunnel.
  - Two tunnels with overlapping address ranges at the same time.
  - Upstream DNS answered by the tunnel's DNS server, not the host resolver.
- Guard with the production settings: redirect and DNS rebinding to a private IP are blocked.
- Tunnel lifecycle: concurrent first requests share one start, idle close, no handshake gives `502`, recovery, drain on shutdown.
- Sharing: 5 channels on one tunnel use one WireGuard device; repeated segments to one host reuse pooled connections.
- Reload: valid change applied, invalid change rejected while the old config keeps serving, removed tunnel drained.
- Security: unsigned or tampered `/r/` and disabled channel give `404`, logo path traversal rejected.
- Capacity: 3 simulated viewers on different channels; 30s on push, 5 minutes nightly; no stalls, no goroutine or memory growth.

## L4 Player

- Media fixtures generated once with ffmpeg into `testdata/media/` (a few seconds each): TS, fMP4, AES-128, demuxed audio; served by a fixture server simulating a live window.
- IPTV Simple Client plays HLS via Kodi's built-in ffmpeg by default, so ffmpeg stands in for Kodi: `ffmpeg -i http://127.0.0.1:PORT/c/{id}.m3u8 -t 10 -f null -` must exit 0 and decode frames, for every fixture, through a tunnel.
- Raw: `ffmpeg -i http://127.0.0.1:PORT/c/{radio-id} -t 5 -f null -` against an Icecast-like and an `ICY` fixture.
- VLC is covered manually in L6 (`cvlc` exits 0 even when playback fails).
- Code: `internal/server/playertest` (fixture server and `TestPlayer`); fixtures and their generator: `testdata/media/generate.sh`. The player tests run only with `WSP_PLAYER_TESTS=1` and ffmpeg in `PATH`, so `go test ./...` passes without ffmpeg; CI runs them in the `Player` job ([CONTRIBUTING.md](../CONTRIBUTING.md#checks)).

## L5 Platform

- v0.1: cross-compile `linux/amd64`, `linux/arm64`, `darwin/arm64` on every push; container smoke test (build image, start with `examples/config`, wait for `healthcheck`, fetch `/playlist.m3u8`); `docker compose config` for Compose.
- v0.1, nightly: unit and integration tests on a native arm64 CI runner (QEMU only as fallback, without `-race`).
- v0.3: `kubeconform` for Kustomize output, `systemd-analyze verify` for the unit.

## L6 Manual release checklist

- Hardware: Raspberry Pi 5 (64-bit OS), Apple Silicon, amd64 PC. No Raspberry Pi 4 is available for testing.
- Players: Kodi IPTV Simple Client with default settings, Kodi with timeshift enabled (inputstream.ffmpegdirect), VLC.
- Streams from the real channel list: at least one per tunnel, one `direct`, one Icecast radio, one Shoutcast `/;` radio. EPG and logos visible, parental lock (`parent-code`) works, channel switching within the idle timeout.
- Load: 3 concurrent HD streams through one tunnel on the Raspberry Pi 5 for 30 minutes; record CPU, memory and throughput.

## CI and coverage

- Every push: `golangci-lint` (incl. `gofumpt`, `errcheck`, `staticcheck`), `govulncheck`, L1, L2 short, L3, L4, L5.
- Nightly ([`nightly.yml`](../.github/workflows/nightly.yml), not a required check): L2 long, capacity 5 minutes, native arm64 run.
  - L2 long: every fuzz target (found with `go test -list '^Fuzz' ./...`) for 10 minutes with `-fuzzminimizetime 2s` (the default 60s stalls `FuzzRewrite`); a failing input is attached to the run as an artifact.
  - Capacity: `TestCapacity` in `internal/server/capacitytest` with `-race`. `WSP_CAPACITY` sets the duration (`5m` nightly; `30s` or `1` for the 30s run on push); unset, the test skips, so plain `go test ./...` does not run it. Bounds: every segment arrives, in order and unaltered, within 3 target durations of being published; goroutines grow by at most 20 and the heap in use (after GC) by at most 32 MiB over the first sample after the warm-up; `goleak` after shutdown.
  - arm64: `go test -race ./...` on `ubuntu-26.04-arm`; no QEMU fallback, the job fails if the runner is not arm64.
- Coverage target 80% for `internal/hls`, `internal/config`, `internal/route`, `internal/server`; none for `cmd/`.
