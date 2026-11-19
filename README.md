# wg-stream-proxy
Route individual IPTV streams through userspace WireGuard tunnels and serve them as one M3U8 playlist for Kodi, VLC and others.

How it works: [docs/architecture.md](docs/architecture.md).

Metrics, Prometheus and the Grafana dashboard: [docs/observability.md](docs/observability.md).

## Third-party material

- The built-in clip segments in [`internal/clips/segments`](internal/clips/segments) show text set in [Inter](https://rsms.me/inter/) 4.1 by Rasmus Andersson and The Inter Project Authors, under the [SIL Open Font License 1.1](https://openfontlicense.org/). Only the rendered pixels ship, not the font; the font is fetched by [`generate.sh`](internal/clips/generate.sh) when the clips are rebuilt.
