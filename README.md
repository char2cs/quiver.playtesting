# quiver-playtesting

<p align="center">
  <img src="https://raw.githubusercontent.com/rabbytesoftware/quiver.core/develop/.github/quiver.svg" alt="Quiver" width="450" />
</p>

Playtesters open `https://playtesting.quiver.ar/s/<token>` and get a noVNC view of one VM. VNC ports are never exposed: the gateway bridges a WebSocket to the VM and does the VNC auth itself. The operator manages everything from a TUI on the server.

```
go build -o bin/quiver-playtest ./cmd/quiver-playtest
./bin/quiver-playtest serve --listen :8480 --public-host playtesting.quiver.ar   # expose only this port
./bin/quiver-playtest tui   --public-url https://playtesting.quiver.ar           # same --data as serve
```

Admin runs over a 0600 Unix socket in `--data`; there is no web admin. TLS and Cloudflare are handled in front; the gateway speaks plain HTTP. HSTS is a Cloudflare setting (turn it on there), the origin cannot send it over plain HTTP.

## Security model and flags

- Client IP: `--real-ip-header` (default `CF-Connecting-IP`) is believed only when the TCP peer is in `--trusted-proxies`. Default `cloudflare` (Cloudflare's published ranges, embedded, copied 2026-10-03). Use a comma list of CIDRs/IPs (the word `cloudflare` expands in place) or `none` to always use the socket peer. Loopback and private ranges are not trusted unless listed. Rate limits and bans key on the verified IP, so a direct hit on the origin port cannot spoof or frame anyone.
- `--max-session` (default 4h) hard cap per session; a session also ends when its link expires. `--idle-timeout` (default 15m), `--max-conns` (default 50).
- Open sockets are capped (2048 total, 64 per direct peer; trusted proxies only count toward the total). Slow headers are cut after 10s.
- `--log-retention` (default 90d) prunes `session_log` daily, `0` keeps everything.
- VM targets come only from the store. Literal IPs that are unspecified, link-local (169.254.0.0/16, fe80::/10, so cloud metadata), multicast or broadcast are rejected at add time, and every connection is re-checked on the resolved address, so DNS rebinding cannot reach them. RFC1918 and loopback are allowed.
- VNC passwords are stored in plaintext in SQLite. File permissions are the boundary: the data dir is forced to 0700 and the db/WAL/socket are 0600. An encryption key file in the same directory would add no protection against anyone who can read the db. VNC auth itself is DES, mandated by the protocol, so treat VM passwords as LAN-grade.
- The link token lives in the URL path, which is how a link works at all. The gateway never logs paths, sends `Referrer-Policy: no-referrer`, `Cache-Control: no-store` and sets no cookies. Cloudflare and any other proxy in front will still see the path in its own logs. A one-time ws ticket would not change that because the page URL carries the token too, so it is not implemented.
- SIGTERM/SIGINT ends every session (reason `shutdown`), logs them and exits 0. Logs go to stderr.

Operator checklist: forward only the gateway port; ideally allow inbound on it only from Cloudflare ranges at the router/firewall; keep Cloudflare proxy (orange cloud) on; add a Cloudflare rate limiting rule on `/ws/*`; never run with `--trusted-proxies` set to ranges you do not control; keep `--data` on a disk only the service user can read.

Local end to end test VM: see `testenv/README.md`. Design: `docs/superpowers/specs/`.

## Deploy as a Quiver arrow

`ARROW.md` is an `arrow@v0` manifest (banner, explanation and manifest in one file, like the Quiver arrows) (validated with quiver.core's parser). It installs the release binary for `linux/amd64` or `linux/arm64` and runs `serve` as a service.

- Variables: `PUBLIC_HOST`, `REAL_IP_HEADER`, `MAX_SESSIONS`, `IDLE_TIMEOUT`. Netbridge allocates `GATEWAY_PORT` (default 8480), the only port to expose.
- State lives in `${INSTALL_PATH}/data` (database and admin socket). Uninstall keeps it.
- Operate it from the host shell: `cd <install path> && ./quiver-playtest tui --data ./data`.
- Releases: pushing a tag `vX.Y.Z` runs `.github/workflows/release.yml`, which publishes tarballs. Until the first stable tag exists, the arrow downloads the rolling `nightly` build; switch its URLs and `version` to the tag once released.

## Nightly builds

Every push to `main` runs `.github/workflows/nightly.yml`: vet, race tests, then it replaces the `nightly` prerelease with fresh `linux/amd64` and `linux/arm64` tarballs and `checksums.txt`. Asset names are stable (`quiver-playtest_nightly_linux_<arch>.tar.gz`), so a fixed URL always serves the latest main. Stable releases still come from `vX.Y.Z` tags.
