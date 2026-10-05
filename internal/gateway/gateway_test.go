package gateway

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"quiver-playtesting/internal/core"
	"quiver-playtesting/internal/live"
	"quiver-playtesting/internal/rfb/rfbtest"
)

type fakeBackend struct {
	links   map[string]core.Link
	vms     map[int64]core.VM
	loggedC chan string
	lastMu  sync.Mutex
	last    core.Session
}

func (b *fakeBackend) lastSession() core.Session {
	b.lastMu.Lock()
	defer b.lastMu.Unlock()
	return b.last
}

func newBackend() *fakeBackend {
	return &fakeBackend{links: map[string]core.Link{}, vms: map[int64]core.VM{}, loggedC: make(chan string, 16)}
}

func (b *fakeBackend) add(token string, id int64, addr, password string) {
	host, p, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(p)
	b.links[token] = core.Link{ID: id, VMID: id, ExpiresAt: time.Now().Add(time.Hour)}
	b.vms[id] = core.VM{ID: id, Name: "vm" + p, Host: host, Port: port, Password: password}
}

func (b *fakeBackend) Resolve(_ context.Context, token string) (core.Link, core.VM, error) {
	l, ok := b.links[token]
	if !ok || !l.Active(time.Now()) {
		return core.Link{}, core.VM{}, core.ErrNotFound
	}
	return l, b.vms[l.VMID], nil
}

func (b *fakeBackend) LogSession(_ context.Context, s core.Session, _ time.Time, reason string) error {
	b.lastMu.Lock()
	b.last = s
	b.lastMu.Unlock()
	b.loggedC <- reason
	return nil
}

type env struct {
	srv *httptest.Server
	h   *handler
	b   *fakeBackend
	reg *live.Registry
}

func setup(t *testing.T, cfg Config) *env {
	t.Helper()
	b := newBackend()
	reg := live.New()
	h := newHandler(cfg, b, reg)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &env{srv: srv, h: h, b: b, reg: reg}
}

func (e *env) dial(t *testing.T, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, "ws"+e.srv.URL[4:]+"/ws/"+token, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Origin": {e.srv.URL}},
		Subprotocols: []string{"binary"},
	})
}

func handshake(t *testing.T, ws *websocket.Conn) net.Conn {
	t.Helper()
	nc := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	nc.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 12)
	if _, err := io.ReadFull(nc, buf); err != nil || string(buf) != "RFB 003.008\n" {
		t.Fatalf("server version %q %v", buf, err)
	}
	nc.Write([]byte("RFB 003.008\n"))
	sec := make([]byte, 2)
	if _, err := io.ReadFull(nc, sec); err != nil || sec[0] != 1 || sec[1] != 1 {
		t.Fatalf("security %v %v", sec, err)
	}
	nc.Write([]byte{1})
	res := make([]byte, 4)
	if _, err := io.ReadFull(nc, res); err != nil || string(res) != "\x00\x00\x00\x00" {
		t.Fatalf("result %v %v", res, err)
	}
	nc.Write([]byte{1})
	si := make([]byte, 28)
	if _, err := io.ReadFull(nc, si); err != nil {
		t.Fatalf("serverinit %v", err)
	}
	return nc
}

func waitSessions(t *testing.T, reg *live.Registry, n int) []core.Session {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if l := reg.List(); len(l) == n {
			return l
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("want %d sessions, have %d", n, len(reg.List()))
	return nil
}

func waitLog(t *testing.T, b *fakeBackend) string {
	t.Helper()
	select {
	case r := <-b.loggedC:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no session log")
		return ""
	}
}

func TestEndToEndEcho(t *testing.T) {
	e := setup(t, Config{})
	e.b.add("tok1", 1, rfbtest.Server(t, "pw"), "pw")
	ws, _, err := e.dial(t, "tok1")
	if err != nil {
		t.Fatal(err)
	}
	nc := handshake(t, ws)
	nc.Write([]byte("hello vm"))
	got := make([]byte, 8)
	if _, err := io.ReadFull(nc, got); err != nil || string(got) != "hello vm" {
		t.Fatalf("echo %q %v", got, err)
	}
	s := waitSessions(t, e.reg, 1)[0]
	if s.ClientIP != "127.0.0.1" || s.LinkID != 1 {
		t.Fatalf("session %+v", s)
	}
	nc.Close()
	if r := waitLog(t, e.b); r != "closed" {
		t.Fatalf("reason %q", r)
	}
	waitSessions(t, e.reg, 0)
}

func TestKillClosesBridge(t *testing.T) {
	e := setup(t, Config{})
	e.b.add("tok1", 1, rfbtest.Server(t, ""), "")
	ws, _, err := e.dial(t, "tok1")
	if err != nil {
		t.Fatal(err)
	}
	nc := handshake(t, ws)
	s := waitSessions(t, e.reg, 1)[0]
	if err := e.reg.Kill(s.ID, "killed"); err != nil {
		t.Fatal(err)
	}
	nc.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := nc.Read(make([]byte, 1)); err == nil {
		t.Fatal("read should fail after kill")
	}
	if r := waitLog(t, e.b); r != "killed" {
		t.Fatalf("reason %q", r)
	}
	waitSessions(t, e.reg, 0)
}

func TestBusyRejected(t *testing.T) {
	e := setup(t, Config{})
	e.b.add("tok1", 1, rfbtest.Server(t, ""), "")
	ws, _, err := e.dial(t, "tok1")
	if err != nil {
		t.Fatal(err)
	}
	nc := handshake(t, ws)
	defer nc.Close()
	_, resp, err := e.dial(t, "tok1")
	if err == nil || resp == nil || resp.StatusCode != 404 {
		t.Fatalf("busy: err=%v resp=%v", err, resp)
	}
}

type snapshot struct {
	status int
	body   string
	header http.Header
}

func (e *env) plain(t *testing.T, path string, hdr map[string]string) snapshot {
	t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
	req.Header.Set("Origin", e.srv.URL)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	h := resp.Header.Clone()
	h.Del("Date")
	return snapshot{resp.StatusCode, string(body), h}
}

func TestGenericNotFoundUniform(t *testing.T) {
	e := setup(t, Config{})
	e.b.add("busy", 1, rfbtest.Server(t, ""), "")
	e.b.add("revoked", 2, "127.0.0.1:1", "")
	l := e.b.links["revoked"]
	l.Revoked = true
	e.b.links["revoked"] = l
	e.b.add("expired", 3, "127.0.0.1:1", "")
	l = e.b.links["expired"]
	l.ExpiresAt = time.Now().Add(-time.Minute)
	e.b.links["expired"] = l

	ws, _, err := e.dial(t, "busy")
	if err != nil {
		t.Fatal(err)
	}
	nc := handshake(t, ws)
	defer nc.Close()

	var snaps []snapshot
	for _, tok := range []string{"busy", "revoked", "expired", "nosuchtoken"} {
		snaps = append(snaps, e.plain(t, "/ws/"+tok, nil))
	}
	for _, s := range snaps {
		if s.status != 404 || s.body != "" {
			t.Fatalf("not generic: %+v", s)
		}
		if !reflect.DeepEqual(s, snaps[0]) {
			t.Fatalf("non uniform: %+v vs %+v", s, snaps[0])
		}
	}
}

func TestRateLimitBan(t *testing.T) {
	e := setup(t, Config{})
	e.b.add("good", 1, rfbtest.Server(t, ""), "")
	for i := 0; i < maxFails; i++ {
		if s := e.plain(t, "/ws/bad"+strconv.Itoa(i), nil); s.status != 404 {
			t.Fatalf("attempt %d: %d", i, s.status)
		}
	}
	if s := e.plain(t, "/ws/good", nil); s.status != 429 {
		t.Fatalf("expected ban, got %d", s.status)
	}
	if _, resp, err := e.dial(t, "good"); err == nil || resp.StatusCode != 429 {
		t.Fatal("banned ip must not connect")
	}
}

var loopback = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}

func TestRealIPHeaderBansSeparately(t *testing.T) {
	e := setup(t, Config{RealIPHeader: "X-Real-IP", TrustedProxies: loopback})
	for i := 0; i < maxFails; i++ {
		e.plain(t, "/ws/bad", map[string]string{"X-Real-IP": "203.0.113.9"})
	}
	if s := e.plain(t, "/ws/bad", map[string]string{"X-Real-IP": "203.0.113.9"}); s.status != 429 {
		t.Fatalf("got %d", s.status)
	}
	if s := e.plain(t, "/ws/bad", map[string]string{"X-Real-IP": "203.0.113.10"}); s.status != 404 {
		t.Fatalf("got %d", s.status)
	}
}

func TestOriginMismatch(t *testing.T) {
	e := setup(t, Config{})
	e.b.add("tok1", 1, rfbtest.Server(t, ""), "")
	for _, o := range []string{"http://evil.example", "", "null", "http://127.0.0.1:1"} {
		hdr := map[string]string{"Origin": o}
		s := e.plain(t, "/ws/tok1", hdr)
		if s.status != 403 {
			t.Fatalf("origin %q got %d", o, s.status)
		}
	}
	if len(e.reg.List()) != 0 {
		t.Fatal("no session expected")
	}

	e2 := setup(t, Config{PublicHost: "play.example.com"})
	e2.b.add("tok1", 1, rfbtest.Server(t, ""), "")
	if s := e2.plain(t, "/ws/tok1", map[string]string{"Origin": "https://evil.example.com"}); s.status != 403 {
		t.Fatalf("got %d", s.status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+e2.srv.URL[4:]+"/ws/tok1", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {"https://play.example.com"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	handshake(t, ws).Close()
}

func TestIdleTimeout(t *testing.T) {
	e := setup(t, Config{IdleTimeout: 300 * time.Millisecond})
	e.b.add("tok1", 1, rfbtest.Server(t, ""), "")
	ws, _, err := e.dial(t, "tok1")
	if err != nil {
		t.Fatal(err)
	}
	nc := handshake(t, ws)
	nc.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := nc.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected close by idle")
	}
	if r := waitLog(t, e.b); r != "idle" {
		t.Fatalf("reason %q", r)
	}
}

func TestMaxConns(t *testing.T) {
	e := setup(t, Config{MaxConns: 1})
	e.b.add("a", 1, rfbtest.Server(t, ""), "")
	e.b.add("b", 2, rfbtest.Server(t, ""), "")
	ws, _, err := e.dial(t, "a")
	if err != nil {
		t.Fatal(err)
	}
	nc := handshake(t, ws)
	defer nc.Close()
	if s := e.plain(t, "/ws/b", nil); s.status != 503 {
		t.Fatalf("got %d", s.status)
	}
}

func TestVMErrorAndBadClient(t *testing.T) {
	e := setup(t, Config{})
	e.b.add("down", 1, "127.0.0.1:1", "")
	ws, _, err := e.dial(t, "down")
	if err != nil {
		t.Fatal(err)
	}
	ws.SetReadLimit(1 << 10)
	if _, _, err := ws.Read(context.Background()); err == nil {
		t.Fatal("expected close")
	}
	if r := waitLog(t, e.b); r != "vm_error" {
		t.Fatalf("reason %q", r)
	}

	e.b.add("ok", 2, rfbtest.Server(t, ""), "")
	ws, _, err = e.dial(t, "ok")
	if err != nil {
		t.Fatal(err)
	}
	nc := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	io.ReadFull(nc, make([]byte, 12))
	nc.Write([]byte("RFB 003.003\n"))
	nc.SetDeadline(time.Now().Add(5 * time.Second))
	io.Copy(io.Discard, nc)
	if r := waitLog(t, e.b); r != "bad_client" {
		t.Fatalf("reason %q", r)
	}
}

func TestStaticRoutes(t *testing.T) {
	e := setup(t, Config{})
	s := e.plain(t, "/s/anything", nil)
	if s.status != 200 {
		t.Fatalf("index status %d", s.status)
	}
	for _, h := range []string{"Content-Security-Policy", "Referrer-Policy", "Cache-Control", "X-Content-Type-Options", "X-Frame-Options"} {
		if s.header.Get(h) == "" {
			t.Fatalf("missing %s", h)
		}
	}
	for _, p := range []string{"/", "/admin", "/assets/", "/assets", "/assets/../index.html", "/s/", "/s/a/b", "/assets/nope.js"} {
		if s := e.plain(t, p, nil); s.status != 404 || s.body != "" {
			t.Fatalf("%s: %+v", p, s)
		}
	}
	req, _ := http.NewRequest("POST", e.srv.URL+"/s/x", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("POST %d", resp.StatusCode)
	}
}

func TestLimiterBoundedMemory(t *testing.T) {
	l := newLimiter()
	for i := 0; i < 3*maxLimiterIPs; i++ {
		l.fail("ip" + strconv.Itoa(i))
	}
	if l.size() > maxLimiterIPs {
		t.Fatalf("size %d", l.size())
	}
	now := time.Now()
	l.now = func() time.Time { return now }
	for i := 0; i < maxFails; i++ {
		l.fail("victim")
	}
	if !l.banned("victim") {
		t.Fatal("expected ban")
	}
	l.now = func() time.Time { return now.Add(banDuration + time.Second) }
	if l.banned("victim") {
		t.Fatal("ban should expire")
	}
}

func TestLimiterKeyGroupsIPv6(t *testing.T) {
	if limiterKey("2001:db8::1") != limiterKey("2001:db8::ffff") {
		t.Fatal("same /64 must share key")
	}
}

func TestContinuousUpdatesEndToEnd(t *testing.T) {
	e := setup(t, Config{ContinuousUpdates: true})
	e.b.add("tok1", 1, rfbtest.Server(t, "pw"), "pw")
	ws, _, err := e.dial(t, "tok1")
	if err != nil {
		t.Fatal(err)
	}
	nc := handshake(t, ws)
	nc.Write([]byte{2, 0, 0, 2, 0, 0, 0, 0, 0xff, 0xff, 0xfe, 0xc7})
	got := make([]byte, 1)
	if _, err := io.ReadFull(nc, got); err != nil || got[0] != 150 {
		t.Fatalf("want EndOfContinuousUpdates, got %v %v", got, err)
	}
	key := []byte{4, 1, 0, 0, 0, 0, 0, 'a'}
	ptr := []byte{5, 1, 0, 10, 0, 20}
	nc.Write(append(append([]byte{3, 1, 0, 0, 0, 0, 0, 1, 0, 1}, key...), ptr...))
	echo := make([]byte, len(key)+len(ptr)+len([]byte{2, 0, 0, 1, 0, 0, 0, 0}))
	if _, err := io.ReadFull(nc, echo); err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte{2, 0, 0, 1, 0, 0, 0, 0}, key...), ptr...)
	if string(echo) != string(want) {
		t.Fatalf("vm saw %v want %v", echo, want)
	}
	nc.Close()
	waitLog(t, e.b)
}

func TestContinuousUpdatesBadClientMessage(t *testing.T) {
	e := setup(t, Config{ContinuousUpdates: true})
	e.b.add("tok1", 1, rfbtest.Server(t, "pw"), "pw")
	ws, _, err := e.dial(t, "tok1")
	if err != nil {
		t.Fatal(err)
	}
	nc := handshake(t, ws)
	nc.Write([]byte{99})
	io.Copy(io.Discard, nc)
	if r := waitLog(t, e.b); r != reasonBadRFB {
		t.Fatalf("reason %q", r)
	}
}

func TestBrowserClientInitIsAlwaysShared(t *testing.T) {
	for _, continuous := range []bool{true, false} {
		t.Run(fmt.Sprintf("continuous=%v", continuous), func(t *testing.T) {
			e := setup(t, Config{ContinuousUpdates: continuous})
			vm := rfbtest.NewFB(t, "", 64, 48)
			e.b.add("tok1", 1, vm.Addr(), "")
			ws, _, err := e.dial(t, "tok1")
			if err != nil {
				t.Fatal(err)
			}
			nc := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
			defer nc.Close()
			nc.SetDeadline(time.Now().Add(5 * time.Second))
			io.ReadFull(nc, make([]byte, 12))
			nc.Write([]byte("RFB 003.008\n"))
			io.ReadFull(nc, make([]byte, 2))
			nc.Write([]byte{1})
			io.ReadFull(nc, make([]byte, 4))
			nc.Write([]byte{0}) // ClientInit with shared = 0
			si := make([]byte, 24)
			if _, err := io.ReadFull(nc, si); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for vm.Clients() == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if !vm.Shared() {
				t.Fatal("the VM saw shared = 0")
			}
		})
	}
}
