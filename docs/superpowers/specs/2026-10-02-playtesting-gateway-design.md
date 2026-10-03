# Quiver Playtesting Gateway: Design

## Goal

Playtesters receive a single link (`https://playtesting.quiver.ar/s/<token>`). It opens a noVNC client showing a VM where they run a playtest session. The operator monitors and controls sessions from a TUI on the server. VNC ports are never exposed. Exactly one port (a high port, plain HTTP) is reachable from outside, fronted by Cloudflare, which the operator configures and owns.

## Non-goals

- No web admin UI, no admin auth (shell access to the server is the credential).
- No VM pool or auto-assignment: every link is bound to one VM.
- No TLS, Cloudflare IP/secret-header gating or WAF logic in the gateway. It expects plain HTTP requests.
- No clipboard or file transfer.

## Architecture

One Go module, one binary `quiver-playtest`, two subcommands:

- `serve`: daemon. Public HTTP listener on a configurable high port plus a Unix admin socket (0600).
- `tui`: Bubble Tea client that talks to the daemon over the Unix socket. Closing it does not affect live sessions.

### Gateway (public)

- `GET /s/<token>`: serves the embedded noVNC page (go:embed, vendored noVNC).
- `GET /ws/<token>`: validates the token, upgrades to WebSocket, opens a TCP connection to the bound VM's VNC port, and bridges bytes.
- The gateway performs the RFB handshake with the VM itself (VNC auth with the stored password) and presents security type "None" to the browser. The VNC password never leaves the server.
- Client IP is read from a configurable header (default `CF-Connecting-IP`), falling back to the socket peer.

### Rules and hardening

- One live session per link and per VM; a second connect is rejected.
- Links expire (default 4h) and can be revoked.
- Tokens: 256-bit random, URL-safe, stored as SHA-256 hashes, compared in constant time.
- Per-IP rate limit on failed token lookups with a short ban.
- Global concurrent connection cap, idle timeout, WebSocket frame size limit, Origin check against the configured public host.
- `Referrer-Policy: no-referrer`, `Cache-Control: no-store`.
- Unknown token, expired, revoked, and VM-down all return the same generic response.
- The gateway only connects to VMs registered in the store. No user-supplied hosts (no SSRF).

### Storage

SQLite (pure-Go driver), file mode 0600.

- `vms(id, name, host, port, password, created_at)`
- `links(id, token_hash, vm_id, label, expires_at, revoked, flagged, created_at)`
- `session_log(id, link_id, vm_id, client_ip, started_at, ended_at, end_reason)`

Live sessions are held in memory and written to `session_log` on end.

### Admin protocol

Line-delimited JSON over the Unix socket. Requests: list/add/remove/test VM, create/revoke/list link, kill session, flag session. A subscribe stream pushes session start/end events so the TUI updates in real time.

### TUI

Three tabs:

- **Sessions**: live list (link label, VM, IP, duration). Keys: kill, flag, drop (kill + revoke link).
- **VMs**: list, add form (name, host, port, password), remove, test connection.
- **Links**: list, create (pick VM, label, TTL), copy URL, revoke.

Flag marks the session's link and logs it. Drop kills the session and optionally revokes the link.

## Configuration

Flags or env: `--listen :PORT` (high port), `--public-host playtesting.quiver.ar`, `--data ./data`, `--real-ip-header CF-Connecting-IP`, `--trusted-proxies cloudflare|none|CIDRs`, `--max-session`, `--log-retention`, `--max-conns`, `--link-ttl`.

## Testing

- Token generation, hashing, expiry, revoke.
- RFB handshake against a fake VNC server (VNC auth, then None to the client).
- Kill and revoke closing a live bridge.
- One-session-per-link/VM enforcement.
- Rate limiter and ban.
- Admin socket protocol round trips.

## Layout

```
quiver-playtesting/
  cmd/quiver-playtest/    main, subcommands
  internal/gateway/       http, ws bridge, rfb
  internal/store/         sqlite
  internal/admin/         socket server + protocol
  internal/tui/           bubble tea
  web/                    embedded noVNC page
```
