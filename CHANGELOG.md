# Changelog

## [0.2.1](https://github.com/KingOfKalk/wg-stream-proxy/compare/v0.2.0...v0.2.1) (2026-11-19)


### Bug Fixes

* **server:** refuse playlists the proxy can't rewrite instead of bypassing the tunnel ([#111](https://github.com/KingOfKalk/wg-stream-proxy/issues/111)) ([b9cc97a](https://github.com/KingOfKalk/wg-stream-proxy/commit/b9cc97aeeb1d97d7aae56b02b64c751b8eb33530))


### Build System

* **container:** let Prune delete old image versions ([#103](https://github.com/KingOfKalk/wg-stream-proxy/issues/103)) ([7825072](https://github.com/KingOfKalk/wg-stream-proxy/commit/782507248c31f320c0ad070fa778b4c2e8bfd254))

## [0.2.0](https://github.com/KingOfKalk/wg-stream-proxy/compare/v0.1.0...v0.2.0) (2026-11-19)


### Features

* **metrics:** timing histograms for tunnel starts and upstream phases ([#93](https://github.com/KingOfKalk/wg-stream-proxy/issues/93)) ([6701987](https://github.com/KingOfKalk/wg-stream-proxy/commit/6701987e7fc9824ee6de7871abdedf99e8bb530e))
* **server:** optional debug endpoints for profiles and recent requests ([#94](https://github.com/KingOfKalk/wg-stream-proxy/issues/94)) ([b540523](https://github.com/KingOfKalk/wg-stream-proxy/commit/b540523fc5f368cc304105c02c9198e3d000e9f0))


### Bug Fixes

* **server:** answer debug requests only by IP or localhost (DNS rebinding) ([#96](https://github.com/KingOfKalk/wg-stream-proxy/issues/96)) ([9cc04a3](https://github.com/KingOfKalk/wg-stream-proxy/commit/9cc04a331f9958c20b09f44411f87d178341474d))


### Documentation

* **observability:** Grafana dashboard and Prometheus guide ([#98](https://github.com/KingOfKalk/wg-stream-proxy/issues/98)) ([b3ba0f5](https://github.com/KingOfKalk/wg-stream-proxy/commit/b3ba0f5c8e39c9fcbe5aba7ba1070824a0583ffe))
* **spec:** reorder the roadmap: v0.2 Observability, v0.3 Experience, v0.4 Content ([#91](https://github.com/KingOfKalk/wg-stream-proxy/issues/91)) ([1927ab0](https://github.com/KingOfKalk/wg-stream-proxy/commit/1927ab026a7d5a76fccb1599da47cd11f236499e))


### Tests

* **server:** wait for the request to end in TestDebugTunnelsAndCooldown ([#100](https://github.com/KingOfKalk/wg-stream-proxy/issues/100)) ([29598a8](https://github.com/KingOfKalk/wg-stream-proxy/commit/29598a8121713f10f2a8db7323f5a1c7169c7bc9))


### Continuous Integration

* **player:** cache the ffmpeg packages and retry slow apt downloads ([#102](https://github.com/KingOfKalk/wg-stream-proxy/issues/102)) ([fbcbc9e](https://github.com/KingOfKalk/wg-stream-proxy/commit/fbcbc9e28369690e9949e0e281e58855d3f2bdcf))

## 0.1.0 (2026-11-18)


### Features

* **config:** global channel defaults in channels.d/_defaults.toml ([#81](https://github.com/KingOfKalk/wg-stream-proxy/issues/81)) ([6ff6ff0](https://github.com/KingOfKalk/wg-stream-proxy/commit/6ff6ff0e2c341dd0860f662bed24d08aa1a87174))
* **config:** load config.toml and channel files ([#33](https://github.com/KingOfKalk/wg-stream-proxy/issues/33)) ([11f97fd](https://github.com/KingOfKalk/wg-stream-proxy/commit/11f97fd4277e81e1a9962d7e18cc4dd110f4f05c))
* **config:** parse wg-quick tunnel files ([#32](https://github.com/KingOfKalk/wg-stream-proxy/issues/32)) ([52eb5ed](https://github.com/KingOfKalk/wg-stream-proxy/commit/52eb5ed8a6d2c86ddae2bd90f6b724c8defefafb))
* **config:** reload on SIGHUP ([#55](https://github.com/KingOfKalk/wg-stream-proxy/issues/55)) ([25f9405](https://github.com/KingOfKalk/wg-stream-proxy/commit/25f9405a2f20e3a8e804f4205d26b58e12f7e809))
* **epg:** add epg ids and epg match ([#52](https://github.com/KingOfKalk/wg-stream-proxy/issues/52)) ([18c2843](https://github.com/KingOfKalk/wg-stream-proxy/commit/18c2843aa49eafa7679e2dcbb47b588eade22d01))
* **hls:** rewrite HLS playlists and sign resource URLs ([#34](https://github.com/KingOfKalk/wg-stream-proxy/issues/34)) ([dbd2590](https://github.com/KingOfKalk/wg-stream-proxy/commit/dbd25905b845e225921a2f80fff502032ff9a470))
* **importer:** import M3U files ([#49](https://github.com/KingOfKalk/wg-stream-proxy/issues/49)) ([8544aaa](https://github.com/KingOfKalk/wg-stream-proxy/commit/8544aaaea25cfb280269595bd826ec92bbe71d19))
* **metrics:** expose Prometheus metrics ([#53](https://github.com/KingOfKalk/wg-stream-proxy/issues/53)) ([effd3d3](https://github.com/KingOfKalk/wg-stream-proxy/commit/effd3d34688ad06b40f8b3f6d18c18c7e7789a30))
* **route:** add routes, private-range guard and upstream HTTP rules ([#37](https://github.com/KingOfKalk/wg-stream-proxy/issues/37)) ([531c2dd](https://github.com/KingOfKalk/wg-stream-proxy/commit/531c2dd2692a4c0357c84ba14edeeff8e0728476))
* **route:** pass through raw and ICY streams ([#45](https://github.com/KingOfKalk/wg-stream-proxy/issues/45)) ([693b2b3](https://github.com/KingOfKalk/wg-stream-proxy/commit/693b2b3dfee003ca3a7409f77cec40f6146cc941))
* **server:** debug log with a timing breakdown per request ([#69](https://github.com/KingOfKalk/wg-stream-proxy/issues/69)) ([44d9023](https://github.com/KingOfKalk/wg-stream-proxy/commit/44d9023177bb8cf9fd8c589e3efc676d6c352655))
* **server:** log channel changes on reload ([#80](https://github.com/KingOfKalk/wg-stream-proxy/issues/80)) ([aa9dbc7](https://github.com/KingOfKalk/wg-stream-proxy/commit/aa9dbc7710f3bf50a9b8102ecb3e0f7315c4e729))
* **server:** serve channel list, channels and resources ([#42](https://github.com/KingOfKalk/wg-stream-proxy/issues/42)) ([bb814cb](https://github.com/KingOfKalk/wg-stream-proxy/commit/bb814cb291ff3b989cbfba27f17dd5cc79bd889c))
* **server:** serve local logos and add healthcheck command ([#43](https://github.com/KingOfKalk/wg-stream-proxy/issues/43)) ([ec1817a](https://github.com/KingOfKalk/wg-stream-proxy/commit/ec1817a2505d87dc78d4ed37655bbf1fd601f3b4))
* **tunnel:** close idle tunnels and drain on shutdown ([#48](https://github.com/KingOfKalk/wg-stream-proxy/issues/48)) ([d52590d](https://github.com/KingOfKalk/wg-stream-proxy/commit/d52590d6f0d7f2454ed7ac3a4ae9b7d8c43af96f))
* **tunnel:** run userspace WireGuard tunnels ([#38](https://github.com/KingOfKalk/wg-stream-proxy/issues/38)) ([f1d4bd3](https://github.com/KingOfKalk/wg-stream-proxy/commit/f1d4bd3d1c8ac0322ca3f77d4d6f24ac4ad496ee))


### Bug Fixes

* **route:** do not reuse ICY connections ([#64](https://github.com/KingOfKalk/wg-stream-proxy/issues/64)) ([a71b21f](https://github.com/KingOfKalk/wg-stream-proxy/commit/a71b21fbc79e9a03042a7a58dcd9d304cf434e27))
* **server:** log only host and file name of upstream URLs in debug lines ([#79](https://github.com/KingOfKalk/wg-stream-proxy/issues/79)) ([56512ad](https://github.com/KingOfKalk/wg-stream-proxy/commit/56512adebc58cb306f63bcf86555d4198a3aa1c4))
* **tunnel:** avoid send on closed channel when a netstack tunnel closes ([#54](https://github.com/KingOfKalk/wg-stream-proxy/issues/54)) ([a67627d](https://github.com/KingOfKalk/wg-stream-proxy/commit/a67627d459b93c5680faf1657c80a62704fa08d5))


### Performance Improvements

* **hls:** handle large DVR media playlists ([#82](https://github.com/KingOfKalk/wg-stream-proxy/issues/82)) ([9a9d356](https://github.com/KingOfKalk/wg-stream-proxy/commit/9a9d35608f810e113941064c920119a79302e792))
* **tunnel:** cache tunnel DNS answers and ask all DNS servers first ([#78](https://github.com/KingOfKalk/wg-stream-proxy/issues/78)) ([3fc6483](https://github.com/KingOfKalk/wg-stream-proxy/commit/3fc64836712c8bac980adf707dd4ea3a16f09f51))
* **tunnel:** retry lost DNS queries inside tunnels after 1s ([#40](https://github.com/KingOfKalk/wg-stream-proxy/issues/40)) ([2ff7af4](https://github.com/KingOfKalk/wg-stream-proxy/commit/2ff7af439cb1a376b156b0325c37ef04117b8460))


### Documentation

* add architecture overview with diagrams ([#36](https://github.com/KingOfKalk/wg-stream-proxy/issues/36)) ([3274888](https://github.com/KingOfKalk/wg-stream-proxy/commit/32748880b4aa8c4b12b6bf155f1bd823b39195bf))
* add spec, deploy, testing and agent docs ([#4](https://github.com/KingOfKalk/wg-stream-proxy/issues/4)) ([1de9329](https://github.com/KingOfKalk/wg-stream-proxy/commit/1de9329986bda95aa67b4eebfd75baedd1fab275))
* explain the internals of the running proxy in architecture.md ([#62](https://github.com/KingOfKalk/wg-stream-proxy/issues/62)) ([475f18d](https://github.com/KingOfKalk/wg-stream-proxy/commit/475f18d91686015be3ab8e33296fbba5419073d6))
* plan v0.1 test hardware, risks and image publishing ([#26](https://github.com/KingOfKalk/wg-stream-proxy/issues/26)) ([c4d1eaa](https://github.com/KingOfKalk/wg-stream-proxy/commit/c4d1eaa0babed1b9283df256985c66830b660729))
* recommend inputstream.adaptive for HLS channels in Kodi ([#75](https://github.com/KingOfKalk/wg-stream-proxy/issues/75)) ([7afc03e](https://github.com/KingOfKalk/wg-stream-proxy/commit/7afc03ea87cf63901bf9c4ba38c737f9b483d0de))
* show radio channels for Kodi with radio="true" ([#71](https://github.com/KingOfKalk/wg-stream-proxy/issues/71)) ([52cf626](https://github.com/KingOfKalk/wg-stream-proxy/commit/52cf62621502f4dc1a3c2bda9c29417d8b72a18b))


### Tests

* add ffmpeg player tests ([#46](https://github.com/KingOfKalk/wg-stream-proxy/issues/46)) ([b70fcf1](https://github.com/KingOfKalk/wg-stream-proxy/commit/b70fcf1b8c6d0e441af4933f28775f6c8b0e99f8))
* fix two timing flakes in tunnel and reload tests ([#65](https://github.com/KingOfKalk/wg-stream-proxy/issues/65)) ([ba1597f](https://github.com/KingOfKalk/wg-stream-proxy/commit/ba1597f6f719f41ea3209c8d9de6974c8b524382))


### Build System

* add container image, Compose file and ghcr.io publishing ([#44](https://github.com/KingOfKalk/wg-stream-proxy/issues/44)) ([767e548](https://github.com/KingOfKalk/wg-stream-proxy/commit/767e5485676b085a3f5500341e1064be77d7343d))
* add Go module skeleton and CI ([#27](https://github.com/KingOfKalk/wg-stream-proxy/issues/27)) ([679daa4](https://github.com/KingOfKalk/wg-stream-proxy/commit/679daa4cac3d72cc600df1f636e6008202676c1f))
* publish images without attestation manifests and prune old versions ([#57](https://github.com/KingOfKalk/wg-stream-proxy/issues/57)) ([361f44b](https://github.com/KingOfKalk/wg-stream-proxy/commit/361f44bfc6e43f6fa96f61fb23732051bfff2c60))


### Continuous Integration

* add nightly workflow ([#50](https://github.com/KingOfKalk/wg-stream-proxy/issues/50)) ([e8913e2](https://github.com/KingOfKalk/wg-stream-proxy/commit/e8913e2e128036d28521276e091dc9f3a61357ce))
* adopt repository standard with release App ([#3](https://github.com/KingOfKalk/wg-stream-proxy/issues/3)) ([5ba4655](https://github.com/KingOfKalk/wg-stream-proxy/commit/5ba4655519c2c0f5b290bdfc1a9284a62c638b8c))
* give every CI job its own Go cache ([#31](https://github.com/KingOfKalk/wg-stream-proxy/issues/31)) ([fade6c9](https://github.com/KingOfKalk/wg-stream-proxy/commit/fade6c98b5dcd6d045c3c3ab0e2553fccf1fca47))
* run lint first, then test and security, then build ([#29](https://github.com/KingOfKalk/wg-stream-proxy/issues/29)) ([84f115f](https://github.com/KingOfKalk/wg-stream-proxy/commit/84f115fb50154da1f140043eefabc7a88db3c0b0))
* run short fuzzing and the capacity test on every push ([#60](https://github.com/KingOfKalk/wg-stream-proxy/issues/60)) ([bf67a82](https://github.com/KingOfKalk/wg-stream-proxy/commit/bf67a82a61321971a7e5badf00d0e7530e36ddd0))
