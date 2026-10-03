# quiver-playtesting

Playtesters open `https://playtesting.quiver.ar/s/<token>` and get a noVNC view of one VM. VNC ports are never exposed: the gateway bridges a WebSocket to the VM and does the VNC auth itself. The operator manages everything from a TUI on the server.

```
go build -o bin/quiver-playtest ./cmd/quiver-playtest
./bin/quiver-playtest serve --listen :8480 --public-host playtesting.quiver.ar   # expose only this port
./bin/quiver-playtest tui   --public-url https://playtesting.quiver.ar           # same --data as serve
```

Admin runs over a 0600 Unix socket in `--data`; there is no web admin. TLS and Cloudflare are handled in front; the gateway speaks plain HTTP and reads the client IP from `--real-ip-header` (default `CF-Connecting-IP`).

Local end to end test VM: see `testenv/README.md`. Design: `docs/superpowers/specs/`.

## Deploy as a Quiver arrow

`arrow.yaml` is an `arrow@v0` manifest (validated with quiver.core's parser). It installs the release binary for `linux/amd64` or `linux/arm64` and runs `serve` as a service.

- Variables: `PUBLIC_HOST`, `REAL_IP_HEADER`, `MAX_SESSIONS`, `IDLE_TIMEOUT`. Netbridge allocates `GATEWAY_PORT` (default 8480), the only port to expose.
- State lives in `${INSTALL_PATH}/data` (database and admin socket). Uninstall keeps it.
- Operate it from the host shell: `cd <install path> && ./quiver-playtest tui --data ./data`.
- Releases: pushing a tag `vX.Y.Z` runs `.github/workflows/release.yml`, which publishes the tarballs the arrow downloads. Bump the URLs and `version` in `arrow.yaml` to match the tag. The repository must be public for the arrow to download release assets without credentials.
