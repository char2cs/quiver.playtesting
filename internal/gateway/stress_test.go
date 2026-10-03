package gateway

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"quiver-playtesting/internal/core"
	"quiver-playtesting/internal/live"
	"quiver-playtesting/internal/rfb/rfbtest"
)

type countBackend struct {
	*fakeBackend
	logged atomic.Int64
}

func (c *countBackend) LogSession(context.Context, core.Session, time.Time, string) error {
	c.logged.Add(1)
	return nil
}

func tryHandshake(nc net.Conn) error {
	nc.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 12)
	if _, err := io.ReadFull(nc, buf); err != nil {
		return err
	}
	nc.Write([]byte("RFB 003.008\n"))
	if _, err := io.ReadFull(nc, make([]byte, 2)); err != nil {
		return err
	}
	nc.Write([]byte{1})
	if _, err := io.ReadFull(nc, make([]byte, 4)); err != nil {
		return err
	}
	nc.Write([]byte{1})
	_, err := io.ReadFull(nc, make([]byte, 28))
	return err
}

func settledGoroutines(base int) int {
	var n int
	for i := 0; i < 100; i++ {
		http.DefaultClient.CloseIdleConnections()
		n = runtime.NumGoroutine()
		if n <= base {
			return n
		}
		time.Sleep(100 * time.Millisecond)
	}
	return n
}

func TestStressNoLeaks(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	b := &countBackend{fakeBackend: newBackend()}
	const links = 24
	addr := rfbtest.Server(t, "")
	for i := 1; i <= links; i++ {
		b.add("good"+strconv.Itoa(i), int64(i), addr, "")
	}
	reg := live.New()
	srv := NewServer(Config{
		MaxConns: 16, IdleTimeout: time.Minute, TrustedProxies: loopback, RealIPHeader: "X-Real-IP",
		MaxOpenConns: 5000, PerPeerConns: 5000,
	}, b, reg)

	var vmMu sync.Mutex
	var vmConns []net.Conn
	realDial := srv.h.dialVM
	srv.h.dialVM = func(ctx context.Context, a, pw string) (net.Conn, error) {
		c, err := realDial(ctx, a, pw)
		if err == nil {
			vmMu.Lock()
			vmConns = append(vmConns, c)
			vmMu.Unlock()
		}
		return c, err
	}
	ln := listen(t)
	go srv.Serve(ln)
	host := ln.Addr().String()

	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	base := runtime.NumGoroutine()

	var wg sync.WaitGroup
	hit := func(f func(i int)) {
		for i := 0; i < 300; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); f(i) }()
		}
	}

	// bad tokens from many identities, plus malformed and oversized requests
	hit(func(i int) {
		req, _ := http.NewRequest("GET", "http://"+host+"/ws/bad"+strconv.Itoa(i), nil)
		req.Header.Set("Origin", "http://"+host)
		req.Header.Set("X-Real-IP", fmt.Sprintf("10.%d.%d.1", i/200, i%200))
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	})
	// half-open sockets: partial request lines and headers, then abandoned
	hit(func(i int) {
		c, err := net.Dial("tcp", host)
		if err != nil {
			return
		}
		switch i % 3 {
		case 0:
			c.Write([]byte("GET /ws/abc HT"))
		case 1:
			c.Write([]byte("GET /ws/abc HTTP/1.1\r\nHost: x\r\nX-A: "))
		case 2:
			c.Write([]byte("GET /ws/abc HTTP/1.1\r\nHost: x\r\nX-Big: " + string(make([]byte, 40<<10)) + "\r\n\r\n"))
		}
		time.Sleep(50 * time.Millisecond)
		c.Close()
	})
	wg.Wait()

	// session churn: complete, abort mid handshake, client vanish, kill, vm death
	for round := 0; round < 3; round++ {
		for i := 1; i <= links; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				ws, _, err := websocket.Dial(ctx, "ws://"+host+"/ws/good"+strconv.Itoa(i), &websocket.DialOptions{
					HTTPHeader: http.Header{"Origin": {"http://" + host}},
				})
				if err != nil {
					return
				}
				switch (i + round) % 4 {
				case 0:
					ws.CloseNow()
				case 1:
					nc := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
					if tryHandshake(nc) == nil {
						nc.Write([]byte("hello"))
					}
					nc.Close()
				case 2:
					nc := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
					if tryHandshake(nc) == nil {
						time.Sleep(30 * time.Millisecond)
						reg.KillByLink(int64(i), "killed")
					}
					io.Copy(io.Discard, nc)
					nc.Close()
				case 3:
					nc := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
					if tryHandshake(nc) == nil {
						vmMu.Lock()
						for _, c := range vmConns {
							c.Close()
						}
						vmMu.Unlock()
					}
					io.Copy(io.Discard, nc)
					nc.Close()
				}
			}()
		}
		wg.Wait()
	}

	reg.KillAll("test")
	deadline := time.Now().Add(15 * time.Second)
	for len(reg.List()) > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := len(reg.List()); n != 0 {
		t.Fatalf("%d sessions leaked", n)
	}
	http.DefaultClient.CloseIdleConnections()
	openConns := func() int {
		srv.ll.mu.Lock()
		defer srv.ll.mu.Unlock()
		return srv.ll.total
	}
	for i := 0; i < 100 && (len(srv.h.sem) != 0 || openConns() != 0); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if n := len(srv.h.sem); n != 0 {
		t.Fatalf("MaxConns semaphore leaked %d slots", n)
	}
	srv.ll.mu.Lock()
	open, peers := srv.ll.total, len(srv.ll.peers)
	srv.ll.mu.Unlock()
	if open != 0 || peers != 0 {
		t.Fatalf("listener accounting leaked: total=%d peers=%d", open, peers)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if n := settledGoroutines(base + 3); n > base+3 {
		buf := make([]byte, 1<<20)
		t.Fatalf("goroutines %d, baseline %d\n%s", n, base, buf[:runtime.Stack(buf, true)])
	}
	t.Logf("sessions logged: %d", b.logged.Load())
}
