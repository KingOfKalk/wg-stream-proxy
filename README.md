# wg-stream-proxy

Route each IPTV and internet radio channel through its own userspace WireGuard tunnel (or direct) and serve them as one M3U playlist for Kodi, VLC and other players. Proxies HLS (TS, fMP4, AES-128), MPEG-DASH without DRM and raw/ICY audio streams; no root, no TUN device, fails closed.

## Features

- HLS: playlists are rewritten so every playlist, segment and key is fetched through the channel's route; TS, fMP4 and AES-128 (keys are passed through, the player decrypts).
- MPEG-DASH without DRM: manifests are rewritten so every segment, the time source and manifest reloads go through the channel's route; `SegmentTemplate`, `SegmentTimeline`, `SegmentList`, live and VOD. DRM manifests are refused.
- Radio: raw and ICY (Icecast, Shoutcast) audio streams.
- Per-channel routes: each channel uses a named WireGuard tunnel or `direct`; several tunnels run in one process. A channel never silently bypasses its tunnel, and a private-range guard blocks the LAN and the host.
- One M3U channel list (`/playlist.m3u8`) with EPG link, logos and Kodi directives; `import` converts an existing M3U file.
- One XMLTV guide (`/epg.xml.gz`): EPG sources fetched through their routes, filtered to your channels and merged.
- Metrics: Prometheus metrics on `/metrics` including timing histograms, and a Grafana dashboard.
- Error clips: an HLS channel that fails to start shows a short clip with the reason ("Tunnel not reachable", "Channel not available", …) instead of a failure dialog.
- Debug: profiles, recent requests, tunnel state and cool-down on a separate listener (off by default).

## Quick start

- Container image `ghcr.io/kingofkalk/wg-stream-proxy` (`linux/amd64`, `linux/arm64`); pin a version tag from the [releases](https://github.com/KingOfKalk/wg-stream-proxy/releases) (there is no `latest` tag).
- Docker run and Compose: [docs/deploy.md](docs/deploy.md); Compose file: [`deploy/compose/compose.yaml`](deploy/compose/compose.yaml).
- Binary: there are no release binaries yet; `go install github.com/KingOfKalk/wg-stream-proxy/cmd/wg-stream-proxy@<version>` or build from source.
- Config: copy [`examples/config`](examples/config) (it connects nowhere until you add your own tunnel file), run `wg-stream-proxy check --config-dir <dir>`, then `wg-stream-proxy serve --config-dir <dir>`.
- Point the player at `http://<host>:2349/playlist.m3u8`.
- Commands and flags: `wg-stream-proxy help` and `wg-stream-proxy help <command>`.

## Non-goals

- Decryption, DRM licenses or keys (permanent).
- Transcoding, restreaming, web UI, Xtream Codes, multi-user.
- DASH with DRM (`ContentProtection`): refused.

## Security

- For LAN use only: the channel list, `/metrics` and the debug endpoints have no authentication. Keep the debug listener on loopback, and put an authenticating reverse proxy in front before exposing anything to the internet.
- Never commit real WireGuard keys or tunnel files; see [AGENTS.md](AGENTS.md).

## Documentation

- [Specification](docs/spec.md) (binding)
- [Architecture](docs/architecture.md): how it works
- [Deployment](docs/deploy.md): standalone, Docker, Compose, Kodi settings
- [Observability](docs/observability.md): metrics, Prometheus, Grafana dashboard
- [Clips](docs/clips.md): error clips
- [Testing](docs/testing.md)
- [Contributing](CONTRIBUTING.md) and [AGENTS.md](AGENTS.md)

## Third-party material

- The built-in clip segments in [`internal/clips/segments`](internal/clips/segments) show text set in [Inter](https://rsms.me/inter/) 4.1 by Rasmus Andersson and The Inter Project Authors, under the [SIL Open Font License 1.1](https://openfontlicense.org/). Only the rendered pixels ship, not the font; the font is fetched by [`generate.sh`](internal/clips/generate.sh) when the clips are rebuilt.

## License

[AGPL-3.0-or-later](LICENSE)
