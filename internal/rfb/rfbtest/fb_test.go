package rfbtest

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func dialFB(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	ver := make([]byte, 12)
	io.ReadFull(c, ver)
	c.Write(ver)
	sec := make([]byte, 2)
	io.ReadFull(c, sec)
	c.Write([]byte{1})
	res := make([]byte, 4)
	io.ReadFull(c, res)
	c.Write([]byte{1}) // shared
	si := make([]byte, 24)
	if _, err := io.ReadFull(c, si); err != nil {
		t.Fatal(err)
	}
	name := make([]byte, binary.BigEndian.Uint32(si[20:]))
	io.ReadFull(c, name)
	return c
}

func readUpdate(t *testing.T, c net.Conn) (rects int, firstEnc int32) {
	t.Helper()
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c, hdr); err != nil || hdr[0] != 0 {
		t.Fatalf("update header %v %v", hdr, err)
	}
	n := int(binary.BigEndian.Uint16(hdr[2:]))
	for i := 0; i < n; i++ {
		r := make([]byte, 12)
		io.ReadFull(c, r)
		w, h := int(binary.BigEndian.Uint16(r[4:])), int(binary.BigEndian.Uint16(r[6:]))
		enc := int32(binary.BigEndian.Uint32(r[8:]))
		if i == 0 {
			firstEnc = enc
		}
		if enc == 0 {
			io.ReadFull(c, make([]byte, w*h*4))
		}
	}
	return n, firstEnc
}

func TestFBServesUpdates(t *testing.T) {
	fb := NewFB(t, "", 8, 4)
	c := dialFB(t, fb.Addr())
	c.Write([]byte{2, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 1}) // SetEncodings [Raw, CopyRect]
	c.Write([]byte{3, 0, 0, 0, 0, 0, 0, 8, 0, 4})       // full request
	if n, enc := readUpdate(t, c); n != 1 || enc != 0 {
		t.Fatalf("first update %d %d", n, enc)
	}
	if !fb.Shared() {
		t.Fatal("shared flag not recorded")
	}
	if e := fb.Encodings(); len(e) != 2 || e[0] != 0 || e[1] != 1 {
		t.Fatalf("encodings %v", e)
	}

	c.Write([]byte{3, 1, 0, 0, 0, 0, 0, 8, 0, 4}) // incremental, nothing changed: must wait
	c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("incremental request answered without a change")
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	fb.Fill(1, 2, 3)
	readUpdate(t, c)

	c.Write([]byte{3, 1, 0, 0, 0, 0, 0, 8, 0, 4})
	fb.Resize(16, 6)
	if n, enc := readUpdate(t, c); n != 2 || enc != -223 {
		t.Fatalf("resize update %d %d", n, enc)
	}
}

func TestFBCloseDropsClients(t *testing.T) {
	fb := NewFB(t, "", 8, 4)
	c := dialFB(t, fb.Addr())
	c.Write([]byte{3, 0, 0, 0, 0, 0, 0, 8, 0, 4})
	readUpdate(t, c)
	if fb.Clients() != 1 {
		t.Fatalf("clients %d", fb.Clients())
	}
	fb.Close()
	fb.Close()
	deadline := time.Now().Add(time.Second)
	for fb.Clients() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("clients still %d after Close", fb.Clients())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if c2, err := net.DialTimeout("tcp", fb.Addr(), 500*time.Millisecond); err == nil {
		c2.Close()
		t.Fatal("dial succeeded after Close")
	}
}
