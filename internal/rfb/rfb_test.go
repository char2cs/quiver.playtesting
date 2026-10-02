package rfb_test

import (
	"context"
	"io"
	"net"
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
