# wg-stream-proxy - Testing strategy

Companion to [spec.md](spec.md). The CI jobs that run these layers, and how
to run them locally, are in [CONTRIBUTING.md](../CONTRIBUTING.md#checks).

## Principles

- TDD: every rule of the spec and every bug gets a test at the lowest layer that shows it; table-driven, golden files in `testdata/`.
- No real broadcaster URLs in tests or CI: they are geo-dependent, flaky and change. All upstreams are local fixtures.
- Real WireGuard without root: the harness (`internal/tunnel/tunneltest`) runs a second wireguard-go netstack as server peer in the same process, with HTTP and DNS servers on its tunnel address.
- Fixtures listen on loopback and tunnel-private addresses, so tests inject a guard allowlist (spec 9); one test runs with the production guard.
- Fail closed is tested directly: a recording direct dialer sees no dial for tunnel channels (segments, keys and redirects included).
- Time-dependent behaviour uses the injected clock, never `time.Sleep`.
- All Go tests run with `-race` and `goleak`. Coverage target 80% for `internal/hls`, `internal/dash`, `internal/config`, `internal/route` and `internal/server`.

## Layers

| Layer | Scope | When |
|---|---|---|
| L1 Unit | The rules of spec 3-7 per package | every push |
| L2 Fuzz | Parsers and rewriters: no panic, output parses and rewrites again, no URL the player could fetch past the proxy | every push (10 s per target), nightly (10 min per target) |
| L3 Integration | Proxy in-process with fixture upstreams and in-process tunnels; capacity test `internal/server/capacitytest` (3 viewers, runs only with `WSP_CAPACITY` set) | every push (capacity 30 s), nightly (capacity 5 min) |
| L4 Player | ffmpeg stands in for Kodi and plays every fixture (HLS, DASH, raw, ICY, error clip) through a tunnel: `internal/server/playertest`, fixtures from `testdata/media/generate.sh`; runs only with `WSP_PLAYER_TESTS=1` and ffmpeg in `PATH` | every push |
| L5 Platform | Cross-compile, container smoke test, `docker compose config`; native arm64 test run | every push; arm64 nightly |
| L6 Manual | Real players, streams and hardware | before each release |

## L6 Manual release checklist

- Hardware: Raspberry Pi 5 (64-bit OS), Apple Silicon, amd64 PC. No Raspberry Pi 4 is available for testing.
- Players: Kodi IPTV Simple Client with default settings, Kodi with timeshift enabled (inputstream.ffmpegdirect), VLC (`cvlc` exits 0 even when playback fails, so VLC is only tested here).
- Streams from the real channel list: at least one per tunnel, one `direct`, one Icecast radio, one Shoutcast `/;` radio. EPG and logos visible, parental lock (`parent-code`) works, channel switching within the idle timeout.
- Load: 3 concurrent HD streams through one tunnel on the Raspberry Pi 5 for 30 minutes; record CPU, memory and throughput.
