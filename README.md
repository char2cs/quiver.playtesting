# quiver-playtesting

Playtesters open `https://playtesting.quiver.ar/s/<token>` and get a noVNC view of one VM. VNC ports are never exposed: the gateway bridges a WebSocket to the VM and does the VNC auth itself. The operator manages everything from a TUI on the server.

```
go build -o bin/quiver-playtest ./cmd/quiver-playtest
./bin/quiver-playtest serve --listen :8480 --public-host playtesting.quiver.ar   # expose only this port
./bin/quiver-playtest tui   --public-url https://playtesting.quiver.ar           # same --data as serve
```

Admin runs over a 0600 Unix socket in `--data`; there is no web admin. TLS and Cloudflare are handled in front; the gateway speaks plain HTTP and reads the client IP from `--real-ip-header` (default `CF-Connecting-IP`).

Local end to end test VM: see `testenv/README.md`. Design: `docs/superpowers/specs/`.
