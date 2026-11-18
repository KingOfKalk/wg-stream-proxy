# wg-stream-proxy - Architecture

How wg-stream-proxy works, in diagrams first and short text second. This
document explains [spec.md](spec.md); it defines nothing new. If the two
ever disagree, the spec wins. Section numbers like "spec 3.2" refer to it.

Related: [spec.md](spec.md), [deploy.md](deploy.md), [testing.md](testing.md)

## Contents

1. [The problem](#1-the-problem)
2. [System context](#2-system-context)
3. [Control plane and data plane](#3-control-plane-and-data-plane)
4. [Request flows](#4-request-flows)
5. [Routing and the guard](#5-routing-and-the-guard)
6. [Tunnel lifecycle](#6-tunnel-lifecycle)
7. [Internals](#7-internals)
8. [Package map](#8-package-map)
9. [Versions](#9-versions)
10. [Deployment](#10-deployment)
11. [Glossary](#11-glossary)

Colours used in all diagrams:

```mermaid
flowchart LR
    lan["Home LAN"]:::lan
    proxy["wg-stream-proxy"]:::proxy
    tunnel["Tunnel"]:::tunnel
    direct["direct"]:::direct
    inet["Internet"]:::inet
    guard{{"Guard"}}:::guard
    later["Later version"]:::later

    classDef lan fill:#3b7fb0,stroke:#24577d,color:#ffffff
    classDef proxy fill:#6f5cc2,stroke:#46398a,color:#ffffff
    classDef tunnel fill:#2a8a68,stroke:#1a5e46,color:#ffffff
    classDef direct fill:#a86a1a,stroke:#6e4511,color:#ffffff
    classDef inet fill:#66727e,stroke:#3f4851,color:#ffffff
    classDef guard fill:#b94845,stroke:#7a2c2a,color:#ffffff
    classDef later fill:#c3cad2,stroke:#5b6670,color:#1b1f24,stroke-dasharray:5 3
```

## 1. The problem

- Some TV and radio streams only play from a certain country. A VPN
  solves that, but a system-wide VPN moves *all* traffic of the host, and
  a household often needs several countries at once.
- wg-stream-proxy picks the exit **per channel**: each channel goes through
  its own WireGuard tunnel (for example `gr` for Greek TV), or explicitly
  `direct` over the normal internet connection.
- The tunnels run **in userspace inside the proxy process**: no TUN
  device, no root, no kernel module. Nothing else on the host is affected.
- Players (Kodi IPTV Simple Client, VLC) only see **one M3U8 channel
  list** on the LAN. Every stream, playlist, segment and key is fetched
  through the proxy, so it always takes the channel's route.
- **Fail closed**: a channel never silently bypasses its tunnel. A broken
  tunnel means an error, never a direct connection.
- Scale: a household, 3 concurrent viewers (spec 1.1).

## 2. System context

```mermaid
flowchart LR
    subgraph lan["Home LAN"]
        direction TB
        kodi["Kodi<br/>IPTV Simple Client"]:::lan
        vlc["VLC"]:::lan
    end
    subgraph proxy["wg-stream-proxy (one process)"]
        direction TB
        srv["HTTP server<br/>playlist, /c, /r"]:::proxy
        routes["Routes<br/>one per tunnel + direct"]:::proxy
        guard{{"Private-range<br/>guard"}}:::guard
    end
    subgraph nets["Tunnels and direct"]
        direction TB
        gr["Tunnel gr<br/>wireguard-go + netstack"]:::tunnel
        uk["Tunnel uk<br/>wireguard-go + netstack"]:::tunnel
        direct["direct<br/>host network"]:::direct
    end
    subgraph inet["Internet"]
        direction TB
        vpn["VPN servers<br/>WireGuard peers"]:::inet
        cdn["Broadcaster CDNs"]:::inet
        epg["EPG<br/>XMLTV file"]:::inet
    end

    kodi & vlc -- "HTTP" --> srv
    srv --> routes --> guard
    guard -- "dial checked IP" --> gr & uk & direct
    gr & uk == "UDP, encrypted" ==> vpn
    vpn --> cdn
    direct --> cdn
    kodi -. "EPG link mode" .-> epg

    classDef lan fill:#3b7fb0,stroke:#24577d,color:#ffffff
    classDef proxy fill:#6f5cc2,stroke:#46398a,color:#ffffff
    classDef tunnel fill:#2a8a68,stroke:#1a5e46,color:#ffffff
    classDef direct fill:#a86a1a,stroke:#6e4511,color:#ffffff
    classDef inet fill:#66727e,stroke:#3f4851,color:#ffffff
    classDef guard fill:#b94845,stroke:#7a2c2a,color:#ffffff
    style lan fill:none,stroke:#3b7fb0,stroke-width:2px
    style proxy fill:none,stroke:#6f5cc2,stroke-width:2px
    style nets fill:none,stroke:#2a8a68,stroke-width:2px
    style inet fill:none,stroke:#66727e,stroke-width:2px
```

- **Players** talk plain HTTP to the proxy on the LAN (default port
  `2349`). They never contact a broadcaster themselves.
- **Routes**: one per tunnel file plus `direct`. Each has its own HTTP
  client with connection pooling (spec 2.1). A channel's route comes from
  its config (section 5).
- **Guard**: sits in front of *every* upstream dial on every route. It
  checks the resolved IP, so redirects and DNS tricks cannot reach the LAN
  or the host (spec 3.5).
- **Tunnels**: one `wireguard-go` device with a gVisor netstack per tunnel
  file, shared by all channels and viewers on it: one key, one UDP
  session. Host names are resolved inside the tunnel with the tunnel's
  `DNS` servers.
- **direct**: the host network, same HTTP settings, same guard. Used only
  where a channel says `tunnel = "direct"`.
- **EPG**: in v0.1 the channel list only *links* the EPG URL
  (`x-tvg-url`); the player downloads it itself (spec 6). Only the CLI
  (`epg ids`, `epg match`, `import --match-epg`) downloads it, over `direct`
  (spec 7.2).

## 3. Control plane and data plane

```mermaid
flowchart TB
    subgraph cp["Control plane"]
        direction LR
        files[("Config dir<br/>config.toml, channels.d,<br/>tunnels")]:::ctl
        hup(["SIGHUP"]):::ctl
        load["Load and validate<br/>all files"]:::ctl
        active["Active config<br/>channels, routes"]:::ctl
        life["Tunnel lifecycle"]:::ctl
        key["Signing key"]:::ctl
        obs["healthz, metrics"]:::ctl
        files --> load
        hup --> load
        load -- "valid: swap in" --> active
        active --> life
    end
    subgraph dp["Data plane"]
        direction LR
        pl["/playlist.m3u8"]:::data
        ch["/c/{id}"]:::data
        res["/r/{channel}/{sig}/..."]:::data
        rt{{"Routes + guard"}}:::guard
        out["HLS rewrite<br/>or io.Copy pipe"]:::data
        ch & res --> rt --> out
    end

    active -- "channel lookups" --> dp
    key -- "sign, verify /r/ URLs" --> dp
    life -- "start, idle close, drain" --> rt
    rt -. "counters, tunnel state" .-> obs

    classDef ctl fill:#a86a1a,stroke:#6e4511,color:#ffffff
    classDef data fill:#3b7fb0,stroke:#24577d,color:#ffffff
    classDef guard fill:#b94845,stroke:#7a2c2a,color:#ffffff
    style cp fill:none,stroke:#a86a1a,stroke-width:2px
    style dp fill:none,stroke:#3b7fb0,stroke-width:2px
```

**Control plane**: decides *what* exists and *how* traffic may flow.

- Config dir (spec 5.1): `config.toml`, `channels.d/*.toml` (with the
  global defaults `channels.d/_defaults.toml`), tunnel files
  in `tunnels/*.conf` (`wg-quick` format, converted to a WireGuard UAPI
  config internally).
- Load and validate: everything is checked at once; all errors are
  reported with file and line. At startup an invalid config exits with
  code `1` (`wg-stream-proxy check` runs only this step).
- `SIGHUP` reload (spec 5.6): the new config is validated completely. An
  invalid one is rejected and the old one keeps serving. Unchanged tunnels
  keep running; changed or removed tunnels are drained.
- Tunnel lifecycle: lazy start, idle close, drain (section 6).
- Signing key: signs and verifies `/r/` URLs (spec 4). Changing it needs
  a restart, like `listen` and the tunnels dir location.
- `/healthz` and `/metrics` (Prometheus, `wsp_` prefix, spec 7.3).

**Data plane**: moves bytes for players.

- `/playlist.m3u8`: renders the channel list from the active config. No
  upstream request, no tunnel.
- `/c/{id}`: the stable, unsigned entry point of a channel. Fetches the
  channel URL through the channel's route, then rewrites (HLS) or pipes
  (raw).
- `/r/...`: signed resources found inside rewritten playlists:
  sub-playlists, segments, keys. Same route and headers as the channel.
- Responses: playlists are rewritten, everything else (segments, keys,
  raw streams) is piped with `io.Copy`.
- The planes touch in four places: each request reads the *active*
  config, `/c/` and `/r/` use the signing key, each upstream dial goes
  through a route whose tunnel the lifecycle manager runs, and each
  request updates the metrics.

## 4. Request flows

### 4.1 Player loads the channel list

```mermaid
sequenceDiagram
    box rgba(59,127,176,0.18) Home LAN
        participant P as Player
    end
    box rgba(111,92,194,0.18) wg-stream-proxy
        participant S as server
        participant C as active config
        participant L as playlist
    end

    P->>S: GET /playlist.m3u8
    S->>C: enabled channels (optional ?group=)
    C-->>S: channels in file order
    S->>L: render
    Note over L: header with EPG link,<br/>extra lines, EXTINF line,<br/>absolute /c/{id} URL per channel
    L-->>S: M3U8 text
    S-->>P: 200 audio/x-mpegurl
    Note over P,S: No upstream request and no tunnel start
```

- Each entry points to `/c/{id}.m3u8` if `upstream.type = "hls"` or the
  upstream URL path contains `.m3u8`, otherwise to `/c/{id}` (spec 6). URLs are absolute: `public_base_url`, else
  `X-Forwarded-*`, else the request `Host` (spec 7.1).
- `#KODIPROP` and other `extra` lines appear only here, never in
  playlists under `/c/` or `/r/`.

### 4.2 HLS channel

Example channel from spec 5.4: `ert-world` on tunnel `gr`.

```mermaid
sequenceDiagram
    autonumber
    box rgba(59,127,176,0.18) Home LAN
        participant P as Player
    end
    box rgba(111,92,194,0.18) wg-stream-proxy
        participant S as server + hls
        participant R as route gr + guard
    end
    box rgba(42,138,104,0.18) Tunnel
        participant T as tunnel gr
    end
    box rgba(102,114,126,0.18) Internet
        participant U as CDN
    end

    P->>S: GET /c/ert-world.m3u8
    S->>R: GET channel URL, merged headers
    opt tunnel not running yet
        R->>T: start (concurrent first requests share it)
        Note over T: no handshake within connect_timeout:<br/>502, the next request retries
    end
    R->>T: resolve host via tunnel DNS
    R->>R: guard checks resolved IP
    R->>T: dial IP:port
    T->>U: GET master playlist
    U-->>S: 200 master playlist
    S->>S: detect HLS, rewrite every URI<br/>to a signed /r/ URL
    S-->>P: 200 rewritten master playlist
    loop live: media playlist refreshes, then segments
        P->>S: GET /r/ert-world/{sig}/{b64url}/index.m3u8
        S->>S: verify signature, look up channel
        S->>R: GET upstream URL (same route)
        R->>T: guard check, dial
        T->>U: GET media playlist
        U-->>S: media playlist
        S-->>P: rewritten media playlist
        P->>S: GET /r/ert-world/{sig}/{b64url}/seg.ts
        S->>R: GET upstream URL (same route)
        R->>T: pooled connection or new dial
        T->>U: GET segment
        U-->>S: segment bytes
        S-->>P: streamed (io.Copy)
    end
```

- Steps 2-8: `/c/` fetches the channel URL through the channel's route.
  The first request on a stopped tunnel starts it (section 6).
- Step 9: type detection (spec 3.1, `hls.Detect`): configured
  `upstream.type`, else `Content-Type`, else `#EXTM3U` at the start of the
  body, otherwise raw. Here: HLS, so `hls.Rewrite` replaces every
  `http(s)` and relative URI with `/r/{channel}/{sig}/{b64url}/{name}`
  (`hls.PathBuilder`, one HMAC state per playlist).
  Anything it cannot rewrite safely makes the whole playlist fail with
  `502` (fail closed, spec 3.2).
- The rewrite is **stateless**: the player chooses variants and
  renditions; the proxy keeps no session.
- `/r/` (spec 4): the signature binds the URL to the channel. A tampered
  URL, an unknown or a disabled channel gives `404`. Route and headers
  always come from the channel config, never from the URL.
- Sub-playlists are detected and rewritten again; everything else
  (segments, keys, init segments) is streamed unchanged. AES-128 keys are
  passed through; the player decrypts.
- Status mapping (spec 3.4): upstream `2xx`/`4xx` pass through, `5xx` and
  connection errors give `502`, timeouts `504`.

### 4.3 Radio: raw and ICY stream

```mermaid
sequenceDiagram
    box rgba(59,127,176,0.18) Home LAN
        participant P as Player
    end
    box rgba(111,92,194,0.18) wg-stream-proxy
        participant S as server
        participant R as route + guard
    end
    box rgba(102,114,126,0.18) Internet
        participant U as radio server
    end

    P->>S: GET /c/{radio-id} with Icy-MetaData
    S->>R: GET channel URL, forward Icy-MetaData
    R->>U: guard check, dial (tunnel or direct)
    U-->>R: ICY 200 OK, icy-* headers
    Note over R: connection wrapper rewrites<br/>"ICY " to "HTTP/1.0 "
    R-->>S: 200, body open
    S->>S: detect: raw (not HLS)
    S-->>P: 200, icy-* headers copied
    loop until the player disconnects
        U-->>S: audio bytes
        S-->>P: io.Copy + flush
    end
    Note over P,U: Player disconnect cancels the upstream request.<br/>No size or total time limit.
```

- Raw passthrough (spec 3.3) covers radio and MPEG-TS: anything that is
  not detected as HLS.
- Shoutcast v1 servers answer `ICY 200 OK` instead of an HTTP status
  line; the route's connection wrapper fixes that before Go's HTTP parser
  reads it.
- An open stream counts as tunnel activity, so the tunnel stays up while
  someone listens.

## 5. Routing and the guard

### 5.1 Which route a channel takes

```mermaid
flowchart TD
    start(["Channel in<br/>channels.d/FILE.toml"]):::proxy --> q1{"tunnel set on<br/>the channel?"}
    q1 -- yes --> name["Route name"]:::proxy
    q1 -- no --> q2{"tunnel set at the<br/>top of the file?"}
    q2 -- yes --> name
    q2 -- no --> q3{"tunnel file named<br/>like the file stem?"}
    q3 -- yes --> name
    q3 -- no --> err["Config error<br/>fail closed"]:::guard
    name --> q4{"name is<br/>direct?"}
    q4 -- yes --> d["Route direct<br/>host network"]:::direct
    q4 -- no --> q5{"tunnels/NAME.conf<br/>exists?"}
    q5 -- yes --> t["Route via<br/>tunnel NAME"]:::tunnel
    q5 -- no --> err

    classDef proxy fill:#6f5cc2,stroke:#46398a,color:#ffffff
    classDef tunnel fill:#2a8a68,stroke:#1a5e46,color:#ffffff
    classDef direct fill:#a86a1a,stroke:#6e4511,color:#ffffff
    classDef guard fill:#b94845,stroke:#7a2c2a,color:#ffffff
```

- First match wins (spec 5.2). The route is fixed when the config is
  loaded, not per request.
- `channels.d/_defaults.toml` sets no route: a global `tunnel` is a
  config error (spec 5.4).
- `direct` is never a default: it is used only where it is written. A
  channel without a route is a config error, so it can never fall back to
  the host network.
- `direct` is a reserved name; no tunnel file can be called `direct.conf`.

### 5.2 The private-range guard

```mermaid
flowchart TD
    a(["Route wants to<br/>connect to host:port"]):::proxy --> b["Resolve host<br/>tunnel: tunnel DNS<br/>direct: host resolver"]:::proxy
    b --> c{"IP in the route's<br/>allow_private list?"}
    c -- yes --> ok["Dial IP:port"]:::tunnel
    c -- no --> d{"Private or<br/>special range?"}
    d -- no --> ok
    d -- yes --> block["Dial refused<br/>counted as reason guard"]:::guard

    classDef proxy fill:#6f5cc2,stroke:#46398a,color:#ffffff
    classDef tunnel fill:#2a8a68,stroke:#1a5e46,color:#ffffff
    classDef guard fill:#b94845,stroke:#7a2c2a,color:#ffffff
```

- Every route offers two operations to the HTTP client: resolve a host
  name, and dial an already resolved `IP:port`. Tunnels resolve and dial
  through their netstack, `direct` through the host. The guard sits
  between the two, so it checks the exact IP that will be dialed.
- It runs on **every** dial: the channel URL, every `/r/` resource and
  every redirect hop (max 10). DNS rebinding to a private IP is caught
  too.
- Blocked ranges: loopback, RFC 1918, RFC 6598, link-local, IPv6 ULA,
  unspecified, multicast, and the IPv4-mapped forms of these.
- Public IPs and any port are allowed. Exceptions are per route, for
  example `[upstream.allow_private] direct = ["192.168.1.0/24"]`.
- Not guarded (spec 3.5): the WireGuard `Endpoint` (UDP from the host)
  and the tunnel's `DNS` servers.
- Inside a tunnel, `AllowedIPs` narrower than `0.0.0.0/0, ::/0` also
  limits which upstream IPs are reachable; others fail (spec 5.5).

## 6. Tunnel lifecycle

```mermaid
stateDiagram-v2
    direction LR
    [*] --> Stopped
    Stopped --> Starting: first request
    Starting --> Up: handshake ok
    Starting --> Stopped: start failed or no handshake<br/>within connect_timeout (502)
    Up --> Stopped: idle_timeout without activity
    Up --> Draining: shutdown, or reload<br/>changed or removed it
    Draining --> [*]: in-flight done or 30s
    note right of Starting
        Concurrent first requests
        wait for the same start.
    end note
```

- **Lazy start** (spec 2.1): nothing connects at startup. The first
  request for a channel on a stopped tunnel starts it. The `Endpoint` host
  name is resolved by the host resolver at that moment.
- **Start failure**: if the device cannot start or no handshake happens
  within `connect_timeout`, the waiting requests get `502`. The tunnel
  returns to *Stopped*; the next request tries again.
- **Idle close**: after `[tunnels] idle_timeout` (default `60m`) without
  activity the tunnel closes. Activity = any in-flight request or open
  stream.
- **Drain**: on shutdown or when a reload changes or removes a tunnel,
  in-flight requests get up to 30s, then the tunnel closes. A changed
  tunnel is recreated lazily: the new one starts in *Stopped*. Such a
  reload cancels a start in progress; its requests get `502` (spec 5.6).
- `wsp_tunnel_up{tunnel}` is `1` while the device runs and the last
  handshake is younger than 180s (spec 7.3).

## 7. Internals

How one running `wg-stream-proxy serve` process is built inside. The
sections above show what happens; this one shows which part of the code
does it. Type and function names are those on `main`; the code is
authoritative for them, the spec for the behaviour.

### 7.1 Layers and planes

Colours in this diagram mark the **planes**, as in section 3: blue is the
data plane (thick arrows), amber the control plane, purple the
observability plane (dotted lines). Red is the guard, grey the host
network. Section 3 counts health and metrics to the control plane; here
they are a plane of their own.

```mermaid
flowchart TB
    op(["Operator: SIGHUP"]):::ctl
    players(["Players: Kodi, VLC"]):::data
    prom(["Prometheus"]):::obs

    subgraph l1["1 · HTTP"]
        pl["/playlist.m3u8<br/>playlist.Render"]:::data
        cr["/c/{id}, /r/…<br/>handleChannel,<br/>handleResource"]:::data
        logos["/logos/{file}"]:::data
        ep["/healthz, /metrics"]:::obs
    end
    subgraph l2["2 · Generation"]
        rel["Reloader<br/>config.Load, tunnel.Parse"]:::ctl
        gen[("generation<br/>Config + route.Set")]:::data
    end
    subgraph l3["3 · Content"]
        proxy["Server.proxy<br/>fetch, then respond"]:::data
        det["hls.Detect<br/>HLS or raw"]:::data
        rw["hls.Rewrite + Signer<br/>URIs to signed /r/"]:::data
        pipe["stream<br/>io.Copy + flush"]:::data
        reg["metrics.Metrics<br/>wsp_* registry"]:::obs
    end
    subgraph l4["4 · Routes"]
        fetch["Route.Fetch<br/>http.Transport + ICY transport"]:::data
        guard{{"dial: lookup,<br/>Guard.Check, connect"}}:::guard
    end
    subgraph l5["5 · Tunnels"]
        mgr["tunnel.Manager<br/>Apply, idle close, drain"]:::ctl
        tun["Tunnel per file<br/>DNS, netstack,<br/>wireguard-go"]:::data
        direct["direct<br/>route.Host()"]:::data
    end
    subgraph l6["6 · Host network"]
        udp["UDP to the<br/>WireGuard peers"]:::inet
        tcp["TCP to the<br/>upstreams"]:::inet
    end

    players ==> pl & cr & logos
    op --> rel
    prom -.- ep
    rel -- "swap" --> gen
    pl & cr & logos ==> gen
    gen ==> proxy
    ep -.- reg
    proxy ==> fetch ==> guard
    guard ==> tun & direct
    rel -- "Apply" --> mgr
    mgr -- "start, stop" --> tun
    tun ==> udp
    direct ==> tcp
    reg -. "gauges" .- tun

    classDef data fill:#3b7fb0,stroke:#24577d,color:#ffffff
    classDef ctl fill:#a86a1a,stroke:#6e4511,color:#ffffff
    classDef obs fill:#6f5cc2,stroke:#46398a,color:#ffffff
    classDef inet fill:#66727e,stroke:#3f4851,color:#ffffff
    classDef guard fill:#b94845,stroke:#7a2c2a,color:#ffffff
    linkStyle 3,5,15,16 stroke:#a86a1a,stroke-width:2px
    linkStyle 4,10,19 stroke:#6f5cc2,stroke-width:2px
    style l1 fill:none,stroke:#66727e,stroke-width:2px
    style l2 fill:none,stroke:#66727e,stroke-width:2px
    style l3 fill:none,stroke:#66727e,stroke-width:2px
    style l4 fill:none,stroke:#66727e,stroke-width:2px
    style l5 fill:none,stroke:#66727e,stroke-width:2px
    style l6 fill:none,stroke:#66727e,stroke-width:2px
```

Requests go down the layers, responses travel back up.

| Layer | Owns | Package | Spec |
|---|---|---|---|
| 1 HTTP | Routing of the paths, absolute base URL, `/logos/` from disk, `/healthz`; graceful shutdown waits for running handlers | `internal/server`, `internal/playlist` | 6, 7.1 |
| 2 Generation | The immutable `generation`: one `config.Config` plus the `route.Set` built for it, in an `atomic.Pointer`. Every request takes it once and keeps it to its end | `internal/server`, `internal/config` | 5.6 |
| 3 Content | `Server.proxy`: fetch through the channel's route, then `hls.Detect`; playlists go through `hls.Rewrite` with a `Signer.PathBuilder`, everything else through `stream` (`io.Copy` + flush). Status mapping, the `206` refetch | `internal/hls`, `internal/server` | 3.1-3.4, 4 |
| 4 Routes | One `Route` per tunnel plus `direct`. `Fetch` applies the upstream header rules, redirects and timeouts; the route's `dial` resolves, runs `Guard.Check` and connects Happy-Eyeballs style. Two `http.Transport`s per route: the normal pool, and the ICY transport for `/c/` of raw and `auto` channels, which uses each connection for one request only | `internal/route` | 2.1, 3.3-3.5 |
| 5 Tunnels | `tunnel.Manager` with one `Tunnel` per tunnel file: lazy start, idle close, drain, reload (`Apply`). `direct` is `route.Host()`: host resolver and dialer | `internal/tunnel`, `internal/tunnel/netstack`, `internal/config/wgquick` | 2.1, 5.5 |
| 6 Host network | One UDP socket per running tunnel to its peer; plain TCP for `direct`. No TUN device, no routing table change | wireguard-go `conn` | 1.1, 2.1 |

- **Data plane**: a player request goes down the layers and back. The
  route layer only knows the `route.Network` interface (resolve a host,
  dial an `ip:port`); `*tunnel.Tunnel` and `route.Host()` implement it.
- **Control plane**: `cmd/wg-stream-proxy` turns `SIGHUP` into
  `Reloader.Reload`. It loads all files (`config.Load`, `tunnel.Parse`);
  if they are valid, `Manager.Apply` swaps the tunnel set,
  `route.Set.Next` builds the next routes (unchanged routes keep their
  pools), and the new generation is swapped in for new requests. Running
  requests finish on the old one. An invalid config changes nothing.
- **Observability plane**: `metrics.Metrics` is a private Prometheus
  registry. `Server.proxy` writes the request counters; the tunnel gauges
  are read from the tunnels at scrape time, so nothing polls the devices.
  `/healthz` only says that the process serves.

### 7.2 Channels, routes and tunnels

Example: two channels in `channels.d/gr.toml` take tunnel `gr` from the
file name, a radio channel names `uk`, and one channel is `direct`.

```mermaid
flowchart LR
    subgraph chans["Channels: config.Channel, route fixed at load"]
        direction TB
        subgraph fgr["channels.d/gr.toml: stem gr"]
            c1["ert-world"]:::proxy
            c2["ert-news"]:::proxy
        end
        subgraph fradio["channels.d/radio.toml"]
            c3["uk-radio<br/>tunnel = &quot;uk&quot;"]:::proxy
        end
        subgraph fde["channels.d/de.toml"]
            c4["das-erste<br/>tunnel = &quot;direct&quot;"]:::proxy
        end
    end
    subgraph set["route.Set of the generation"]
        direction TB
        rgr{{"Route gr<br/>+ guard"}}:::guard
        ruk{{"Route uk<br/>+ guard"}}:::guard
        rdir{{"Route direct<br/>+ guard"}}:::guard
    end
    subgraph nets["Networks"]
        direction TB
        tgr["Tunnel gr<br/>netstack + device"]:::tunnel
        tuk["Tunnel uk<br/>netstack + device"]:::tunnel
        host["route.Host()<br/>host resolver + dialer"]:::direct
    end
    subgraph inet["Internet"]
        direction TB
        pgr["VPN peer gr"]:::inet
        puk["VPN peer uk"]:::inet
        cdn["Upstreams<br/>CDNs, radio servers"]:::inet
    end
    fail["No handshake:<br/>502, no fallback"]:::guard

    c1 & c2 --> rgr
    c3 --> ruk
    c4 --> rdir
    rgr --> tgr
    ruk --> tuk
    rdir --> host
    tgr == "UDP, one session" ==> pgr
    tuk == "UDP, one session" ==> puk
    pgr & puk --> cdn
    host --> cdn
    tgr -. "start failed" .-> fail

    classDef proxy fill:#6f5cc2,stroke:#46398a,color:#ffffff
    classDef tunnel fill:#2a8a68,stroke:#1a5e46,color:#ffffff
    classDef direct fill:#a86a1a,stroke:#6e4511,color:#ffffff
    classDef inet fill:#66727e,stroke:#3f4851,color:#ffffff
    classDef guard fill:#b94845,stroke:#7a2c2a,color:#ffffff
    style chans fill:none,stroke:#6f5cc2,stroke-width:2px
    style fgr fill:none,stroke:#6f5cc2,stroke-width:1px
    style fradio fill:none,stroke:#6f5cc2,stroke-width:1px
    style fde fill:none,stroke:#6f5cc2,stroke-width:1px
    style set fill:none,stroke:#b94845,stroke-width:2px
    style nets fill:none,stroke:#2a8a68,stroke-width:2px
    style inet fill:none,stroke:#66727e,stroke-width:2px
```

- The route of a channel is fixed when the config loads (section 5.1,
  spec 5.2); the handler looks it up by name in the generation's
  `route.Set`.
- **Routes are shared** by all channels that name them: one guard with
  the route's `allow_private` list, one connection pool per transport.
- **Tunnels are shared** by all routes and viewers: one `Tunnel` per
  tunnel file, one key, one UDP session to the peer (spec 2.1). Only the
  route of the same name uses it.
- **`direct` bypasses tunnels** but not the guard: same transport
  settings, host resolver and dialer. It has no idle close and no drain.
- **Nothing falls back**: a route only knows its own network. If a tunnel
  cannot start or the guard blocks every address, the request fails with
  `502`; it never takes another route.

### 7.3 Inside one tunnel

```mermaid
flowchart LR
    route["Route gr<br/>dial, Fetch"]:::proxy

    subgraph tun["tunnel.Tunnel gr"]
        look["LookupNetIP<br/>host name"]:::proxy
        dialc["DialContext<br/>ip:port"]:::proxy
        dns["dnsClient<br/>A + AAAA, retries,<br/>TCP if truncated, cache"]:::proxy
        chk{{"check<br/>AllowedIPs,<br/>address family"}}:::guard
        act["Begin, end<br/>requests in flight"]:::ctl
        ens["ensure<br/>lazy start,<br/>one startCall"]:::ctl
        idle["idle timer"]:::ctl
        subgraph run["running: only while started"]
            ns["netstack.Net<br/>gVisor TCP/IP"]:::tunnel
            dev["wireguard-go<br/>device"]:::tunnel
            bind["conn.Bind<br/>UDP socket"]:::tunnel
        end
    end
    mgr["tunnel.Manager<br/>Apply, Shutdown"]:::ctl
    peer["VPN peer<br/>Endpoint"]:::inet
    up["DNS servers,<br/>upstreams"]:::inet

    route -- "resolve" --> look --> dns
    route -- "connect" --> dialc --> chk
    dns -- "UDP, TCP :53" --> ns
    chk -- "TCP" --> ns
    ns --> dev --> bind
    bind == "UDP" ==> peer --> up
    route -- "per request" --> act
    act -- "0 in flight" --> idle
    look & dialc -.-> ens
    ens -- "start" --> run
    idle -- "stop" --> run
    mgr -- "retire, drain" --> run

    classDef proxy fill:#6f5cc2,stroke:#46398a,color:#ffffff
    classDef tunnel fill:#2a8a68,stroke:#1a5e46,color:#ffffff
    classDef ctl fill:#a86a1a,stroke:#6e4511,color:#ffffff
    classDef inet fill:#66727e,stroke:#3f4851,color:#ffffff
    classDef guard fill:#b94845,stroke:#7a2c2a,color:#ffffff
    style tun fill:none,stroke:#2a8a68,stroke-width:2px
    style run fill:none,stroke:#2a8a68,stroke-width:1px
```

- **Interface to the route layer**: `*tunnel.Tunnel` implements
  `route.Network` (`LookupNetIP`, `DialContext`) and `route.Activity`
  (`Begin`, returning `end`). The route counts each request from before
  its first lookup until its body is closed.
- **`check`**: an address must be inside the peer's `AllowedIPs` and of a
  family the tunnel has an `Address` for, else `ErrNotAllowed` before
  anything is sent (spec 5.5). Lookup answers and the file's `DNS`
  servers are filtered the same way.
- **`dnsClient`**: tunnel DNS over UDP and TCP inside the netstack, A and
  AAAA in parallel (only for families the tunnel has), first attempt to
  every `DNS` server at once, retries after 0.5s, 1s, 2s, then every 4s
  on the next server, TCP after a truncated answer, IPv4 answers first
  (spec 2.1). It replaces the netstack resolver, which retries only after
  5s. It lives in `running`: answers are cached by TTL (10s to 5min, 256
  entries) only while the device runs, concurrent lookups of a name share
  one query, and every stop drops the cache.
- **`ensure`**: the lazy start. Concurrent callers wait for one
  `startCall`; `startDevice` resolves the `Endpoint` with the host
  resolver, builds the UAPI text (`wgquick.UAPIConfig`), creates the
  netstack and the device, and waits for the first handshake, all within
  `connect_timeout`. A failure is `ErrUnavailable`, answered with `502`.
- **`running`** (device + netstack) exists only while the tunnel is up.
  Every stop ends in `running.close`: the idle timer (armed when the last
  request ends, `idle_timeout` after the last activity), a reload that
  retires the tunnel (`Apply`: a start in progress is cancelled, requests
  in flight get up to 30s), and `Shutdown`. Before the device closes, the
  `OnStop` hook makes the route close its idle pooled connections.
- **`internal/tunnel/netstack`** is a trimmed copy of wireguard-go's
  `tun/netstack` with one change: a packet gVisor sends after `Close` is
  dropped instead of sent on a closed channel, which crashed the process
  ([#47](https://github.com/KingOfKalk/wg-stream-proxy/issues/47), spec
  9). It is updated together with wireguard-go.
- Keys stay inside: the UAPI text is never logged; wireguard-go's own log
  output reaches the debug log only with the peer shown by tunnel name and
  anything key-like redacted (spec 8).
- Upstream URLs stay out of the logs: every logged URL, and `route.Error.URL`
  (which ends up in error messages), goes through `route.LogURL` and shows
  only scheme, host and file name (spec 8).

### 7.4 A segment request through the layers

A `/r/` segment of `ert-world` while tunnel `gr` is already up.

```mermaid
sequenceDiagram
    autonumber
    box rgba(59,127,176,0.18) Home LAN
        participant P as Player
    end
    box rgba(111,92,194,0.18) wg-stream-proxy
        participant S as server
        participant G as generation
        participant M as metrics
        participant R as route gr
    end
    box rgba(42,138,104,0.18) Tunnel
        participant T as tunnel gr
    end
    box rgba(102,114,126,0.18) Internet
        participant U as CDN
    end

    P->>S: GET /r/ert-world/{sig}/{b64url}/seg.ts
    S->>S: Signer.Parse: channel id, upstream URL
    S->>G: active(), channel(id)
    G-->>S: enabled channel, its route gr
    Note over S,G: bad signature, unknown or<br/>disabled channel: 404
    S->>M: Begin(channel, segment)
    Note right of M: active_requests +1
    S->>R: Fetch(URL, channel headers, Range), header_timeout runs
    R->>T: Begin: in flight +1, idle timer off
    alt idle connection in the pool
        R->>R: reuse it, guard checked at its dial
    else new connection
        R->>T: LookupNetIP(host): tunnel DNS
        R->>R: Guard.Check every address
        R->>T: DialContext(ip:port), Happy Eyeballs
        T->>T: AllowedIPs check, device already up
    end
    R->>U: GET through netstack, device, UDP
    U-->>R: 200, headers
    R-->>S: response, body with read_timeout
    S->>S: hls.Detect: not a playlist
    S->>M: Response(200)
    Note right of M: upstream_requests_total
    loop until the body ends
        U-->>S: bytes
        S-->>P: io.Copy + flush
    end
    S->>R: close body
    R->>T: end: in flight -1, idle timer armed at 0
    S->>M: End()
    Note right of M: active_requests -1
```

- Steps 2-4 (layers 1-2): the signature binds the URL to the channel; the
  generation taken here serves the request to its end, even if a reload
  swaps in a new one meanwhile.
- Step 5: `wsp_active_requests{channel,kind}` counts from here. The kind
  is `segment` because the path does not end in `.m3u8` or `.m3u`; a
  detected playlist would switch it to `playlist`.
- Step 7: from here until step 21 the request is tunnel activity, so an
  open stream keeps the tunnel up and an idle pooled connection does not.
- Steps 8-12 (layer 4): the guard runs on every new connection, also for
  each redirect hop, never on a reused one (it was checked when it was
  dialed).
- Step 17: `wsp_upstream_requests_total{channel,kind,code}`. On an error
  before the response, `FetchError` counts `code="error"` and
  `wsp_upstream_errors_total{channel,reason}`, and the player gets `502`
  or `504`. A request the player gave up counts no error.
- A playlist (step 16 detects HLS) is read whole, rewritten and sent
  instead of streamed, as in section 4.2.

## 8. Package map

Layout per spec 9. Arrows show the expected main dependencies; package
boundaries inside this layout are an implementation choice, so the code is
authoritative.

```mermaid
flowchart TD
    main["cmd/wg-stream-proxy<br/>subcommands"]:::done
    server["internal/server<br/>HTTP handlers, Run, reload"]:::done
    playlist["internal/playlist<br/>channel list"]:::done
    hls["internal/hls<br/>Detect, Rewrite, Signer"]:::done
    route["internal/route<br/>routes, guard, upstream rules"]:::done
    tunnel["internal/tunnel<br/>Manager, Tunnel, DNS"]:::done
    netstack["internal/tunnel/netstack<br/>copy of wireguard-go's"]:::done
    config["internal/config<br/>Resolve, Load"]:::done
    wgquick["internal/config/wgquick<br/>Parse, UAPIConfig"]:::done
    metrics["internal/metrics<br/>Prometheus"]:::done
    importer["internal/importer<br/>M3U import"]:::done
    epg["internal/epg<br/>epg ids, epg match"]:::done

    main --> server & config & importer & epg & route & tunnel & hls
    server --> playlist & hls & route & tunnel & metrics
    playlist --> config
    route --> config
    tunnel -. "implements route Network" .-> route
    tunnel --> config & wgquick & netstack
    importer --> config & epg
    epg --> config & route

    classDef done fill:#2a8a68,stroke:#1a5e46,color:#ffffff
```

All packages of v0.1 exist on `main`. Section 7 shows how they work
together at run time.

| Package | Role | Status |
|---|---|---|
| `cmd/wg-stream-proxy` | Subcommand dispatch; `serve`, `check`, `import`, `healthcheck`, `epg` and `version` work | exists |
| `internal/config` | Config dir resolution (`Resolve`), loading and validation of `config.toml` and channel files (`Load`), slugs (`Slug`), route resolution | exists |
| `internal/config/wgquick` | Tunnel file parser (`Parse`), UAPI conversion (`UAPIConfig`), `AllowedIPs` check (`Allows`) | exists |
| `internal/hls` | Type detection (`Detect`), playlist rewriter (`Rewrite`), signed `/r/` URLs (`Signer`), signing key (`LoadSigningKey`); pure functions, no I/O | exists |
| `internal/tunnel` | One `wireguard-go` device + gVisor netstack per tunnel, DNS inside the tunnel, lazy start ([#11](https://github.com/KingOfKalk/wg-stream-proxy/issues/11)); idle close and drain ([#16](https://github.com/KingOfKalk/wg-stream-proxy/issues/16)); reload (`Manager.Apply`) | exists |
| `internal/tunnel/netstack` | Trimmed copy of wireguard-go's `tun/netstack` (MIT) that drops packets sent after `Close` ([#47](https://github.com/KingOfKalk/wg-stream-proxy/issues/47), spec 9) | exists |
| `internal/route` | One route per tunnel plus `direct`, `http.Transport` settings, guard, upstream header rules, timeouts, status mapping ([#12](https://github.com/KingOfKalk/wg-stream-proxy/issues/12)); raw and ICY ([#15](https://github.com/KingOfKalk/wg-stream-proxy/issues/15)). Defines the `Network` interface (resolve + dial a resolved `IP:port`) that tunnels and `direct` implement | exists |
| `internal/server` | `/playlist.m3u8`, `/c/`, `/r/`, `/healthz`, absolute URL base, graceful shutdown (`Run`) ([#14](https://github.com/KingOfKalk/wg-stream-proxy/issues/14)); `/logos/`, `healthcheck` ([#19](https://github.com/KingOfKalk/wg-stream-proxy/issues/19)); `/metrics` ([#18](https://github.com/KingOfKalk/wg-stream-proxy/issues/18)); `SIGHUP` reload (`Reloader`) | exists |
| `internal/playlist` | Renders the channel list (spec 6) | exists |
| `internal/metrics` | Private Prometheus registry with the `wsp_` metrics (spec 7.3) and the Go and process metrics; tunnel gauges read at scrape time ([#18](https://github.com/KingOfKalk/wg-stream-proxy/issues/18)) | exists |
| `internal/importer` | `import` of an existing M3U file (spec 7.2), `--match-epg` with an `epg.Index`; pure function, no I/O | exists |
| `internal/epg` | Downloads `epg.url` over the direct route, streams the XMLTV channel list (plain or gzip), ranks EPG names for `epg match` and `import --match-epg` (spec 7.2, [#21](https://github.com/KingOfKalk/wg-stream-proxy/issues/21)) | exists |

## 9. Versions

### 9.1 What each version adds

| Version | Adds | Where it shows up |
|---|---|---|
| **v0.1 Core** | Everything in the spec: config, tunnels, routes and guard, HLS rewrite, raw/ICY, signed `/r/`, channel list, `SIGHUP` reload, metrics, `import`, `epg`; Dockerfile, Compose file, image on `ghcr.io` (`linux/amd64`, `linux/arm64`); test layers L1-L6 | the whole picture above |
| **v0.2 Experience** | Placeholder and error clips with seamless switchover (own design doc), per-request prefetch, file-watch reload | data plane (clips, prefetch), control plane (reload trigger) |
| **v0.3 Distribution** | GoReleaser, systemd unit, launchd plist, Kustomize | deployment only |
| **Later** | 32-bit ARM, scaling beyond 3 viewers, EPG proxy mode, logo proxying, per-tunnel overrides, ad-hoc proxy endpoint, DASH | mostly data plane |

```mermaid
flowchart LR
    v01["v0.1 Core<br/>spec complete<br/>container image"]:::proxy
    v02["v0.2 Experience<br/>clips, prefetch,<br/>file-watch reload"]:::later
    v03["v0.3 Distribution<br/>GoReleaser, systemd,<br/>launchd, Kustomize"]:::later
    later["Later<br/>EPG and logo proxy, DASH,<br/>32-bit ARM, more viewers,<br/>per-tunnel overrides,<br/>ad-hoc endpoint"]:::later
    v01 --> v02 --> v03 -.-> later

    classDef proxy fill:#6f5cc2,stroke:#46398a,color:#ffffff
    classDef later fill:#c3cad2,stroke:#5b6670,color:#1b1f24,stroke-dasharray:5 3
```

### 9.2 Final version

The same structure as in section 2; future parts are grey and dashed. The
details of v0.2 and later are not specified yet (the clips get their own
design doc), so this shows only *where* each part fits.

```mermaid
flowchart LR
    subgraph lan["Home LAN"]
        players["Kodi, VLC"]:::lan
    end
    subgraph proxy["wg-stream-proxy"]
        direction TB
        ctl["Config + reload<br/>SIGHUP"]:::proxy
        watch["File-watch reload<br/>v0.2"]:::later
        srv["HTTP server<br/>playlist, /c, /r"]:::proxy
        clips["Placeholder and<br/>error clips v0.2"]:::later
        pre["Prefetch v0.2"]:::later
        extra["EPG proxy, logo proxy,<br/>ad-hoc endpoint, DASH<br/>later"]:::later
        routes{{"Routes + guard"}}:::guard
    end
    subgraph nets["Tunnels and direct"]
        direction TB
        tun["Tunnels<br/>per-tunnel overrides later"]:::tunnel
        direct["direct"]:::direct
    end
    subgraph inet["Internet"]
        direction TB
        cdn["Broadcaster CDNs"]:::inet
        epg["EPG"]:::inet
    end

    players --> srv
    watch -.-> ctl
    ctl --> srv
    srv -.-> clips
    srv -.-> pre
    srv -.-> extra
    srv --> routes
    pre -.-> routes
    extra -.-> routes
    routes --> tun & direct
    tun & direct --> cdn
    players -. "EPG link mode" .-> epg

    classDef lan fill:#3b7fb0,stroke:#24577d,color:#ffffff
    classDef proxy fill:#6f5cc2,stroke:#46398a,color:#ffffff
    classDef tunnel fill:#2a8a68,stroke:#1a5e46,color:#ffffff
    classDef direct fill:#a86a1a,stroke:#6e4511,color:#ffffff
    classDef inet fill:#66727e,stroke:#3f4851,color:#ffffff
    classDef guard fill:#b94845,stroke:#7a2c2a,color:#ffffff
    classDef later fill:#c3cad2,stroke:#5b6670,color:#1b1f24,stroke-dasharray:5 3
    style lan fill:none,stroke:#3b7fb0,stroke-width:2px
    style proxy fill:none,stroke:#6f5cc2,stroke-width:2px
    style nets fill:none,stroke:#2a8a68,stroke-width:2px
    style inet fill:none,stroke:#66727e,stroke-width:2px
```

- The core stays the same in every version: one process, routes with the
  guard in front of every dial, tunnels in userspace, fail closed.
- v0.2 adds behaviour *in front of* the routes (clips, prefetch) and a
  second reload trigger next to `SIGHUP`.
- v0.3 changes only how the binary is shipped and run (section 10).
- Later items extend the data plane (EPG and logos fetched through the
  proxy, an ad-hoc endpoint, DASH) or the limits (more viewers, 32-bit
  ARM).

## 10. Deployment

```mermaid
flowchart TB
    subgraph v01["v0.1"]
        direction LR
        img["Container image<br/>ghcr.io, distroless,<br/>non-root"]:::proxy
        bin["Binary<br/>linux amd64 + arm64,<br/>darwin arm64"]:::proxy
        pi["Raspberry Pi 5<br/>LibreELEC Docker add-on"]:::lan
        compose["Docker / Compose<br/>Linux host"]:::lan
        host["Linux or macOS<br/>standalone"]:::lan
        img --> pi & compose
        bin --> host
    end
    subgraph v03["v0.3"]
        direction LR
        gor["GoReleaser<br/>release binaries"]:::later
        svc["systemd unit (Linux)<br/>launchd plist (macOS)"]:::later
        k8s["Kustomize base<br/>1 replica, Recreate"]:::later
        cluster["Kubernetes"]:::later
        gor -.-> svc
        k8s -.-> cluster
    end
    v01 ~~~ v03

    classDef lan fill:#3b7fb0,stroke:#24577d,color:#ffffff
    classDef proxy fill:#6f5cc2,stroke:#46398a,color:#ffffff
    classDef later fill:#c3cad2,stroke:#5b6670,color:#1b1f24,stroke-dasharray:5 3
    style v01 fill:none,stroke:#6f5cc2,stroke-width:2px
    style v03 fill:none,stroke:#5b6670,stroke-width:2px,stroke-dasharray:5 3
```

- **One instance only**: tunnel state lives in memory, and VPN providers
  often limit connections per key ([deploy.md](deploy.md)).
- Every variant uses the same config layout (spec 5.1) and env vars,
  default `/etc/wg-stream-proxy` (mounted read-only in the container).
  Tunnel files must be readable by the process but not world-readable.
  The signing key comes from `WG_STREAM_PROXY_SIGNING_KEY_FILE` or
  `WG_STREAM_PROXY_SIGNING_KEY`; without one, `/r/` URLs do not survive a
  restart.
- The process needs no root and no capabilities: the tunnels are in
  userspace, so there is no TUN device and no routing table change.
- LAN use only; exposing it to the internet needs an authenticating
  reverse proxy (spec 8).
- Details per variant: [deploy.md](deploy.md).

## 11. Glossary

| Term | Meaning |
|---|---|
| Channel | One entry of the channel list: name, upstream URL, route, playlist attributes. Configured in `channels.d/*.toml`. |
| Upstream | The server the stream really comes from (broadcaster CDN, radio server), and its URL. |
| Route | How a channel reaches its upstream: through a named tunnel or `direct`. One per tunnel file plus `direct`, each with its own HTTP client. |
| Tunnel | A userspace WireGuard connection (`wireguard-go` + gVisor netstack) built from one `tunnels/*.conf` file. Shared by all channels using it. |
| `direct` | The reserved route over the host's normal network. Used only when written in the config. |
| Guard | The private-range guard: checks the resolved IP before every upstream dial and blocks LAN, loopback and similar ranges unless allowed per route. |
| Fail closed | When something is wrong (no route, tunnel down, unsafe playlist), the request fails; it never falls back to a less safe path. |
| Resource URL | A signed `/r/{channel}/{sig}/{b64url}/{name}` URL that replaces an upstream URL inside a rewritten playlist. |
| Signing key | The secret for the HMAC signature of resource URLs, so `/r/` only fetches URLs that came from a rewritten playlist of that channel. |
| Control plane | Config, reload, tunnel lifecycle, signing key, health and metrics. |
| Data plane | The request path: channel list, `/c/`, `/r/`, raw pipe. |
| Observability plane | `/healthz`, `/metrics` and what feeds them. Section 3 counts it to the control plane; section 7 draws it on its own. |
| Generation | One loaded config with the routes built for it. A request uses the generation it started with; a reload swaps in the next one for new requests. |
