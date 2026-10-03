package gateway

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"quiver-playtesting/internal/core"
	"quiver-playtesting/internal/live"
	"quiver-playtesting/internal/rfb/rfbtest"
)

func TestClientIPTrust(t *testing.T) {
	cf := CloudflareProxies()
	h := &handler{cfg: Config{RealIPHeader: "CF-Connecting-IP", TrustedProxies: cf}}
	get := func(h *handler, remote, hdr string) string {
		r, _ := http.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		if hdr != "" {
			r.Header.Set("CF-Connecting-IP", hdr)
		}
		return h.clientIP(r)
	}
	if got := get(h, "173.245.48.5:443", "198.51.100.7"); got != "198.51.100.7" {
		t.Fatalf("trusted peer: %s", got)
	}
	if got := get(h, "[2606:4700::1]:443", "198.51.100.7"); got != "198.51.100.7" {
		t.Fatalf("trusted v6 peer: %s", got)
	}
	if got := get(h, "203.0.113.50:1234", "198.51.100.7"); got != "203.0.113.50" {
		t.Fatalf("direct client spoofed identity: %s", got)
	}
	if got := get(h, "127.0.0.1:1234", "198.51.100.7"); got != "127.0.0.1" {
		t.Fatalf("loopback must not be trusted by default: %s", got)
	}
	if got := get(h, "10.0.0.2:1234", "198.51.100.7"); got != "10.0.0.2" {
		t.Fatalf("private must not be trusted by default: %s", got)
	}
	if got := get(h, "173.245.48.5:443", "garbage"); got != "173.245.48.5" {
		t.Fatalf("bad header from trusted peer: %s", got)
	}
	if got := get(h, "173.245.48.5:443", "fe80::1%eth0"); got != "fe80::1" {
		t.Fatalf("zone must be stripped: %s", got)
	}
	none := &handler{cfg: Config{RealIPHeader: "CF-Connecting-IP"}}
	if got := get(none, "173.245.48.5:443", "198.51.100.7"); got != "173.245.48.5" {
		t.Fatalf("none must ignore header: %s", got)
	}
}

func TestParseTrustedProxies(t *testing.T) {
	d, err := ParseTrustedProxies("")
	if err != nil || len(d) != len(cloudflareRanges) {
		t.Fatalf("default: %v %v", d, err)
	}
	if n, err := ParseTrustedProxies("none"); err != nil || len(n) != 0 {
		t.Fatalf("none: %v %v", n, err)
	}
	l, err := ParseTrustedProxies("10.0.0.0/8, 192.168.1.1, cloudflare")
	if err != nil || len(l) != 2+len(cloudflareRanges) || !trusted(l, netip.MustParseAddr("192.168.1.1")) || trusted(l, netip.MustParseAddr("192.168.1.2")) {
		t.Fatalf("list: %v %v", l, err)
	}
	if _, err := ParseTrustedProxies("nonsense"); err == nil {
		t.Fatal("expected error")
	}
	if !trusted(d, netip.MustParseAddr("104.16.0.1")) || trusted(d, netip.MustParseAddr("8.8.8.8")) {
		t.Fatal("cloudflare membership wrong")
	}
}

func TestSpoofedHeaderCannotEvadeOrFrame(t *testing.T) {
	e := setup(t, Config{RealIPHeader: "CF-Connecting-IP"})
	for i := 0; i < maxFails; i++ {
		e.plain(t, "/ws/bad", map[string]string{"CF-Connecting-IP": "203.0.113." + string(rune('1'+i))})
	}
	if s := e.plain(t, "/ws/bad", map[string]string{"CF-Connecting-IP": "198.51.100.1"}); s.status != 429 {
		t.Fatalf("rotating the header evaded the ban: %d", s.status)
	}
	e2 := setup(t, Config{RealIPHeader: "CF-Connecting-IP"})
	e2.b.add("good", 1, rfbtest.Server(t, ""), "")
	for i := 0; i < maxFails+3; i++ {
		e2.plain(t, "/ws/bad", map[string]string{"CF-Connecting-IP": "192.0.2.77"})
	}
	e3 := setup(t, Config{RealIPHeader: "CF-Connecting-IP", TrustedProxies: loopback})
	if s := e3.plain(t, "/ws/bad", map[string]string{"CF-Connecting-IP": "192.0.2.77"}); s.status != 404 {
		t.Fatalf("unrelated ip framed: %d", s.status)
	}
}

func TestInvalidTokenFormatsCountAsFailures(t *testing.T) {
	e := setup(t, Config{})
	for i := 0; i < maxFails; i++ {
		e.plain(t, "/ws/bad%20token", nil)
	}
	if s := e.plain(t, "/ws/anything", nil); s.status != 429 {
		t.Fatalf("got %d", s.status)
	}
}

func TestHeadersOnEveryPath(t *testing.T) {
	e := setup(t, Config{})
	for _, p := range []string{"/", "/s/abc", "/s/", "/s/a%2Fb", "/assets/app.js", "/assets/", "/assets/nope", "/assets/../index.html", "/assets/novnc/core", "/ws/abc", "/x", "/ws/" + strings.Repeat("a", 500)} {
		for _, method := range []string{"GET", "HEAD", "POST"} {
			req, _ := http.NewRequest(method, e.srv.URL+p, nil)
			req.Header.Set("Origin", e.srv.URL)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			for _, k := range []string{"X-Content-Type-Options", "Referrer-Policy", "X-Frame-Options", "Content-Security-Policy"} {
				if resp.Header.Get(k) == "" {
					t.Errorf("%s %s: missing %s (status %d)", method, p, k, resp.StatusCode)
				}
			}
			if resp.Header.Get("Set-Cookie") != "" {
				t.Errorf("%s %s sets a cookie", method, p)
			}
			if p != "/assets/app.js" && resp.Header.Get("Cache-Control") != "no-store" {
				t.Errorf("%s %s: cache-control %q", method, p, resp.Header.Get("Cache-Control"))
			}
		}
	}
}

func TestStaticHandlerEdges(t *testing.T) {
	e := setup(t, Config{})
	cases := map[string]int{
		"/assets/":                     404,
		"/assets/novnc":                404,
		"/assets/novnc/":               404,
		"/assets/../static/index.html": 404,
		"/assets/%2e%2e/index.html":    404,
		"/assets/novnc/core/../rfb.js": 404,
		"/assets/app.js":               200,
		"/s/abcDEF123_-":               200,
		"/s/has.dot":                   404,
		"/s/":                          404,
		"/index.html":                  404,
	}
	for p, want := range cases {
		if s := e.plain(t, p, nil); s.status != want {
			t.Errorf("%q: got %d want %d", p, s.status, want)
		}
	}
	req, _ := http.NewRequest("HEAD", e.srv.URL+"/s/abc", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("HEAD: %v", err)
	}
	resp.Body.Close()
	s := e.plain(t, "/s/abc", nil)
	for _, want := range []string{"script-src 'self'", "frame-ancestors 'none'", "default-src 'none'", "base-uri 'none'"} {
		if !strings.Contains(s.header.Get("Content-Security-Policy"), want) {
			t.Errorf("csp missing %s", want)
		}
	}
	if strings.Contains(s.body, "<script>") {
		t.Error("inline script in index")
	}
}

type panicBackend struct{ *fakeBackend }

func (panicBackend) Resolve(context.Context, string) (core.Link, core.VM, error) { panic("boom") }

func TestHandlerPanicDoesNotKillServer(t *testing.T) {
	e := setup(t, Config{})
	e.h.b = panicBackend{e.b}
	if s := e.plain(t, "/ws/tok", nil); s.status != 500 {
		t.Fatalf("got %d", s.status)
	}
	if s := e.plain(t, "/assets/app.js", nil); s.status != 200 {
		t.Fatalf("server died: %d", s.status)
	}
}

func TestMaxSession(t *testing.T) {
	e := setup(t, Config{MaxSession: 400 * time.Millisecond})
	e.b.add("tok1", 1, rfbtest.Server(t, ""), "")
	ws, _, err := e.dial(t, "tok1")
	if err != nil {
		t.Fatal(err)
	}
	nc := handshake(t, ws)
	nc.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := nc.Read(make([]byte, 1)); err == nil {
		t.Fatal("session should have been cut")
	}
	if r := waitLog(t, e.b); r != "max_session" {
		t.Fatalf("reason %q", r)
	}
}

func TestSessionEndsAtLinkExpiry(t *testing.T) {
	e := setup(t, Config{})
	e.b.add("tok1", 1, rfbtest.Server(t, ""), "")
	l := e.b.links["tok1"]
	l.ExpiresAt = time.Now().Add(1500 * time.Millisecond)
	e.b.links["tok1"] = l
	ws, _, err := e.dial(t, "tok1")
	if err != nil {
		t.Fatal(err)
	}
	nc := handshake(t, ws)
	nc.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := nc.Read(make([]byte, 1)); err == nil {
		t.Fatal("session should have been cut")
	}
	if r := waitLog(t, e.b); r != "expired" {
		t.Fatalf("reason %q", r)
	}
}

func TestShutdownKillsAndLogsSessions(t *testing.T) {
	b := newBackend()
	reg := live.New()
	b.add("tok1", 1, rfbtest.Server(t, ""), "")
	srv := NewServer(Config{TrustedProxies: loopback}, b, reg)
	ln := listen(t)
	go srv.Serve(ln)
	ws := dialWS(t, ln.Addr().String(), "tok1")
	nc := handshake(t, ws)
	waitSessions(t, reg, 1)
	go io.Copy(io.Discard, nc)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if r := waitLog(t, b); r != "shutdown" {
		t.Fatalf("reason %q", r)
	}
	if len(reg.List()) != 0 {
		t.Fatal("sessions left")
	}
}

func TestPerPeerConnectionCap(t *testing.T) {
	srv := NewServer(Config{PerPeerConns: 3}, newBackend(), live.New())
	ln := listen(t)
	go srv.Serve(ln)
	defer srv.Close()
	conns := dialMany(t, ln.Addr().String(), 8)
	defer closeAll(conns)
	open := 0
	for _, c := range conns {
		c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		if _, err := c.Read(make([]byte, 1)); err != nil && !isTimeout(err) {
			continue
		}
		open++
	}
	if open != 3 {
		t.Fatalf("open peers %d want 3", open)
	}
}

func TestTrustedPeerNotPerPeerCapped(t *testing.T) {
	srv := NewServer(Config{PerPeerConns: 2, TrustedProxies: loopback}, newBackend(), live.New())
	ln := listen(t)
	go srv.Serve(ln)
	defer srv.Close()
	conns := dialMany(t, ln.Addr().String(), 6)
	defer closeAll(conns)
	for _, c := range conns {
		c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		if _, err := c.Read(make([]byte, 1)); err != nil && !isTimeout(err) {
			t.Fatal("trusted proxy connection dropped")
		}
	}
}
