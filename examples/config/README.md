# Example config dir

A complete config dir ([spec 5.1](../../docs/spec.md#51-directories-and-precedence))
to try wg-stream-proxy and for the container smoke test.

| File | Content |
|---|---|
| `config.toml` | All settings with their defaults (spec 5.3) |
| `channels.d/greek-tv.toml` | The example channels of spec 5.4: one through the tunnel `gr`, one `direct` |
| `tunnels/gr.conf` | **Example only**: test-only keys, endpoint `vpn.example.com`; it connects nowhere |

Git does not keep file modes. Make the tunnel file readable only by the
proxy user, or the proxy warns that it is world-readable:

```sh
chmod 600 examples/config/tunnels/gr.conf
wg-stream-proxy check --config-dir examples/config
wg-stream-proxy serve --config-dir examples/config
```

`/playlist.m3u8` and `/healthz` work with this config. The `gr` channel
answers `502`, as the example tunnel cannot connect; replace
`tunnels/gr.conf` with your provider's wg-quick file to use it.
