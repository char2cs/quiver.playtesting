// Package gateway exposes the browser client and bridges websockets to VNC servers.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"quiver-playtesting/internal/core"
	"quiver-playtesting/internal/live"
	"quiver-playtesting/internal/rfb"
	"quiver-playtesting/web"
)

const (
	maxTokenLen   = 128
	resolveWait   = 5 * time.Second
	pingEvery     = 20 * time.Second
	pingTimeout   = 10 * time.Second
	logWait       = 5 * time.Second
	copyBufSize   = 32 * 1024
	defaultConns  = 100
	defaultIdle   = 5 * time.Minute
	defaultMaxSes = 4 * time.Hour
	baseCSP       = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
	staticCSP     = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
	genericClose  = "unavailable"
	reasonClosed  = "closed"
	reasonIdle    = "idle"
	reasonVMError = "vm_error"
	reasonBadRFB  = "bad_client"
	reasonMaxSes  = "max_session"
	reasonExpired = "expired"
)

type Config struct {
	Listen       string
	PublicHost   string
	RealIPHeader string
	MaxConns     int
	IdleTimeout  time.Duration
	MaxSession   time.Duration
	// TrustedProxies are the only TCP peers whose RealIPHeader is believed.
	TrustedProxies []netip.Prefix
	MaxOpenConns   int
	PerPeerConns   int
	// ContinuousUpdates makes the gateway serve the RFB ContinuousUpdates extension itself;
	// off falls back to copying bytes untouched.
	ContinuousUpdates bool
}

type Backend interface {
	Resolve(ctx context.Context, token string) (core.Link, core.VM, error)
	LogSession(ctx context.Context, s core.Session, ended time.Time, reason string) error
}

type handler struct {
	cfg     Config
	b       Backend
	reg     *live.Registry
	lim     *limiter
	sem     chan struct{}
	static  fs.FS
	dialVM  func(ctx context.Context, addr, password string) (net.Conn, error)
	pingInt time.Duration

	mu      sync.Mutex
	closing bool
	wg      sync.WaitGroup
}

func newHandler(cfg Config, b Backend, reg *live.Registry) *handler {
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = defaultConns
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = defaultIdle
	}
	if cfg.MaxSession <= 0 {
		cfg.MaxSession = defaultMaxSes
	}
	static, err := fs.Sub(web.FS, "static")
	if err != nil {
		panic(err)
	}
	return &handler{
		cfg:     cfg,
		b:       b,
		reg:     reg,
		lim:     newLimiter(),
		sem:     make(chan struct{}, cfg.MaxConns),
		static:  static,
		dialVM:  rfb.Dial,
		pingInt: pingEvery,
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if v := recover(); v != nil {
			if v == http.ErrAbortHandler {
				panic(v)
			}
			slog.Error("handler panic", "panic", v)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}()
	hd := w.Header()
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Referrer-Policy", "no-referrer")
	hd.Set("X-Frame-Options", "DENY")
	hd.Set("Content-Security-Policy", baseCSP)
	hd.Set("Cross-Origin-Opener-Policy", "same-origin")
	hd.Set("Cross-Origin-Resource-Policy", "same-origin")
	hd.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
	hd.Set("Cache-Control", "no-store")
	static := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch {
	case static && strings.HasPrefix(r.URL.Path, "/s/"):
		if !validToken(r.URL.Path[3:]) {
			notFound(w)
			return
		}
		h.serveIndex(w, r)
	case static && strings.HasPrefix(r.URL.Path, "/assets/"):
		h.serveAsset(w, r, r.URL.Path[1:])
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/ws/"):
		token := r.URL.Path[4:]
		if !validToken(token) {
			h.lim.fail(limiterKey(h.clientIP(r)))
			notFound(w)
			return
		}
		h.serveWS(w, r, token)
	default:
		notFound(w)
	}
}

func validToken(t string) bool {
	if t == "" || len(t) > maxTokenLen {
		return false
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func notFound(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
}

func (h *handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	f, err := h.static.Open("index.html")
	if err != nil {
		notFound(w)
		return
	}
	defer f.Close()
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		notFound(w)
		return
	}
	hd := w.Header()
	hd.Set("Content-Security-Policy", staticCSP)
	hd.Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", time.Time{}, rs)
}

func (h *handler) serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	rel := strings.TrimPrefix(name, "assets/")
	if rel == "" || strings.HasSuffix(rel, "/") || !fs.ValidPath("assets/"+rel) {
		notFound(w)
		return
	}
	f, err := h.static.Open("assets/" + rel)
	if err != nil {
		notFound(w)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	rs, ok := f.(io.ReadSeeker)
	if err != nil || st.IsDir() || !ok {
		notFound(w)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, rel, time.Time{}, rs)
}

// clientIP trusts RealIPHeader only when the TCP peer is a configured proxy,
// otherwise any direct client could pick its own identity.
func (h *handler) clientIP(r *http.Request) string {
	peer := peerAddr(r.RemoteAddr)
	a := peer
	if h.cfg.RealIPHeader != "" && trusted(h.cfg.TrustedProxies, peer) {
		if v := r.Header.Get(h.cfg.RealIPHeader); v != "" {
			if p, err := netip.ParseAddr(strings.TrimSpace(v[strings.LastIndexByte(v, ',')+1:])); err == nil {
				a = p.WithZone("").Unmap()
			}
		}
	}
	if !a.IsValid() {
		return "unknown"
	}
	return a.String()
}

func limiterKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil || a.Is4() {
		return ip
	}
	p, _ := a.Prefix(64)
	return p.String()
}

func (h *handler) originOK(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" || len(o) > 256 {
		return false
	}
	u, err := url.Parse(o)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	want := h.cfg.PublicHost
	if want == "" {
		want = r.Host
	}
	return strings.EqualFold(u.Host, want)
}

func newSessionID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (h *handler) serveWS(w http.ResponseWriter, r *http.Request, token string) {
	ip := h.clientIP(r)
	key := limiterKey(ip)
	if h.lim.banned(key) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if !h.originOK(r) {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	rctx, rcancel := context.WithTimeout(r.Context(), resolveWait)
	link, vm, err := h.b.Resolve(rctx, token)
	rcancel()
	if err != nil || !link.Active(time.Now()) {
		h.lim.fail(key)
		notFound(w)
		return
	}

	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	h.wg.Add(1)
	h.mu.Unlock()
	defer h.wg.Done()

	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	sess := core.Session{
		ID: newSessionID(), LinkID: link.ID, Label: link.Label, VMID: vm.ID,
		VMName: vm.Name, ClientIP: ip, StartedAt: time.Now(),
	}
	if err := h.reg.Start(sess, cancel); err != nil {
		notFound(w)
		return
	}
	reason := reasonClosed
	defer func() {
		h.reg.End(sess.ID, reason)
		lctx, lcancel := context.WithTimeout(context.Background(), logWait)
		defer lcancel()
		if err := h.b.LogSession(lctx, sess, time.Now(), reason); err != nil {
			slog.Warn("log session failed", "session", sess.ID)
		}
		slog.Info("session ended", "session", sess.ID, "reason", reason)
	}()

	// A revoke between Resolve and Start would not have found this session to kill.
	rctx, rcancel = context.WithTimeout(context.Background(), resolveWait)
	_, _, err = h.b.Resolve(rctx, token)
	rcancel()
	if err != nil {
		reason = "revoked"
		notFound(w)
		return
	}

	// Origin was verified above, so the library's own same-host check is skipped.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:       []string{"binary"},
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		reason = "upgrade_failed"
		return
	}
	slog.Info("session started", "session", sess.ID)

	vmConn, err := h.dialVM(ctx, net.JoinHostPort(vm.Host, strconv.Itoa(vm.Port)), vm.Password)
	if err != nil {
		reason = reasonVMError
		closeWS(ws, websocket.StatusInternalError, genericClose)
		return
	}
	nc := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	if err := rfb.Accept(nc); err != nil {
		reason = reasonBadRFB
		vmConn.Close()
		closeWS(ws, websocket.StatusPolicyViolation, genericClose)
		return
	}
	limit, limitReason := h.cfg.MaxSession, reasonMaxSes
	if left := time.Until(link.ExpiresAt); left < limit {
		limit, limitReason = max(left, time.Second), reasonExpired
	}
	reason = h.bridge(ctx, cancel, ws, nc, vmConn, limit, limitReason)
}

// Close waits for the peer's close frame, which the library bounds to a few seconds.
func closeWS(ws *websocket.Conn, code websocket.StatusCode, msg string) {
	ws.Close(code, msg)
}

// activity tracks the last traffic on the monotonic clock, so wall clock jumps cannot fake or hide idleness.
type activity struct {
	base time.Time
	last atomic.Int64
}

func (a *activity) touch()                 { a.last.Store(int64(time.Since(a.base))) }
func (a *activity) idleFor() time.Duration { return time.Since(a.base) - time.Duration(a.last.Load()) }

type touchConn struct {
	net.Conn
	act *activity
}

func (c touchConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.act.touch()
	}
	return n, err
}

func (c touchConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.act.touch()
	}
	return n, err
}

func goSafe(f func()) {
	go func() {
		defer func() {
			if v := recover(); v != nil {
				slog.Error("goroutine panic", "panic", v)
			}
		}()
		f()
	}()
}

func (h *handler) bridge(ctx context.Context, cancel context.CancelCauseFunc, ws *websocket.Conn, nc, vm net.Conn, limit time.Duration, limitReason string) string {
	act := &activity{base: time.Now()}
	a, b := touchConn{nc, act}, touchConn{vm, act}

	var wg sync.WaitGroup
	if h.cfg.ContinuousUpdates {
		wg.Add(1)
		goSafe(func() {
			defer wg.Done()
			defer cancel(errors.New(reasonClosed))
			if err := rfb.Relay(ctx, a, b, rfb.RelayOptions{PumpWriter: vm}); errors.Is(err, rfb.ErrProtocol) {
				cancel(errors.New(reasonBadRFB))
			}
		})
	} else {
		cp := func(dst, src net.Conn) {
			defer wg.Done()
			defer cancel(errors.New(reasonClosed))
			io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{src}, make([]byte, copyBufSize))
		}
		wg.Add(2)
		goSafe(func() { cp(b, a) })
		goSafe(func() { cp(a, b) })
	}
	wg.Add(1)
	goSafe(func() {
		defer wg.Done()
		defer cancel(errors.New(reasonClosed))
		idle := h.cfg.IdleTimeout
		tick := time.NewTicker(max(idle/4, time.Millisecond))
		defer tick.Stop()
		ping := time.NewTicker(h.pingInt)
		defer ping.Stop()
		deadline := time.NewTimer(limit)
		defer deadline.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-deadline.C:
				cancel(errors.New(limitReason))
				return
			case <-tick.C:
				if act.idleFor() > idle {
					cancel(errors.New(reasonIdle))
					return
				}
			case <-ping.C:
				pctx, pc := context.WithTimeout(ctx, pingTimeout)
				err := ws.Ping(pctx)
				pc()
				if err != nil && ctx.Err() == nil {
					cancel(errors.New("ping_timeout"))
					return
				}
			}
		}
	})

	<-ctx.Done()
	vm.Close()
	closeWS(ws, websocket.StatusNormalClosure, "")
	wg.Wait()
	cause := context.Cause(ctx)
	if cause == nil {
		return reasonClosed
	}
	return cause.Error()
}
