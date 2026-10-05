package recorder

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"quiver-playtesting/internal/rfb"
	"quiver-playtesting/internal/rfb/rfbtest"
)

func startRun(t *testing.T, addr string) (fbc chan *Framebuffer, errc chan error, cancel context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	conn, err := rfb.Dial(ctx, addr, "")
	if err != nil {
		t.Fatal(err)
	}
	fbc = make(chan *Framebuffer, 1)
	errc = make(chan error, 1)
	go func() { errc <- Run(ctx, conn, func(f *Framebuffer) { fbc <- f }) }()
	return fbc, errc, cancel
}

func waitPixel(t *testing.T, fb *Framebuffer, want [3]byte) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap, _, _ := fb.Snapshot(nil)
		if snap[0] == want[0] && snap[1] == want[1] && snap[2] == want[2] {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	snap, _, _ := fb.Snapshot(nil)
	t.Fatalf("pixel %v, want %v", snap[:3], want)
}

func TestRunAppliesUpdatesAndResizes(t *testing.T) {
	srv := rfbtest.NewFB(t, "", 8, 4)
	srv.Fill(30, 20, 10) // r, g, b
	fbc, errc, cancel := startRun(t, srv.Addr())
	fb := <-fbc
	if w, h := fb.Size(); w != 8 || h != 4 {
		t.Fatalf("size %d %d", w, h)
	}
	waitPixel(t, fb, [3]byte{10, 20, 30}) // BGR order in memory

	srv.Fill(5, 6, 7)
	waitPixel(t, fb, [3]byte{7, 6, 5})

	srv.Resize(16, 6)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if w, h := fb.Size(); w == 16 && h == 6 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if w, h := fb.Size(); w != 16 || h != 6 {
		t.Fatalf("size after resize %d %d", w, h)
	}

	if !srv.Shared() {
		t.Fatal("capture must connect as a shared client")
	}
	if e := srv.Encodings(); len(e) != 3 || e[0] != 0 || e[1] != 1 || e[2] != -223 {
		t.Fatalf("encodings %v", e)
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("clean stop returned %v", err)
	}
}

func TestRunServerDisconnect(t *testing.T) {
	srv := rfbtest.NewFB(t, "", 8, 4)
	fbc, errc, _ := startRun(t, srv.Addr())
	<-fbc
	srv.Close()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("disconnect must be an error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not notice the disconnect")
	}
}

// scripted plays a server over a pipe: it accepts the client's setup, then writes script.
func scripted(t *testing.T, w, h int, script func(c net.Conn)) error {
	t.Helper()
	_, err := scriptedFB(t, w, h, script)
	return err
}

func scriptedFB(t *testing.T, w, h int, script func(c net.Conn)) (*Framebuffer, error) {
	t.Helper()
	cli, srv := net.Pipe()
	go func() {
		defer srv.Close()
		var ci [1]byte
		io.ReadFull(srv, ci[:])
		si := binary.BigEndian.AppendUint16(nil, uint16(w))
		si = binary.BigEndian.AppendUint16(si, uint16(h))
		si = append(si, make([]byte, 16)...)
		si = binary.BigEndian.AppendUint32(si, 0)
		srv.Write(si)
		io.CopyN(io.Discard, srv, 20+16+10) // SetPixelFormat, SetEncodings(3), first request
		go io.Copy(io.Discard, srv)         // pipes are synchronous, so the client's update requests must be drained
		script(srv)
	}()
	fbc := make(chan *Framebuffer, 1)
	errc := make(chan error, 1)
	go func() { errc <- Run(context.Background(), cli, func(f *Framebuffer) { fbc <- f }) }()
	select {
	case err := <-errc:
		return <-fbc, err
	case <-time.After(3 * time.Second):
		t.Fatal("Run hung on hostile input")
		return nil, nil
	}
}

func update(rects ...[]byte) []byte {
	out := []byte{0, 0}
	out = binary.BigEndian.AppendUint16(out, uint16(len(rects)))
	for _, r := range rects {
		out = append(out, r...)
	}
	return out
}

func rect(x, y, w, h int, enc int32, data []byte) []byte {
	b := binary.BigEndian.AppendUint16(nil, uint16(x))
	b = binary.BigEndian.AppendUint16(b, uint16(y))
	b = binary.BigEndian.AppendUint16(b, uint16(w))
	b = binary.BigEndian.AppendUint16(b, uint16(h))
	b = binary.BigEndian.AppendUint32(b, uint32(enc))
	return append(b, data...)
}

func TestRunRejectsHostileServers(t *testing.T) {
	cases := map[string]func(c net.Conn){
		"unknown message":      func(c net.Conn) { c.Write([]byte{99}) },
		"colour map":           func(c net.Conn) { c.Write([]byte{1, 0, 0, 0, 0, 0}) },
		"unsupported encoding": func(c net.Conn) { c.Write(update(rect(0, 0, 1, 1, 7, nil))) },
		"rect outside screen":  func(c net.Conn) { c.Write(update(rect(7, 3, 4, 4, 0, make([]byte, 64)))) },
		"giant desktop size":   func(c net.Conn) { c.Write(update(rect(0, 0, 65535, 65535, -223, nil))) },
		"giant cut text":       func(c net.Conn) { c.Write([]byte{3, 0, 0, 0, 0xff, 0xff, 0xff, 0xff}) },
		"copyrect outside":     func(c net.Conn) { c.Write(update(rect(0, 0, 4, 4, 1, []byte{0, 99, 0, 99}))) },
		"two desktop sizes": func(c net.Conn) {
			c.Write(update(rect(0, 0, 9, 4, -223, nil), rect(0, 0, 10, 4, -223, nil)))
		},
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			err := scripted(t, 8, 4, script)
			if !errors.Is(err, ErrProtocol) {
				t.Errorf("got %v, want ErrProtocol", err)
			}
		})
	}
}

func TestRunManyTinyRects(t *testing.T) {
	err := scripted(t, 8, 4, func(c net.Conn) {
		rects := make([][]byte, 65535)
		for i := range rects {
			rects[i] = rect(0, 0, 1, 1, 0, []byte{1, 2, 3, 0})
		}
		c.Write(update(rects...))
		c.Write([]byte{99})
	})
	if !errors.Is(err, ErrProtocol) || !strings.Contains(err.Error(), "message type 99") {
		t.Fatalf("got %v, want the update to be accepted and then message type 99 rejected", err)
	}
}

func TestRunSameSizeDesktopSizeKeepsPixels(t *testing.T) {
	fb, err := scriptedFB(t, 8, 4, func(c net.Conn) {
		c.Write(update(rect(0, 0, 1, 1, 0, []byte{1, 2, 3, 0})))
		c.Write(update(rect(0, 0, 8, 4, -223, nil)))
		c.Write([]byte{99})
	})
	if !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
	if snap, _, _ := fb.Snapshot(nil); snap[0] != 1 || snap[1] != 2 || snap[2] != 3 {
		t.Fatalf("pixel %v, want [1 2 3]", snap[:3])
	}
}

func TestRunAcceptsBellAndCutText(t *testing.T) {
	err := scripted(t, 8, 4, func(c net.Conn) {
		c.Write([]byte{2})
		c.Write([]byte{3, 0, 0, 0, 0, 0, 0, 3, 'a', 'b', 'c'})
		c.Write([]byte{99}) // ends the run so the test can observe it
	})
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("got %v", err)
	}
}

func copyRects(n, w, h int) [][]byte {
	rects := make([][]byte, n)
	for i := range rects {
		rects[i] = rect(0, 0, w, h, 1, []byte{0, 0, 0, 0})
	}
	return rects
}

func TestRunCapsCopyRectArea(t *testing.T) {
	// A 40x20 screen is 800 px so the cap is 3200 px: 8 rects of 400 px fit, 9 do not.
	err := scripted(t, 40, 20, func(c net.Conn) { c.Write(update(copyRects(9, 20, 20)...)) })
	if !errors.Is(err, ErrProtocol) || !strings.Contains(err.Error(), "copy rectangles exceed") {
		t.Fatalf("got %v, want the copy cap to trip", err)
	}
}

func TestRunAcceptsCopyRectsWithinCap(t *testing.T) {
	err := scripted(t, 40, 20, func(c net.Conn) {
		c.Write(update(copyRects(8, 20, 20)...))
		c.Write([]byte{99})
	})
	if !errors.Is(err, ErrProtocol) || !strings.Contains(err.Error(), "message type 99") {
		t.Fatalf("got %v, want the update accepted", err)
	}
}
