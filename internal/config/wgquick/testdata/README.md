# Test fixtures

All WireGuard keys in this directory and in the tests of this package are
TEST-ONLY: freshly generated for this repository, they protect nothing.
Never use them for a real tunnel. Addresses use documentation ranges
(`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`, `2001:db8::/32`),
endpoints use `example.com`.

- `full.conf`: every honoured key; `full.uapi`: its UAPI text with the
  endpoint resolved to `192.0.2.1`.
- `minimal.conf`: only the required keys.
