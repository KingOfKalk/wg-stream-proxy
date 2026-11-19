# AGENTS.md

Guidance for AI coding agents working on this repository.

## Project

- Go service: route each IPTV and internet radio channel through its own
  userspace WireGuard tunnel (or direct) and serve them as one M3U playlist
  for Kodi, VLC and other players. Proxies HLS (TS, fMP4, AES-128) and
  raw/ICY audio streams; no root, no TUN device, fails closed.
- Spec (binding): [docs/spec.md](docs/spec.md). Deployment:
  [docs/deploy.md](docs/deploy.md). Testing:
  [docs/testing.md](docs/testing.md). Monitoring:
  [docs/observability.md](docs/observability.md). Clips design (v0.3):
  [docs/clips.md](docs/clips.md).
- Git workflow, labels, issue lifecycle, releases:
  [CONTRIBUTING.md](CONTRIBUTING.md).

## Rules

1. Precise, short answers. No fluff, no sycophancy.
2. Work via issues and pull requests ([CONTRIBUTING.md](CONTRIBUTING.md)).
   Never commit or push on `main`, never force-push, never merge a PR (the
   owner does).
3. Keep changes minimal and focused. Build only what the spec lists for
   the current roadmap version (spec 10); no extras.
4. Behavior changes update [docs/spec.md](docs/spec.md) in the same PR.
5. TDD: write the failing test first, see it fail for the right reason,
   then implement. Follow [docs/testing.md](docs/testing.md): `-race`,
   `goleak`, injected clock instead of `time.Sleep`, no real broadcaster
   URLs in tests or CI.
6. Code conventions: spec 9 (layout, dependencies, SPDX header,
   `atomic.Int64` types, comments in English).
7. Code, comments, commit messages and docs in English.

## Public repository

This repository is public. Everything committed, and every issue, PR and
comment, is world-readable and permanent.

8. Never commit real WireGuard keys or tunnel files, signing keys, VPN
   provider endpoints or accounts, personal channel lists, or URLs with
   tokens. Fixtures and examples use generated test-only keys and
   anonymised data.
9. Never log, echo, or commit secrets. If you detect one in staged
   changes or output, abort and warn the user.
10. Commit only as `KingOfKalk` with the GitHub `noreply` address
    (`git config user.name` / `user.email`); no other personal data in
    commits, issues or docs.
11. No AI attribution: not in commits, branch names, issues or PRs. No
    co-authoring.

## Toolchain

12. The dev container has no Go toolchain. Run Go in a throwaway
    container; container paths equal workspace paths:

    ```sh
    docker run --rm -v "$PWD":"$PWD" -w "$PWD" \
      --user "$(id -u):$(id -g)" -e HOME=/tmp \
      golang:<version> go test -race ./...
    ```

    `<version>`: the image CI uses, otherwise the latest stable tag,
    pinned exactly.
13. Before marking a PR ready, run the CI checks locally where possible
    and make sure CI is green ([CONTRIBUTING.md](CONTRIBUTING.md#checks)).

## Dependencies

14. Never take a version from memory. Look up the latest stable release
    at the authoritative source (forge releases API, Go proxy, registry),
    pin it exactly, and name version and source in the PR description.
15. gVisor stays at the version in `wireguard-go`'s `go.mod` and is only
    updated together with it (spec 9). The same holds for the patched
    `tun/netstack` copy in `internal/tunnel/netstack`.
16. Workflows: `permissions: {}` at the top and the minimum per job,
    actions pinned by full commit SHA with a `# vX.Y.Z` comment,
    `persist-credentials: false` on checkout unless the job pushes, no
    `pull_request_target`, never print secrets. Pin runner labels
    (`ubuntu-26.04`, `ubuntu-26.04-arm`), never `*-latest`.
