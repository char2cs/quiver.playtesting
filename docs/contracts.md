# Package contracts (build-time, authoritative)

Module: `quiver-playtesting`. Go 1.26. Deps already in go.mod: modernc.org/sqlite, github.com/coder/websocket, charm.land/bubbletea/v2, charm.land/bubbles/v2, charm.land/lipgloss/v2. Shared types/interfaces: `internal/core/core.go` (do not change without telling the orchestrator).
Style: simple, minimal comments (only non-obvious WHY), no em/en dashes anywhere. Security first. Each package ships its own tests (`go test ./...` must pass, `go vet` clean).

## internal/store  (Store: core.Store)
`store.Open(path string) (*store.Store, error)` SQLite via modernc (driver name "sqlite"), creates schema, WAL, foreign keys on, file mode 0600, `SetMaxOpenConns` sane. Tables vms, links (token_hash BLOB UNIQUE, 32 bytes), session_log. RemoveVM revokes its links then deletes the VM (keep links rows, vm_name resolved via join; if VM deleted show name from a snapshot column `vm_name`). All queries parameterized.

## internal/live  (live session registry)
```go
func New() *Registry
// Start registers a session. Returns core.ErrBusy if the link OR the VM already has a live session.
func (r *Registry) Start(s core.Session, cancel context.CancelCauseFunc) error
func (r *Registry) End(id string, reason string)            // idempotent; emits "end" event
func (r *Registry) Kill(id, reason string) error            // cancel(errors.New(reason)); core.ErrNotFound if unknown
func (r *Registry) KillByLink(linkID int64, reason string)
func (r *Registry) KillByVM(vmID int64, reason string)
func (r *Registry) List() []core.Session
func (r *Registry) Get(id string) (core.Session, bool)
func (r *Registry) Subscribe() (<-chan core.Event, func()) // buffered, never blocks the registry (drop on slow consumer)
```
Start emits a "start" event. Safe for concurrent use.

## internal/rfb
```go
// Dial connects to addr, performs the RFB 3.8 client handshake using VNC authentication
// (DES challenge/response, VNC bit-reversed key) with password, and returns the conn positioned
// right before ClientInit. Respects ctx and a handshake deadline (10s). Refuses banned target IPs (unspecified, link-local, multicast, broadcast) on the resolved address via `rfb.CheckIP`. Answers 3.7 servers with 3.7. Rejects servers that do not offer VNC auth
// (security type 2) unless password == "" and type 1 (None) is offered. Never logs the password.
func Dial(ctx context.Context, addr, password string) (net.Conn, error)
// Accept runs the server side of the handshake against an untrusted client (the browser): sends "RFB 003.008\n",
// requires a 3.8 client version, offers only security type None, replies SecurityResult OK. Handshake deadline 10s, strict reads.
func Accept(conn net.Conn) error
```
After both succeed, the gateway copies bytes both ways (ClientInit/ServerInit and everything after pass through).

## internal/service  (implements core.Service + gateway.Backend)
```go
func New(st core.Store, reg *live.Registry) *Service
func (s *Service) Resolve(ctx context.Context, token string) (core.Link, core.VM, error) // core.ErrNotFound for unknown/expired/revoked/missing VM
func (s *Service) LogSession(ctx context.Context, sess core.Session, ended time.Time, reason string) error
```
Tokens: 32 bytes crypto/rand, base64.RawURLEncoding; store sha256(token) ; Resolve hashes the presented token and looks up (constant-time compare not needed on a hash lookup, but do not early-return on length differences observably: just hash whatever is given, cap input length at 128 chars first). TestVM: rfb.Dial then close. CreateLink ttl bounds: 1 minute..30 days. AddVM validation: name 1..64 printable, host is a DNS name or IP (no scheme, no path, no whitespace), port 1..65535, password <= 8 bytes is VNC's real limit (reject longer: VNC auth only uses the first 8). Registry kills on RemoveVM/RevokeLink/Kill(revoke=true).

## internal/gateway
```go
type Config struct {
    Listen, PublicHost, RealIPHeader string
    TrustedProxies []netip.Prefix // only these TCP peers may set RealIPHeader; nil trusts nobody
    MaxConns int; IdleTimeout, MaxSession time.Duration; MaxOpenConns, PerPeerConns int
}
type Backend interface {
    Resolve(ctx context.Context, token string) (core.Link, core.VM, error)
    LogSession(ctx context.Context, s core.Session, ended time.Time, reason string) error
}
func NewServer(cfg Config, b Backend, reg *live.Registry) *Server // *Server embeds *http.Server; Shutdown also kills and logs live sessions; timeouts set (ReadHeaderTimeout, ReadTimeout, WriteTimeout where compatible with ws, IdleTimeout, MaxHeaderBytes)
```
Routes (anything else: 404 with empty generic body):
- `GET /s/{token}` -> serves web.FS `static/index.html` (token is NOT validated here, so page existence leaks nothing; page then opens the ws). Headers: CSP (`default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'`), `Referrer-Policy: no-referrer`, `Cache-Control: no-store`, `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`.
- `GET /assets/*` -> `web.FS` `static/assets/*`, long cache ok, same nosniff.
- `GET /ws/{token}` -> Origin must equal PublicHost (host match; if PublicHost empty allow same-host only), Resolve token (all failures: identical generic 404), per-IP failed-attempt rate limit with temporary ban (e.g. 10 fails/min -> 5 min ban, 429), live.Start (busy -> same generic 404), rfb.Dial to VM (failure -> close ws with generic code, log reason vm_error), websocket accept with subprotocol "binary" tolerated, read limit, rfb.Accept on a net.Conn adapter over the ws (websocket.NetConn), then bidirectional copy. Idle timeout (no bytes either way for cfg.IdleTimeout) ends the session. Ping the client every 20s. Global cap MaxConns via semaphore (503). Session ends on ctx cancel (kill): close both ends, `reg.End(id, reason)`, `LogSession`. Real client IP from cfg.RealIPHeader only when the TCP peer is in cfg.TrustedProxies (and the header parses as an IP), else RemoteAddr host. Sessions also end at MaxSession and at link expiry. Tokens are base64url, at most 128 chars, anything else is the generic 404 and counts as a failure.
- A dedicated gateway package test uses a fake VNC server (can reuse a helper in internal/rfb test util `rfbtest`: `rfbtest.Server(t, password) (addr string)`, a minimal RFB 3.8 server with VNC auth that echoes after ServerInit; put it in `internal/rfb/rfbtest` as a normal (non _test) package so gateway tests can import it).

## web  (package web)
`web/static/index.html` + `web/static/assets/*`; `package web; //go:embed all:static; var FS embed.FS`. Page: full-viewport noVNC (vendor noVNC's `core/` + `vendor/` ES modules into assets/novnc/, pinned version, with its LICENSE), no inline scripts, no external requests. `assets/app.js` derives ws URL from `location`: replace the leading `/s/` with `/ws/`, ws:// or wss:// per page protocol. Shows small status overlay (connecting / connected / ended or invalid link: generic text "This session is not available."). View+input only, `clipboard` and file transfer not wired, `scaleViewport = true`, `resizeSession = false`. Disable noVNC credentials prompts (the gateway offers no auth).

## internal/admin
```go
func Serve(ctx context.Context, socketPath string, svc core.Service) error   // unix socket 0600 (umask-safe), removes stale socket, refuses if another live daemon owns it; verifies peer uid == own uid (SO_PEERCRED/LOCAL_PEERCRED or at least dir 0700), line-delimited JSON, max line 64KiB, per-conn read deadline except for subscribe
func Dial(socketPath string) (*Client, error)  // *Client implements core.Service (Subscribe opens its own stream connection); Client.Close()
```
Protocol is the implementer's choice but must be documented in package doc. Never serialize VM passwords back to clients (core.VM already hides them; AddVM request carries it one way).

## internal/tui
`tui.Run(svc core.Service) error` Bubble Tea v2 (charm.land/bubbletea/v2). Tabs: Sessions | VMs | Links, key hints footer. Sessions: live table fed by Subscribe + periodic ListSessions refresh, `k` kill, `f` flag, `d` drop (kill + revoke). VMs: list, `a` add form (name, host, port, password masked), `t` test (shows result), `x` remove (confirm). Links: list with status (active/expired/revoked/flagged), `n` create (VM picker, label, TTL like 4h), after creating show the full URL once with `c` copying it (OSC52 / clipboard if available) , `r` revoke (confirm). URL base comes from `tui.Run` option `PublicURL string` (e.g. https://playtesting.quiver.ar): signature `tui.Run(svc core.Service, publicURL string) error`. Plus tui tests for model update logic where cheap.

## cmd/quiver-playtest  (orchestrator writes this)
