// Package rfbtest provides a minimal RFB 3.8 VNC server for tests.
package rfbtest

import (
	"bytes"
	"crypto/des"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"testing"
)

// Server starts a fake VNC server. After auth it reads ClientInit, sends a
// ServerInit and echoes every byte. An empty password offers security type None.
func Server(t testing.TB, password string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(c, password)
		}
	}()
	return ln.Addr().String()
}

func serve(c net.Conn, password string) {
	defer c.Close()
	if _, ok := handshake(c, password, 640, 480); !ok {
		return
	}
	io.Copy(c, c)
}

// handshake runs version, security and init, ending after ServerInit. It returns the ClientInit shared flag.
func handshake(c net.Conn, password string, w, h int) (shared, ok bool) {
	if _, err := c.Write([]byte("RFB 003.008\n")); err != nil {
		return false, false
	}
	var ver [12]byte
	if _, err := io.ReadFull(c, ver[:]); err != nil || string(ver[:]) != "RFB 003.008\n" {
		return false, false
	}
	var sel [1]byte
	if password == "" {
		c.Write([]byte{1, 1})
		if _, err := io.ReadFull(c, sel[:]); err != nil || sel[0] != 1 {
			return false, false
		}
	} else {
		c.Write([]byte{1, 2})
		if _, err := io.ReadFull(c, sel[:]); err != nil || sel[0] != 2 {
			return false, false
		}
		var ch [16]byte
		rand.Read(ch[:])
		c.Write(ch[:])
		var resp [16]byte
		if _, err := io.ReadFull(c, resp[:]); err != nil {
			return false, false
		}
		if !bytes.Equal(resp[:], encrypt(password, ch)) {
			msg := "bad password"
			out := binary.BigEndian.AppendUint32(nil, 1)
			out = binary.BigEndian.AppendUint32(out, uint32(len(msg)))
			c.Write(append(out, msg...))
			return false, false
		}
	}
	c.Write([]byte{0, 0, 0, 0})
	var ci [1]byte
	if _, err := io.ReadFull(c, ci[:]); err != nil {
		return false, false
	}
	name := "fake"
	si := binary.BigEndian.AppendUint16(nil, uint16(w))
	si = binary.BigEndian.AppendUint16(si, uint16(h))
	si = append(si, make([]byte, 16)...)
	si = binary.BigEndian.AppendUint32(si, uint32(len(name)))
	c.Write(append(si, name...))
	return ci[0] != 0, true
}

func encrypt(password string, ch [16]byte) []byte {
	var key [8]byte
	copy(key[:], password)
	for i, b := range key {
		var r byte
		for j := 0; j < 8; j++ {
			r |= (b >> j & 1) << (7 - j)
		}
		key[i] = r
	}
	blk, _ := des.NewCipher(key[:])
	out := make([]byte, 16)
	blk.Encrypt(out[:8], ch[:8])
	blk.Encrypt(out[8:], ch[8:])
	return out
}
