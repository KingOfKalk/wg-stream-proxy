# wg-stream-proxy - Observability

Companion to [spec.md](spec.md) (7.3 metrics, 7.1 debug endpoints, 8 log) and [deploy.md](deploy.md). It explains what the proxy exposes, what Prometheus should collect, and how to read the Grafana dashboard [`deploy/grafana/wg-stream-proxy.json`](../deploy/grafana/wg-stream-proxy.json).

## What is exposed where

| Where | What | Use |
|---|---|---|
| `GET /metrics` on the main listener (`listen`, default `:2349`) | The `wsp_` metrics ([spec 7.3](spec.md)) plus the standard `go_*` and `process_*` metrics | Trends over days: Prometheus and Grafana (this document) |
| Log, `log_level = "debug"` | One `request` line per `/c/`, `/r/`, `/p/` and `/clip/` request with its timings ([spec 8](spec.md)); tunnel start, failure and close lines carry durations at every level; a failure answered with an error clip has `clip=<name>` on its `warn` line (every level) | Why one request was slow or failed |
| Debug endpoints on `debug.listen` (off by default) | Profiles, tunnel state, cool-down ([spec 7.1](spec.md), [deploy.md](deploy.md#debug-endpoints)) | Looking at the running process now |

`/metrics` has no authentication and sits on the player-facing port: keep the proxy on the LAN ([spec 8](spec.md)). Metrics hold channel ids and tunnel names from your config, never URLs or keys.

The metrics in short (definitions in [spec 7.3](spec.md)):

| Metric | Type | Labels | Answers |
|---|---|---|---|
| `wsp_tunnel_up` | gauge | `tunnel` | Does the tunnel run with a handshake younger than 180 s? |
| `wsp_tunnel_last_handshake_timestamp_seconds` | gauge | `tunnel` | When was the latest handshake? `0` while not running |
| `wsp_tunnel_start_seconds` | histogram | `tunnel` | How long does a cold start take? How often do tunnels start? |
| `wsp_upstream_phase_seconds` | histogram | `route`, `phase` | Where does the time go: `dns`, `connect`, `tls`, `first_byte` |
| `wsp_active_requests` | gauge | `channel`, `kind` | What is being watched right now? |
| `wsp_upstream_requests_total` | counter | `channel`, `kind`, `code` | Upstream status codes (`error`: no response) |
| `wsp_upstream_errors_total` | counter | `channel`, `reason` | Why upstream requests fail |
| `wsp_error_clips_total` | counter | `clip` | How often viewers saw an error clip instead of an HTTP error, and which one ([spec 3.4](spec.md)) |
| `wsp_epg_fetches_total` | counter | `source`, `result` | EPG proxy mode ([spec 6.1](spec.md)): fetches of each source, `ok` or why they failed |
| `wsp_epg_last_success_timestamp_seconds` | gauge | `source` | When was the EPG copy a source serves fetched? `0` without one |

## Prometheus

Scrape config (`prometheus.yml`); `<pi>` is the host of the proxy, `2349` its `listen` port:

```yaml
global:
  scrape_interval: 15s

scrape_configs:
  - job_name: wg-stream-proxy
    metrics_path: /metrics        # the default
    static_configs:
      - targets: ["<pi>:2349"]
```

The dashboard expects the job name `wg-stream-proxy` (its `instance` variable lists the instances of that job); with another name, edit the variable query.

### Scrape interval

- Use `15s` as the default and `30s` if Prometheus runs on the Raspberry Pi itself (fewer SD card writes). Do not go below `10s`: the proxy gains nothing, and storage and load grow.
- A longer interval loses no events: counters and histograms accumulate, and `rate()` and `increase()` still count every request and observation. Only the time resolution gets coarser.
- Rate windows need at least 4 times the scrape interval (`rate(x[1m])` at `15s`) so that they hold enough samples. In Grafana, `$__rate_interval` picks such a window from the data source's scrape interval; the dashboard uses it everywhere. Set the data source's "Scrape interval" to the value in `prometheus.yml`.
- Short-lived gauges can be missed between two scrapes: a `wsp_active_requests` series of kind `segment` lives for a fraction of a second, so the gauge shows only the segments in flight at the scrape moment. For how much was fetched, read `wsp_upstream_requests_total`, not the gauge. Open raw streams and playlists in flight last long enough to show up.

### Series and storage

One scrape of a proxy with 6 channels (all `direct`), one configured tunnel and a few thousand requests returned 119 series: 75 `wsp_`, 35 `go_` and 9 `process_`. Each timing histogram series is 14 series (11 buckets, `+Inf`, sum, count), so the histograms dominate. At `15s` that is 5,760 samples per series and day, about 0.7 million samples or roughly 1 to 1.5 MB per day for 119 series (Prometheus needs [about 1 to 2 bytes per sample](https://prometheus.io/docs/prometheus/latest/storage/)); at `30s` half of it. Each further channel adds a handful of series (its `kind`, `code` and `reason` combinations), each further tunnel 16.

- Retention: Prometheus keeps 15 days by default. For trends over months, start it with `--storage.tsdb.retention.time=180d`; the storage above stays small.
- Cardinality is bounded by your config: `channel` by the channel ids, `tunnel` and `route` by the tunnel names plus `direct`, `source` by the `[[epg.source]]` names (14 series each); `kind`, `phase`, `reason`, `code`, `clip` and `result` have a few fixed values.
- Counters and histograms start from zero when the proxy restarts; `rate()` and `increase()` handle the reset. Gauges are recomputed. Tunnels are idle after a restart until the first request, so `wsp_tunnel_up` is `0` then.
- Tunnels start on demand and stop after `tunnels.idle_timeout`, so `wsp_tunnel_up == 0` is normal while nothing plays.

## Useful PromQL

Timing. The histograms have buckets from 10 ms to 15 s: values above 15 s, or tails inside a bucket, show only as the bucket bound, so read quantiles as estimates. A request that streams (a radio channel, a TS stream) is recorded when it closes, so its phases show up after the stream ends. A phase that did not happen (pooled connection, IP literal, plain HTTP) is not observed, so `connect` has fewer samples than `first_byte`.

```promql
# Median and 95th percentile of the first upstream byte per route, over 1 hour
histogram_quantile(0.5, sum by (le, route) (rate(wsp_upstream_phase_seconds_bucket{phase="first_byte"}[1h])))
histogram_quantile(0.95, sum by (le, route) (rate(wsp_upstream_phase_seconds_bucket{phase="first_byte"}[1h])))

# 95th percentile per route and phase
histogram_quantile(0.95, sum by (le, route, phase) (rate(wsp_upstream_phase_seconds_bucket[1h])))

# 95th percentile of tunnel starts per tunnel (starts are rare: use a long window)
histogram_quantile(0.95, sum by (le, tunnel) (rate(wsp_tunnel_start_seconds_bucket[6h])))
```

Tunnel starts: a tunnel starts on the first request after it was idle, so the start rate shows how often tunnels cold-start.

```promql
# Tunnel starts per hour
sum by (tunnel) (increase(wsp_tunnel_start_seconds_count[1h]))

# Average tunnel start time over 6 hours
sum by (tunnel) (rate(wsp_tunnel_start_seconds_sum[6h])) / sum by (tunnel) (rate(wsp_tunnel_start_seconds_count[6h]))

# Age of the latest handshake of running tunnels, in seconds
(time() - wsp_tunnel_last_handshake_timestamp_seconds) and (wsp_tunnel_last_handshake_timestamp_seconds > 0)
```

Requests and errors:

```promql
# Failed upstream requests (no response or 5xx) as a share of all, per channel
sum by (channel) (rate(wsp_upstream_requests_total{code=~"error|5.."}[10m]))
  / sum by (channel) (rate(wsp_upstream_requests_total[10m]))

# Errors per second by reason
sum by (reason) (rate(wsp_upstream_errors_total[10m]))

# Error clips shown to viewers per hour, by clip
sum by (clip) (increase(wsp_error_clips_total[1h]))

# Upstream requests per second by status code
sum by (code) (rate(wsp_upstream_requests_total[5m]))

# Viewers right now: open raw streams and playlist or segment requests in flight
sum by (channel, kind) (wsp_active_requests)
```

EPG proxy mode ([spec 6.1](spec.md)): each source is fetched every `[epg] refresh` (default 6 h), so these change rarely.

```promql
# Age of the EPG copy each source serves, in seconds (grows past refresh while fetches fail)
(time() - wsp_epg_last_success_timestamp_seconds) and (wsp_epg_last_success_timestamp_seconds > 0)

# Failed EPG fetches per day, by source and result
sum by (source, result) (increase(wsp_epg_fetches_total{result!="ok"}[1d]))
```

## Example alerting rules

Examples to adapt, not defaults; thresholds depend on your streams. Load them through `rule_files` in `prometheus.yml`.

```yaml
groups:
  - name: wg-stream-proxy
    rules:
      - alert: WgStreamProxyDown
        expr: up{job="wg-stream-proxy"} == 0
        for: 2m
        annotations:
          summary: "wg-stream-proxy {{ $labels.instance }} is not scraped"

      # The tunnel device runs but its handshake is older than 180 s
      # (wsp_tunnel_up == 0, timestamp != 0) while connection attempts
      # through it keep coming.
      - alert: WgStreamProxyTunnelDown
        expr: |
          (wsp_tunnel_up == 0 and wsp_tunnel_last_handshake_timestamp_seconds > 0)
          and on (instance, tunnel)
          label_replace(
            sum by (instance, route) (rate(wsp_upstream_phase_seconds_count{phase="connect", route!="direct"}[5m])) > 0,
            "tunnel", "$1", "route", "(.*)")
        for: 3m
        annotations:
          summary: "Tunnel {{ $labels.tunnel }} is requested but has no fresh handshake"

      - alert: WgStreamProxyUpstreamErrors
        expr: |
          sum by (instance, channel) (rate(wsp_upstream_requests_total{code=~"error|5.."}[10m]))
            / sum by (instance, channel) (rate(wsp_upstream_requests_total[10m])) > 0.2
          and sum by (instance, channel) (rate(wsp_upstream_requests_total[10m])) > 0.05
        for: 10m
        annotations:
          summary: "More than 20% of the upstream requests of {{ $labels.channel }} fail"

      - alert: WgStreamProxySlowFirstByte
        expr: |
          histogram_quantile(0.95,
            sum by (le, instance, route) (rate(wsp_upstream_phase_seconds_bucket{phase="first_byte"}[10m]))) > 5
        for: 10m
        annotations:
          summary: "p95 first byte on route {{ $labels.route }} is above 5 s"

      # The EPG copy of a source is older than twice the default refresh:
      # its fetches fail and the guide serves the last good copy.
      - alert: WgStreamProxyEPGStale
        expr: |
          (time() - wsp_epg_last_success_timestamp_seconds > 12 * 3600)
          and (wsp_epg_last_success_timestamp_seconds > 0)
        for: 30m
        annotations:
          summary: "EPG source {{ $labels.source }} has not been fetched for more than 12 h"
```

## Grafana dashboard

[`deploy/grafana/wg-stream-proxy.json`](../deploy/grafana/wg-stream-proxy.json) is a dashboard (Grafana 13, `schemaVersion` 42) built only on the metrics above. It has no data source UID of a real instance: the Prometheus data source is an import input (`${DS_PROMETHEUS}`).

Import in the UI: Dashboards, New, Import, upload the JSON file, pick your Prometheus data source. Set the data source's "Scrape interval" to the one in `prometheus.yml` (see above).

Provisioning (files, no UI): provisioning has no import inputs, so replace the placeholder by your data source's UID and let Grafana read the folder.

```sh
# the Prometheus data source provisioned with "uid: prometheus"
mkdir -p /var/lib/grafana/dashboards
sed 's/\${DS_PROMETHEUS}/prometheus/g' wg-stream-proxy.json > /var/lib/grafana/dashboards/wg-stream-proxy.json
```

```yaml
# /etc/grafana/provisioning/datasources/prometheus.yml
apiVersion: 1
datasources:
  - name: Prometheus
    uid: prometheus
    type: prometheus
    access: proxy
    url: http://prometheus:9090
    jsonData:
      timeInterval: 15s   # = scrape_interval

# /etc/grafana/provisioning/dashboards/wg-stream-proxy.yml
apiVersion: 1
providers:
  - name: wg-stream-proxy
    type: file
    options:
      path: /var/lib/grafana/dashboards
```

Variables: `instance` (single select, no "All"; the instances of job `wg-stream-proxy`), and with "All" `route` (`direct` and tunnel names), `tunnel`, `channel` (channel ids seen in the requests). Panels that have no data for a selection stay empty: for example, the tunnel panels while tunnels are idle.

| Row | Panels | Answers |
|---|---|---|
| Overview | Tunnels up, last handshake, active requests, upstream request rate, upstream error rate | Is it alive and busy right now? |
| Tunnels | Up/down timeline, handshake age, tunnel start p50 / p95, tunnel starts | Do the tunnels connect, how fast, how often do they cold-start? |
| Upstream timing | p50 and p95 per route and phase, heatmap of the first byte | Is a route slow, and in DNS, connect, TLS or the upstream's first byte? Tunnel or provider? |
| Channels | Active requests by channel and kind, requests by status code, errors by reason and error clips, failed share by channel | Which channel is watched, which one fails and why, and what did the viewer see? |
| Process | Resident memory and heap, CPU, goroutines, GC pause time | Does the proxy fit the Raspberry Pi: growing memory or goroutines, CPU peaks? |
| EPG | Age of the EPG copy per source | Does each `[[epg.source]]` still update, or does the guide serve an old copy? Empty in link mode |

Reading hints:

- The tunnel start p50 / p95 and starts panels have data only after a tunnel started; their lines connect the rare windows with a start.
- First byte of a channel on a tunnel includes the round trip through the VPN; compare `direct` and tunnel routes to separate the provider's origin from the tunnel.
- Tunnel start waits are not in `wsp_upstream_phase_seconds` (spec 7.3); they are `wsp_tunnel_start_seconds`.
- Error clips (`clip ...` series in "Errors by reason and error clips") count what viewers saw, for all channels (the metric has no `channel` label): `channel-not-available`, `tunnel-unreachable`, `channel-blocked`. The failure behind each is also in the `reason` series or, for an upstream status, in "Upstream requests by code".
- The dashboard is kept in sync with the metrics by a test (`internal/metrics`): every `wsp_`, `go_` and `process_` name in a query must exist.
