package gateway

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"quiver-playtesting/internal/live"
)

const (
	defaultMaxOpen = 2048
	defaultPerPeer = 64
)

// Server is an http.Server whose Shutdown also ends live sessions and waits for them to be logged.
type Server struct {
	*http.Server
	h  *handler
	ll *limitListener
}

func NewServer(cfg Config, b Backend, reg *live.Registry) *Server {
	h := newHandler(cfg, b, reg)
	return &Server{
		Server: &http.Server{
			Addr:              cfg.Listen,
			Handler:           h,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    16 << 10,
		},
		h: h,
	}
}

func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

func (s *Server) Serve(ln net.Listener) error {
	s.ll = newLimitListener(ln, s.h.cfg.MaxOpenConns, s.h.cfg.PerPeerConns, s.h.cfg.TrustedProxies)
	return s.Server.Serve(s.ll)
}

// Shutdown stops accepting, kills every session with reason "shutdown" and waits until they are logged or ctx ends.
func (s *Server) Shutdown(ctx context.Context) error {
	s.h.mu.Lock()
	s.h.closing = true
	s.h.mu.Unlock()
	err := s.Server.Shutdown(ctx)
	s.h.reg.KillAll("shutdown")
	done := make(chan struct{})
	go func() { s.h.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		if err == nil {
			err = ctx.Err()
		}
	}
	return err
}

// limitListener bounds open connections overall and per direct peer, so
// half-open floods cannot exhaust file descriptors. Trusted proxies only count toward the total.
type limitListener struct {
	net.Listener
	max, perPeer int
	trusted      []netip.Prefix
	mu           sync.Mutex
	total        int
	peers        map[netip.Addr]int
}

func newLimitListener(ln net.Listener, max, perPeer int, tr []netip.Prefix) *limitListener {
	if max <= 0 {
		max = defaultMaxOpen
	}
	if perPeer <= 0 {
		perPeer = defaultPerPeer
	}
	return &limitListener{Listener: ln, max: max, perPeer: perPeer, trusted: tr, peers: map[netip.Addr]int{}}
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		peer := peerAddr(c.RemoteAddr().String())
		counted := !trusted(l.trusted, peer)
		l.mu.Lock()
		if l.total >= l.max || (counted && l.peers[peer] >= l.perPeer) {
			l.mu.Unlock()
			c.Close()
			continue
		}
		l.total++
		if counted {
			l.peers[peer]++
		}
		l.mu.Unlock()
		return &limitedConn{Conn: c, l: l, peer: peer, counted: counted}, nil
	}
}

type limitedConn struct {
	net.Conn
	l       *limitListener
	peer    netip.Addr
	counted bool
	once    sync.Once
}

func (c *limitedConn) Close() error {
	c.once.Do(func() {
		c.l.mu.Lock()
		c.l.total--
		if c.counted {
			if c.l.peers[c.peer]--; c.l.peers[c.peer] <= 0 {
				delete(c.l.peers, c.peer)
			}
		}
		c.l.mu.Unlock()
	})
	return c.Conn.Close()
}
