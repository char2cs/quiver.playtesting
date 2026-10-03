package rfb_test

import (
	"context"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"quiver-playtesting/internal/rfb"
	"quiver-playtesting/internal/rfb/rfbtest"
)

func TestDialEndToEnd(t *testing.T) {
	addr := rfbtest.Server(t, "secret")
	c, err := rfb.Dial(context.Background(), addr, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte{1})
	si := make([]byte, 24+4)
	if _, err := io.ReadFull(c, si); err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("ping"))
	got := make([]byte, 4)
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "ping" {
		t.Fatalf("echo %q %v", got, err)
	}
}

func TestDialBadPassword(t *testing.T) {
	addr := rfbtest.Server(t, "secret")
	if c, err := rfb.Dial(context.Background(), addr, "wrong"); err == nil {
		c.Close()
		t.Fatal("expected failure")
	}
}

func TestDialNoneAndMismatch(t *testing.T) {
	addr := rfbtest.Server(t, "")
	c, err := rfb.Dial(context.Background(), addr, "")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if c, err := rfb.Dial(context.Background(), addr, "x"); err == nil {
		c.Close()
		t.Fatal("password set but server offers only None must fail")
	}
}

func TestDialCanceled(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(2 * time.Second)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := rfb.Dial(ctx, ln.Addr().String(), "x"); err == nil {
		t.Fatal("expected error")
	}
	if time.Since(start) > time.Second {
		t.Fatal("ctx not respected")
	}
}

func TestDialBadServerVersion(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Write([]byte("HTTP/1.1 200\n"))
			c.Close()
		}
	}()
	if _, err := rfb.Dial(context.Background(), ln.Addr().String(), ""); err == nil {
		t.Fatal("expected error")
	}
}

func acceptWith(t *testing.T, client func(c net.Conn)) error {
	a, b := net.Pipe()
	defer a.Close()
	go func() { defer b.Close(); client(b) }()
	return rfb.Accept(a)
}

func TestAcceptGood(t *testing.T) {
	err := acceptWith(t, func(c net.Conn) {
		buf := make([]byte, 12)
		io.ReadFull(c, buf)
		c.Write([]byte("RFB 003.008\n"))
		two := make([]byte, 2)
		io.ReadFull(c, two)
		c.Write([]byte{1})
		io.ReadFull(c, make([]byte, 4))
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAcceptRejects(t *testing.T) {
	for _, v := range []string{"RFB 003.003\n", "RFB 003.007\n", "RFB 003.889\n", "GET / HTTP/1.", "RFB 003.008X"} {
		err := acceptWith(t, func(c net.Conn) {
			io.ReadFull(c, make([]byte, 12))
			c.Write([]byte(v))
			io.Copy(io.Discard, c)
		})
		if err == nil {
			t.Fatalf("accepted %q", v)
		}
	}
	err := acceptWith(t, func(c net.Conn) {
		io.ReadFull(c, make([]byte, 12))
		c.Write([]byte("RFB 003.008\n"))
		io.ReadFull(c, make([]byte, 2))
		c.Write([]byte{2})
		io.Copy(io.Discard, c)
	})
	if err == nil {
		t.Fatal("accepted non-None security")
	}
}

func TestAcceptEOF(t *testing.T) {
	if err := acceptWith(t, func(c net.Conn) {}); err == nil {
		t.Fatal("expected error")
	}
}

func TestCheckIP(t *testing.T) {
	bad := []string{"169.254.169.254", "0.0.0.0", "::", "fe80::1", "::ffff:169.254.169.254", "224.0.0.1", "ff02::1", "255.255.255.255", "fd00:ec2::254"}
	for _, s := range bad {
		if rfb.CheckIP(netip.MustParseAddr(s)) == nil {
			t.Errorf("%s allowed", s)
		}
	}
	for _, s := range []string{"192.168.1.10", "10.0.0.5", "172.16.3.3", "127.0.0.1", "8.8.8.8", "::1"} {
		if err := rfb.CheckIP(netip.MustParseAddr(s)); err != nil {
			t.Errorf("%s rejected", s)
		}
	}
}

func TestDialRefusesBannedAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, a := range []string{"169.254.169.254:80", "0.0.0.0:5900", "[fe80::1]:5900"} {
		start := time.Now()
		if c, err := rfb.Dial(ctx, a, ""); err == nil {
			c.Close()
			t.Fatalf("dialed %s", a)
		}
		if time.Since(start) > time.Second {
			t.Fatalf("%s: should fail before connecting", a)
		}
	}
}

func fakeServer(t *testing.T, script func(c net.Conn)) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err == nil {
			defer c.Close()
			script(c)
		}
	}()
	return ln.Addr().String()
}

func TestDialVersion37(t *testing.T) {
	var got [12]byte
	addr := fakeServer(t, func(c net.Conn) {
		c.Write([]byte("RFB 003.007\n"))
		io.ReadFull(c, got[:])
		c.Write([]byte{1, 1})
		io.ReadFull(c, make([]byte, 1))
	})
	c, err := rfb.Dial(context.Background(), addr, "")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if string(got[:]) != "RFB 003.007\n" {
		t.Fatalf("client answered %q", got[:])
	}
}

func TestDialMalformedServers(t *testing.T) {
	scripts := map[string]func(c net.Conn){
		"refused":      func(c net.Conn) { c.Write([]byte("RFB 003.008\n\x00")) },
		"short types":  func(c net.Conn) { c.Write([]byte("RFB 003.008\n\x05\x02")) },
		"unknown type": func(c net.Conn) { c.Write([]byte("RFB 003.008\n\x01\x63")) },
		"short chal": func(c net.Conn) {
			c.Write([]byte("RFB 003.008\n\x01\x02abc"))
		},
		"v3.3": func(c net.Conn) { c.Write([]byte("RFB 003.003\n")) },
	}
	for name, sc := range scripts {
		addr := fakeServer(t, sc)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if c, err := rfb.Dial(ctx, addr, "pw"); err == nil {
			c.Close()
			t.Fatalf("%s: accepted", name)
		}
		cancel()
	}
}
